package main

import (
	"encoding/binary"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// Watcher delivers (dir, name) change notifications; the server reconciles
// them against the filesystem state (lstat), so merged/duplicate events are harmless.
type Watcher interface {
	Run(s *Server)
	Kind() string
}

// ---------------- fanotify (root) ----------------

type fanWatcher struct {
	fd      int
	mountFd map[[2]int32]int
}

func newFanotify(mounts []Mount, meta bool) (*fanWatcher, error) {
	fd, err := unix.FanotifyInit(unix.FAN_CLASS_NOTIF|unix.FAN_CLOEXEC|unix.FAN_REPORT_DFID_NAME|unix.FAN_UNLIMITED_QUEUE,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_LARGEFILE)
	if err != nil {
		return nil, err
	}
	w := &fanWatcher{fd: fd, mountFd: map[[2]int32]int{}}
	mask := uint64(unix.FAN_CREATE | unix.FAN_DELETE | unix.FAN_MOVED_FROM | unix.FAN_MOVED_TO | unix.FAN_ONDIR)
	if meta {
		mask |= unix.FAN_CLOSE_WRITE | unix.FAN_ATTRIB
	}
	marked := 0
	for _, m := range mounts {
		var st unix.Statfs_t
		if unix.Statfs(m.Point, &st) != nil {
			continue
		}
		key := [2]int32{st.Fsid.Val[0], st.Fsid.Val[1]}
		if _, ok := w.mountFd[key]; ok {
			continue
		}
		if err := unix.FanotifyMark(fd, unix.FAN_MARK_ADD|unix.FAN_MARK_FILESYSTEM, mask, unix.AT_FDCWD, m.Point); err != nil {
			log.Printf("fanotify: cannot mark %s (%s): %v", m.Point, m.FS, err)
			continue
		}
		mfd, err := unix.Open(m.Point, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			continue
		}
		w.mountFd[key] = mfd
		marked++
		log.Printf("fanotify: watching filesystem %s (%s)", m.Point, m.FS)
	}
	if marked == 0 {
		unix.Close(fd)
		return nil, os.ErrNotExist
	}
	return w, nil
}

func (w *fanWatcher) Kind() string { return "fanotify" }

func (w *fanWatcher) Run(s *Server) {
	buf := make([]byte, 256<<10)
	for {
		n, err := unix.Read(w.fd, buf)
		if err != nil {
			if err == unix.EINTR || err == unix.EAGAIN {
				continue
			}
			log.Printf("fanotify read: %v", err)
			return
		}
		var batch [][2]string
		for off := 0; off+24 <= n; {
			evLen := int(binary.LittleEndian.Uint32(buf[off:]))
			metaLen := int(binary.LittleEndian.Uint16(buf[off+6:]))
			mask := binary.LittleEndian.Uint64(buf[off+8:])
			if evLen < 24 || off+evLen > n {
				break
			}
			if mask&unix.FAN_Q_OVERFLOW != 0 {
				go s.Rescan("fanotify queue overflow")
			}
			for io := off + metaLen; io+4 <= off+evLen; {
				itype := buf[io]
				ilen := int(binary.LittleEndian.Uint16(buf[io+2:]))
				if ilen <= 0 {
					break
				}
				if itype == unix.FAN_EVENT_INFO_TYPE_DFID_NAME && ilen >= 4+8+8 {
					var fsid [2]int32
					fsid[0] = int32(binary.LittleEndian.Uint32(buf[io+4:]))
					fsid[1] = int32(binary.LittleEndian.Uint32(buf[io+8:]))
					hb := int(binary.LittleEndian.Uint32(buf[io+12:]))
					ht := int32(binary.LittleEndian.Uint32(buf[io+16:]))
					hs := io + 20
					if hs+hb <= io+ilen {
						handle := buf[hs : hs+hb]
						nm := buf[hs+hb : io+ilen]
						if k := indexByte0(nm); k >= 0 {
							nm = nm[:k]
						}
						if dir, ok := w.resolve(fsid, ht, handle); ok && len(nm) > 0 {
							batch = append(batch, [2]string{dir, string(nm)})
						}
					}
				}
				io += ilen
			}
			off += evLen
		}
		if len(batch) > 0 {
			s.Apply(batch)
		}
	}
}

func indexByte0(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}

func (w *fanWatcher) resolve(fsid [2]int32, ht int32, h []byte) (string, bool) {
	mfd, ok := w.mountFd[fsid]
	if !ok {
		return "", false
	}
	fd, err := unix.OpenByHandleAt(mfd, unix.NewFileHandle(ht, append([]byte(nil), h...)), unix.O_PATH|unix.O_CLOEXEC)
	if err != nil {
		return "", false
	}
	defer unix.Close(fd)
	p, err := os.Readlink("/proc/self/fd/" + itoa(fd))
	if err != nil || strings.HasSuffix(p, " (deleted)") {
		return "", false
	}
	return p, true
}

func itoa(i int) string {
	var b [20]byte
	k := len(b)
	for {
		k--
		b[k] = byte('0' + i%10)
		i /= 10
		if i == 0 {
			break
		}
	}
	return string(b[k:])
}

// ---------------- inotify (non-root fallback) ----------------

type inoWatcher struct {
	fd    int
	mu    sync.Mutex
	wd2p  map[int32]string
	p2wd  map[string]int32
	full  bool
	sk    *Skipper
	mask  uint32
}

const inoMask = unix.IN_CREATE | unix.IN_DELETE | unix.IN_MOVED_FROM | unix.IN_MOVED_TO | unix.IN_ONLYDIR | unix.IN_DONT_FOLLOW | unix.IN_EXCL_UNLINK

func newInotify(sk *Skipper, meta bool) (*inoWatcher, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	if err != nil {
		return nil, err
	}
	m := uint32(inoMask)
	if meta {
		m |= unix.IN_CLOSE_WRITE | unix.IN_ATTRIB
	}
	return &inoWatcher{fd: fd, wd2p: map[int32]string{}, p2wd: map[string]int32{}, sk: sk, mask: m}, nil
}

func (w *inoWatcher) Kind() string { return "inotify" }

func (w *inoWatcher) add(p string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.full {
		return
	}
	if _, ok := w.p2wd[p]; ok {
		return
	}
	wd, err := unix.InotifyAddWatch(w.fd, p, w.mask)
	if err != nil {
		if err == unix.ENOSPC {
			w.full = true
			log.Printf("inotify: watch limit reached at %d dirs (raise fs.inotify.max_user_watches or run as root for fanotify)", len(w.p2wd))
		}
		return
	}
	if old, ok := w.wd2p[int32(wd)]; ok { // same inode re-added under new path
		delete(w.p2wd, old)
	}
	w.wd2p[int32(wd)] = p
	w.p2wd[p] = int32(wd)
}

func (w *inoWatcher) addTree(root string) {
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && w.sk.Skip(p) {
				return filepath.SkipDir
			}
			w.add(p)
		}
		return nil
	})
}

