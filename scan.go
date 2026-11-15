package main

import (
	"bufio"
	"encoding/binary"
	"os"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

type ref struct {
	shard int32 // -1: idx is a global id in the target index
	idx   uint32
}

type shardBuf struct {
	parent []ref
	off    []uint32
	flags  []uint8
	sz     []uint64
	mt     []uint32
	names  []byte // exact names, NUL-terminated
}

type job struct {
	path string
	self ref
}

type queue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	stack  []job
	active int
}

func (q *queue) pop() (job, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.stack) == 0 {
		if q.active == 0 {
			q.cond.Broadcast()
			return job{}, false
		}
		q.cond.Wait()
	}
	j := q.stack[len(q.stack)-1]
	q.stack = q.stack[:len(q.stack)-1]
	q.active++
	return j, true
}

func (q *queue) done(newJobs []job) {
	q.mu.Lock()
	q.stack = append(q.stack, newJobs...)
	q.active--
	if len(newJobs) > 0 || (q.active == 0 && len(q.stack) == 0) {
		q.cond.Broadcast()
	}
	q.mu.Unlock()
}

type Skipper struct {
	exact    map[string]bool
	prefixes []string
}

func (s *Skipper) Skip(p string) bool { return s.exact[p] }
func (s *Skipper) Excluded(p string) bool {
	for _, x := range s.prefixes {
		if p == x || strings.HasPrefix(p, x+"/") {
			return true
		}
	}
	return false
}

func joinPath(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}

// Walk does a parallel directory walk using raw getdents64 (+ fstatat per
// entry when meta is set, for size and mtime).
func Walk(roots []job, sk *Skipper, workers int, meta bool) []*shardBuf {
	q := &queue{stack: append([]job(nil), roots...)}
	q.cond = sync.NewCond(&q.mu)
	shards := make([]*shardBuf, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		sb := &shardBuf{}
		shards[w] = sb
		wg.Add(1)
		go func(w int, sb *shardBuf) {
			defer wg.Done()
			buf := make([]byte, 64<<10)
			var nj []job
			for {
				j, ok := q.pop()
				if !ok {
					return
				}
				nj = scanDir(j, int32(w), sb, buf, sk, nj[:0], meta)
				q.done(nj)
			}
		}(w, sb)
	}
	wg.Wait()
	return shards
}

func statMeta(st *unix.Stat_t) (uint64, uint32) {
	sz := uint64(st.Size)
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		sz = 0
	}
	mt := st.Mtim.Sec
	if mt < 0 {
		mt = 0
	}
	return sz, uint32(mt)
}

