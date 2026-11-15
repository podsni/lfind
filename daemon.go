package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type Config struct {
	DB, Sock, Root string
	Workers        int
	Exclude        []string
	IncludeFuse    bool
	Meta           bool
}

type Server struct {
	cfg        Config
	mu         sync.RWMutex // readers: queries; writer: swap/apply
	wmu        sync.Mutex   // serialises all index mutations (events, rebuild, rescan swap)
	ix         *Index
	sk         *Skipper
	mounts     []Mount
	dirty      bool
	rescanning bool
	pending    [][2]string
	scanMu     sync.Mutex
	events     uint64
}

func (s *Server) Apply(batch [][2]string) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rescanning {
		s.pending = append(s.pending, batch...)
	}
	for _, e := range batch {
		s.reconcile(e[0], e[1], 0)
	}
	s.events += uint64(len(batch))
	s.dirty = true
}

func (s *Server) under(p string) bool {
	r := s.ix.RootPath()
	return r == "/" || p == r || strings.HasPrefix(p, r+"/")
}

// reconcile makes the index agree with the filesystem for dir/name.
func (s *Server) reconcile(dir, name string, depth int) {
	if depth > 64 || !s.under(dir) {
		return
	}
	full := joinPath(dir, name)
	if s.sk.Excluded(full) || (s.sk.Excluded(dir) && dir != s.ix.RootPath()) {
		return
	}
	ix := s.ix
	pid, ok := ix.Resolve(dir)
	if !ok {
		if dir == ix.RootPath() || dir == "/" {
			return
		}
		s.reconcile(filepath.Dir(dir), filepath.Base(dir), depth+1)
		return
	}
	var st unix.Stat_t
	exists := unix.Lstat(full, &st) == nil
	isDir := exists && st.Mode&unix.S_IFMT == unix.S_IFDIR
	cid, found := ix.Lookup(pid, []byte(name))
	if found && (!exists || ix.IsDir(cid) != isDir) {
		ix.Delete(cid)
		found = false
	}
	if !exists {
		return
	}
	sz, mt := statMeta(&st)
	if found {
		ix.setMeta(cid, sz, mt)
		return
	}
	id := ix.Add(pid, []byte(name), isDir, sz, mt)
	if isDir && !s.sk.Skip(full) {
		f := &flat{Meta: ix.Meta}
		shardsToFlat(f, Walk([]job{{full, ref{-1, subRoot}}}, s.sk, 4, ix.Meta))
		n0 := uint32(ix.Len())
		for i := range f.P {
			p := f.P[i]
			if p == subRoot {
				p = id
			} else {
				p = n0 + p // parent inside this subtree
			}
			var sz uint64
			var mt uint32
			if f.Meta {
				sz, mt = f.Sz[i], f.Mt[i]
			}
			ix.Add(p, f.raw(uint32(i)), f.Fl[i]&flagDir != 0, sz, mt)
		}
	}
}

func (s *Server) swap(nix *Index) {
	old := s.ix
	s.ix = nix
	if old.mm != nil && (nix.mm == nil || &old.mm[0] != &nix.mm[0]) {
		go func(b []byte) { time.Sleep(5 * time.Second); unix.Munmap(b) }(old.mm)
	}
	go func() { time.Sleep(100 * time.Millisecond); debug.FreeOSMemory() }()
}

func (s *Server) Rescan(reason string) {
	if !s.scanMu.TryLock() {
		return
	}
	defer s.scanMu.Unlock()
	log.Printf("rescan started (%s)", reason)
	s.wmu.Lock()
	s.rescanning, s.pending = true, nil
	s.wmu.Unlock()
	t0 := time.Now()
	nix := BuildIndex(s.cfg.Root, s.sk, s.cfg.Workers, s.cfg.Meta)
	s.wmu.Lock()
	s.mu.Lock()
	s.swap(nix)
	for _, e := range s.pending {
		s.reconcile(e[0], e[1], 0)
	}
	s.pending, s.rescanning, s.dirty = nil, false, true
	s.mu.Unlock()
	s.wmu.Unlock()
	log.Printf("rescan done in %v: %s", time.Since(t0).Round(time.Millisecond), s.ix.Stats())
	s.Save()
}

// Save merges the delta into a re-sorted base (readers keep running) and writes the cache.
func (s *Server) Save() {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if !s.dirty {
		return
	}
	t0 := time.Now()
	ix := s.ix
	if ix.Delta() > 0 || ix.dead > 0 {
		s.mu.RLock()
		nix := ix.Rebuild()
		s.mu.RUnlock()
		s.mu.Lock()
		s.swap(nix)
		s.mu.Unlock()
		ix = nix
	}
	t1 := time.Now()
	if err := ix.Save(s.cfg.DB); err != nil {
		log.Printf("save: %v", err)
		return
	}
	s.dirty = false
	log.Printf("rebuilt in %v, saved %s (%d entries) in %v", t1.Sub(t0).Round(time.Millisecond), s.cfg.DB, ix.Len(), time.Since(t1).Round(time.Millisecond))
}

const subRoot = noParent - 1

type wireReq struct {
	Query
	Op string `json:"op,omitempty"`
}

func rssKB() int {
	b, _ := os.ReadFile("/proc/self/status")
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmRSS:") {
			v, _ := strconv.Atoi(strings.Fields(l)[1])
			return v
		}
	}
	return 0
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	var req wireReq
	w := bufio.NewWriterSize(c, 256<<10)
	defer w.Flush()
	if err := json.Unmarshal(line, &req); err != nil {
		fmt.Fprintf(w, "ERR %v\n", err)
		return
	}
	if req.Op == "stats" {
		s.mu.RLock()
		fmt.Fprintf(w, "OK %s events=%d rescanning=%v rss=%.1fMB\n", s.ix.Stats(), s.events, s.rescanning, float64(rssKB())/1024)
		s.mu.RUnlock()
		return
	}
	s.mu.RLock()
	res, err := s.ix.Search(req.Query)
	s.mu.RUnlock()
	if err != nil {
		fmt.Fprintf(w, "ERR %v\n", err)
		return
	}
	writeResult(w, res)
}