func (w *inoWatcher) dropTree(root string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for p, wd := range w.p2wd {
		if p == root || strings.HasPrefix(p, root+"/") {
			unix.InotifyRmWatch(w.fd, uint32(wd))
			delete(w.p2wd, p)
			delete(w.wd2p, wd)
		}
	}
}

func (w *inoWatcher) Seed(s *Server) {
	s.mu.RLock()
	ix := s.ix
	var dirs []string
	for i := 0; i < ix.Len(); i++ {
		if ix.fl(uint32(i))&flagDir != 0 && ix.fl(uint32(i))&flagDead == 0 {
			dirs = append(dirs, ix.Path(uint32(i)))
		}
	}
	s.mu.RUnlock()
	for _, d := range dirs {
		if !w.sk.Excluded(d) || d == ix.RootPath() {
			w.add(d)
		}
	}
	log.Printf("inotify: watching %d directories", len(w.p2wd))
}

func (w *inoWatcher) Run(s *Server) {
	go w.Seed(s)
	buf := make([]byte, 256<<10)
	for {
		n, err := unix.Read(w.fd, buf)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			log.Printf("inotify read: %v", err)
			return
		}
		var batch [][2]string
		var newDirs, goneDirs []string
		for off := 0; off+16 <= n; {
			wd := int32(binary.LittleEndian.Uint32(buf[off:]))
			mask := binary.LittleEndian.Uint32(buf[off+4:])
			l := int(binary.LittleEndian.Uint32(buf[off+12:]))
			nm := buf[off+16 : off+16+l]
			if k := indexByte0(nm); k >= 0 {
				nm = nm[:k]
			}
			off += 16 + l
			if mask&unix.IN_Q_OVERFLOW != 0 {
				go s.Rescan("inotify queue overflow")
				continue
			}
			if mask&unix.IN_IGNORED != 0 {
				w.mu.Lock()
				if p, ok := w.wd2p[wd]; ok {
					delete(w.p2wd, p)
					delete(w.wd2p, wd)
				}
				w.mu.Unlock()
				continue
			}
			w.mu.Lock()
			dir, ok := w.wd2p[wd]
			w.mu.Unlock()
			if !ok || len(nm) == 0 {
				continue
			}
			batch = append(batch, [2]string{dir, string(nm)})
			if mask&unix.IN_ISDIR != 0 && mask&(unix.IN_CREATE|unix.IN_MOVED_TO|unix.IN_DELETE|unix.IN_MOVED_FROM) != 0 {
				full := joinPath(dir, string(nm))
				if mask&(unix.IN_CREATE|unix.IN_MOVED_TO) != 0 {
					newDirs = append(newDirs, full)
				} else {
					goneDirs = append(goneDirs, full)
				}
			}
		}
		for _, d := range goneDirs {
			w.dropTree(d)
		}
		for _, d := range newDirs {
			w.addTree(d)
		}
		if len(batch) > 0 {
			s.Apply(batch)
		}
	}
}