func scanDir(j job, w int32, sb *shardBuf, buf []byte, sk *Skipper, nj []job, meta bool) []job {
	fd, err := unix.Open(j.path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nj
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	for {
		n, err := unix.Getdents(fd, buf)
		if err != nil || n <= 0 {
			break
		}
		for off := 0; off < n; {
			reclen := int(binary.LittleEndian.Uint16(buf[off+16:]))
			typ := buf[off+18]
			name := buf[off+19 : off+reclen]
			if k := indexByte0(name); k >= 0 {
				name = name[:k]
			}
			off += reclen
			if len(name) == 0 || (name[0] == '.' && (len(name) == 1 || (len(name) == 2 && name[1] == '.'))) {
				continue
			}
			isDir := typ == unix.DT_DIR
			var sz uint64
			var mt uint32
			if meta || typ == unix.DT_UNKNOWN {
				if unix.Fstatat(fd, string(name), &st, unix.AT_SYMLINK_NOFOLLOW) == nil {
					isDir = st.Mode&unix.S_IFMT == unix.S_IFDIR
					sz, mt = statMeta(&st)
				}
			}
			idx := uint32(len(sb.parent))
			sb.parent = append(sb.parent, j.self)
			sb.off = append(sb.off, uint32(len(sb.names)))
			var f uint8
			if isDir {
				f = flagDir
			}
			sb.flags = append(sb.flags, f)
			if meta {
				sb.sz = append(sb.sz, sz)
				sb.mt = append(sb.mt, mt)
			}
			sb.names = append(append(sb.names, name...), 0)
			if isDir {
				cp := joinPath(j.path, string(name))
				if !sk.Skip(cp) {
					nj = append(nj, job{cp, ref{w, idx}})
				}
			}
		}
	}
	return nj
}

// flat is an unsorted entry table with exact names (build input).
type flat struct {
	P    []uint32
	Fl   []uint8
	Sz   []uint64
	Mt   []uint32
	Raw  []byte
	ROff []uint32
	Root uint32
	Meta bool
}

func (f *flat) raw(i uint32) []byte {
	e := uint32(len(f.Raw)) - 1
	if int(i)+1 < len(f.ROff) {
		e = f.ROff[i+1] - 1
	}
	return f.Raw[f.ROff[i]:e]
}

// shardsToFlat appends walker output to f; global refs (shard -1) are kept as-is.
func shardsToFlat(f *flat, shards []*shardBuf) {
	base := make([]uint32, len(shards))
	nbase := make([]uint32, len(shards))
	n0 := len(f.P)
	n, nb := n0, len(f.Raw)
	for i, s := range shards {
		base[i], nbase[i] = uint32(n), uint32(nb)
		n += len(s.parent)
		nb += len(s.names)
	}
	f.P = growU32(f.P, n)
	f.ROff = growU32(f.ROff, n)
	f.Fl = growU8(f.Fl, n)
	f.Raw = growU8(f.Raw, nb)
	if f.Meta {
		f.Mt = growU32(f.Mt, n)
		if cap(f.Sz) >= n {
			f.Sz = f.Sz[:n]
		} else {
			t := make([]uint64, n)
			copy(t, f.Sz)
			f.Sz = t
		}
	}
	var wg sync.WaitGroup
	for si, s := range shards {
		wg.Add(1)
		go func(si int, s *shardBuf) {
			defer wg.Done()
			b, nbB := base[si], nbase[si]
			for k, r := range s.parent {
				p := r.idx
				if r.shard >= 0 {
					p = base[r.shard] + r.idx
				}
				f.P[b+uint32(k)] = p
				f.ROff[b+uint32(k)] = nbB + s.off[k]
			}
			copy(f.Fl[b:], s.flags)
			copy(f.Raw[nbB:], s.names)
			if f.Meta {
				copy(f.Sz[b:], s.sz)
				copy(f.Mt[b:], s.mt)
			}
		}(si, s)
	}
	wg.Wait()
}

func growU32(s []uint32, n int) []uint32 {
	if cap(s) >= n {
		return s[:n]
	}
	t := make([]uint32, n, n+n/8)
	copy(t, s)
	return t
}
func growU8(s []byte, n int) []byte {
	if cap(s) >= n {
		return s[:n]
	}
	t := make([]byte, n, n+n/8)
	copy(t, s)
	return t
}

// BuildIndex scans root from scratch and returns a sorted index.
func BuildIndex(root string, sk *Skipper, workers int, meta bool) *Index {
	f := &flat{Meta: meta}
	rn := root
	if root == "/" {
		rn = ""
	}
	f.P = []uint32{noParent}
	f.Fl = []uint8{flagDir}
	f.ROff = []uint32{0}
	f.Raw = append([]byte(rn), 0)
	if meta {
		var st unix.Stat_t
		unix.Lstat(root, &st)
		sz, mt := statMeta(&st)
		f.Sz, f.Mt = []uint64{sz}, []uint32{mt}
	}
	tl := newTimer()
	sh := Walk([]job{{root, ref{-1, 0}}}, sk, workers, meta)
	tl.mark("walk")
	shardsToFlat(f, sh)
	tl.mark("flatten")
	return finalize(f)
}

var pseudoFS = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true, "cgroup": true, "cgroup2": true,
	"securityfs": true, "debugfs": true, "tracefs": true, "pstore": true, "bpf": true, "mqueue": true,
	"hugetlbfs": true, "configfs": true, "fusectl": true, "selinuxfs": true, "autofs": true,
	"binfmt_misc": true, "efivarfs": true, "rpc_pipefs": true, "nsfs": true, "ramfs": true,
	"nfs": true, "nfs4": true, "cifs": true, "smb3": true, "sshfs": true, "9p": true, "squashfs": true,
}

var defaultExcl = []string{"/proc", "/sys", "/dev", "/run", "/tmp/.X11-unix"}

type Mount struct{ Point, FS string }

func readMounts() []Mount {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	defer f.Close()
	var ms []Mount
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fl := strings.Fields(sc.Text())
		for i, x := range fl {
			if x == "-" && i+1 < len(fl) && len(fl) > 4 {
				ms = append(ms, Mount{unescapeMount(fl[4]), fl[i+1]})
				break
			}
		}
	}
	return ms
}

func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// BuildSkipper returns the skip set and the list of real mounts to index/watch.
func BuildSkipper(extra []string, includeFuse bool) (*Skipper, []Mount) {
	sk := &Skipper{exact: map[string]bool{}}
	for _, p := range append(append([]string{}, defaultExcl...), extra...) {
		if p = strings.TrimRight(p, "/"); p != "" {
			sk.exact[p] = true
			sk.prefixes = append(sk.prefixes, p)
		}
	}
	var real []Mount
	for _, m := range readMounts() {
		if pseudoFS[m.FS] || (!includeFuse && strings.HasPrefix(m.FS, "fuse")) {
			if m.Point != "/" {
				sk.exact[m.Point] = true
				sk.prefixes = append(sk.prefixes, m.Point)
			}
			continue
		}
		real = append(real, m)
	}
	var keep []Mount
	for _, m := range real {
		if !sk.Excluded(m.Point) {
			keep = append(keep, m)
		}
	}
	return sk, keep
}