func writeResult(w *bufio.Writer, res Result) {
	long := 0
	if res.Sizes != nil {
		long = 1
	}
	fmt.Fprintf(w, "OK %d %d %d %d\n", res.Total, res.Elapsed.Microseconds(), len(res.Paths), long)
	for i, p := range res.Paths {
		if long == 1 {
			d := 0
			if res.Dirs[i] {
				d = 1
			}
			fmt.Fprintf(w, "%d\t%d\t%d\t", res.Sizes[i], res.Mtimes[i], d)
		}
		w.WriteString(p)
		w.WriteByte('\n')
	}
}

func RunDaemon(cfg Config, noRescan bool) error {
	debug.SetGCPercent(20) // index is pointer-free; keep heap slack small
	sk, mounts := BuildSkipper(cfg.Exclude, cfg.IncludeFuse)
	s := &Server{cfg: cfg, sk: sk, mounts: mounts}
	t0 := time.Now()
	ix, err := Load(cfg.DB, true)
	loaded := err == nil && ix.RootPath() == filepath.Clean(cfg.Root) && ix.Meta == cfg.Meta
	if loaded {
		log.Printf("loaded cache %s in %v: %s", cfg.DB, time.Since(t0).Round(time.Microsecond), ix.Stats())
	} else {
		if err != nil {
			log.Printf("cache: %v", err)
		}
		log.Printf("scanning %s ...", cfg.Root)
		ix = BuildIndex(cfg.Root, sk, cfg.Workers, cfg.Meta)
		log.Printf("scanned in %v: %s", time.Since(t0).Round(time.Millisecond), ix.Stats())
		s.dirty = true
	}
	s.ix = ix

	var wt Watcher
	if os.Geteuid() == 0 {
		var wm []Mount
		for _, m := range mounts {
			if s.under(m.Point) || strings.HasPrefix(cfg.Root, m.Point) {
				wm = append(wm, m)
			}
		}
		if fw, err := newFanotify(wm, cfg.Meta); err == nil {
			wt = fw
		} else {
			log.Printf("fanotify unavailable (%v), falling back to inotify", err)
		}
	}
	if wt == nil {
		iw, err := newInotify(sk, cfg.Meta)
		if err != nil {
			return err
		}
		wt = iw
	}
	log.Printf("live updates via %s", wt.Kind())
	go wt.Run(s)

	os.Remove(cfg.Sock)
	ln, err := net.Listen("unix", cfg.Sock)
	if err != nil {
		return err
	}
	os.Chmod(cfg.Sock, 0666)
	log.Printf("listening on %s", cfg.Sock)

	if loaded && !noRescan {
		go s.Rescan("catch up with changes made while daemon was stopped")
	} else if s.dirty {
		go s.Save()
	}
	go func() {
		for range time.Tick(5 * time.Minute) {
			s.Save()
		}
	}()
	go func() { // fold big deltas into the sorted base sooner
		for range time.Tick(10 * time.Second) {
			s.mu.RLock()
			big := s.ix.Delta() > 50000 || s.ix.dead > 200000
			s.mu.RUnlock()
			if big {
				s.Save()
			}
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Printf("shutting down, saving index")
		ln.Close()
		s.Save()
		os.Remove(cfg.Sock)
		os.Exit(0)
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			select {}
		}
		go s.handle(c)
	}
}

// ---- client ----

func remoteSearch(sock string, q Query) (Result, error) {
	c, err := net.DialTimeout("unix", sock, 200*time.Millisecond)
	if err != nil {
		return Result{}, err
	}
	defer c.Close()
	b, _ := json.Marshal(wireReq{Query: q})
	c.Write(append(b, '\n'))
	r := bufio.NewReaderSize(c, 256<<10)
	hdr, err := r.ReadString('\n')
	if err != nil {
		return Result{}, err
	}
	if strings.HasPrefix(hdr, "ERR ") {
		return Result{}, fmt.Errorf("%s", strings.TrimSpace(hdr[4:]))
	}
	var res Result
	var us int64
	var n, long int
	if _, err := fmt.Sscanf(hdr, "OK %d %d %d %d", &res.Total, &us, &n, &long); err != nil {
		return Result{}, fmt.Errorf("bad reply %q", hdr)
	}
	res.Elapsed = time.Duration(us) * time.Microsecond
	res.Paths = make([]string, 0, n)
	for i := 0; i < n; i++ {
		l, err := r.ReadString('\n')
		if err != nil {
			break
		}
		l = l[:len(l)-1]
		if long == 1 {
			f := strings.SplitN(l, "\t", 4)
			if len(f) == 4 {
				sz, _ := strconv.ParseUint(f[0], 10, 64)
				mt, _ := strconv.ParseInt(f[1], 10, 64)
				res.Sizes = append(res.Sizes, sz)
				res.Mtimes = append(res.Mtimes, mt)
				res.Dirs = append(res.Dirs, f[2] == "1")
				l = f[3]
			}
		}
		res.Paths = append(res.Paths, l)
	}
	return res, nil
}

func remoteStats(sock string) (string, error) {
	c, err := net.DialTimeout("unix", sock, 200*time.Millisecond)
	if err != nil {
		return "", err
	}
	defer c.Close()
	c.Write([]byte(`{"op":"stats"}` + "\n"))
	l, err := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(l), err
}
