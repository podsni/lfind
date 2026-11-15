package main

import (
	"errors"
	"net"
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// version is set at build time: -ldflags "-X main.version=v1.0.0"
var version = "dev"

const usage = `lfind - instant filename search for Linux (an "Everything" clone)

Usage:
  lfind index  [-root /] [-j N] [-exclude a,b] [-include-fuse] [-meta=false] [-db FILE]
  lfind daemon [-root /] [-j N] [-exclude a,b] [-meta=false] [-no-rescan] [-db FILE] [-sock FILE]
  lfind stats | version
  lfind [options] QUERY...          (options may appear anywhere)
  lfind -i [QUERY]                  interactive search-as-you-type (Enter prints the path)

Options:
  -n N          max results (0 = all)          -count   print only the number of matches
  -sort KEY     name (default) | path | size | dm      -desc  descending
  -l            long output: size, modified time, path
  -c  match case    -w  match whole words    -p  match full path    -r  query is one regex
  -t            print match count + latency to stderr
  -local        don't use the daemon; search the cache file directly

Search syntax (Everything-compatible subset; terms AND, | is OR and binds tighter):
  foo bar            name contains foo and bar (case- and accent-insensitive)
  *.mp? a?c*         wildcards match the whole name; ? = one character
  .jpg | .png        OR          !tmp      NOT          "my file"   quoted
  ext:jpg;png        extensions  size:>10mb size:1kb..2mb size:empty|tiny|small|medium|large|huge|gigantic
  dm:today dm:yesterday dm:thisweek dm:lastmonth dm:last3days dm:2026 dm:2026-11 dm:>=2026-01-01 dm:a..b
  folder: file:      only folders / only files (also as prefix: folder:src)
  case: ww: path: diacritics: nocase: nowholeword: nopath:    per-term modifiers, combinable
  regex:^a.*z$       regex term on the name
  parent:/usr/bin    direct children of a folder;  any term containing / matches the full path

Environment: LFIND_DB (cache file), LFIND_SOCK (daemon socket), LFIND_DEBUG=1 (build phase timings).
`

func defaultDB() string {
	if v := os.Getenv("LFIND_DB"); v != "" {
		return v
	}
	if os.Geteuid() == 0 {
		return "/var/cache/lfind/index.db"
	}
	if h, err := os.UserCacheDir(); err == nil {
		return filepath.Join(h, "lfind", "index.db")
	}
	return "/tmp/lfind-index.db"
}

func defaultSock() string {
	if v := os.Getenv("LFIND_SOCK"); v != "" {
		return v
	}
	return "/tmp/lfind.sock"
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func cfgFlags(fs *flag.FlagSet) (*Config, *string) {
	c := &Config{}
	fs.StringVar(&c.DB, "db", defaultDB(), "index cache file")
	fs.StringVar(&c.Sock, "sock", defaultSock(), "daemon socket")
	fs.StringVar(&c.Root, "root", "/", "directory to index")
	fs.IntVar(&c.Workers, "j", runtime.NumCPU()*2, "scan workers")
	fs.BoolVar(&c.IncludeFuse, "include-fuse", false, "also index FUSE mounts")
	fs.BoolVar(&c.Meta, "meta", true, "index size and modification time (+12 B/entry)")
	ex := fs.String("exclude", "", "comma-separated paths to skip")
	return c, ex
}

func main() {
	log.SetFlags(log.Ltime)
	log.SetPrefix("lfind: ")
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "index":
			fs := flag.NewFlagSet("index", flag.ExitOnError)
			c, ex := cfgFlags(fs)
			fs.Parse(args[1:])
			c.Exclude = splitList(*ex)
			cmdIndex(c)
			return
		case "daemon":
			fs := flag.NewFlagSet("daemon", flag.ExitOnError)
			c, ex := cfgFlags(fs)
			nr := fs.Bool("no-rescan", false, "don't rescan after loading the cache")
			fs.Parse(args[1:])
			c.Exclude = splitList(*ex)
			c.Root, _ = filepath.Abs(c.Root)
			if err := RunDaemon(*c, *nr); err != nil {
				log.Fatal(err)
			}
			return
		case "stats":
			fs := flag.NewFlagSet("stats", flag.ExitOnError)
			c, _ := cfgFlags(fs)
			fs.Parse(args[1:])
			if s, err := remoteStats(c.Sock); err == nil {
				fmt.Println("daemon:", s)
			} else {
				fmt.Println("daemon: not running")
			}
			t0 := time.Now()
			if ix, err := loadAny(c.DB); err == nil {
				fmt.Printf("cache: %s (loaded in %v)\n", ix.Stats(), time.Since(t0).Round(time.Microsecond))
			} else {
				fmt.Println("cache:", err)
			}
			return
		case "version", "--version", "-version":
			fmt.Println("lfind", version)
			return
		case "help", "-h", "--help":
			fmt.Print(usage)
			return
		}
	}
	fs := flag.NewFlagSet("lfind", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	db := fs.String("db", defaultDB(), "")
	sock := fs.String("sock", defaultSock(), "")
	inter := fs.Bool("i", false, "")
	n := fs.Int("n", 0, "")
	cs := fs.Bool("c", false, "")
	re := fs.Bool("r", false, "")
	srt := fs.String("sort", "name", "")
	desc := fs.Bool("desc", false, "")
	ww := fs.Bool("w", false, "")
	mp := fs.Bool("p", false, "")
	long := fs.Bool("l", false, "")
	cnt := fs.Bool("count", false, "")
	tm := fs.Bool("t", false, "")
	local := fs.Bool("local", false, "")
	var terms []string
	for rest := args; ; {
		fs.Parse(rest)
		if fs.NArg() == 0 {
			break
		}
		terms = append(terms, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	q := Query{Q: strings.Join(terms, " "), Limit: *n, Case: *cs, Regex: *re, Sort: *srt, Desc: *desc, Count: *cnt, WholeWord: *ww, MatchPath: *mp, Long: *long}
	search := makeSearcher(*db, *sock, *local)
	if *inter {
		if err := RunTUI(search, q); err != nil {
			log.Fatal(err)
		}
		return
	}
	if q.Q == "" {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	t0 := time.Now()
	res, err := search(q)
	if err != nil {
		log.Fatal(err)
	}
	if *cnt {
		fmt.Println(res.Total)
	} else {
		w := bufio.NewWriterSize(os.Stdout, 1<<20)
		for i, p := range res.Paths {
			if *long && i < len(res.Sizes) {
				sz := humanSize(res.Sizes[i])
				if res.Dirs[i] {
					sz = "<DIR>"
				}
				fmt.Fprintf(w, "%10s  %s  ", sz, time.Unix(res.Mtimes[i], 0).Format("2006-01-02 15:04"))
			}
			w.WriteString(p)
			w.WriteByte('\n')
		}
		w.Flush()
	}
	if *tm {
		fmt.Fprintf(os.Stderr, "%d matches, search %v, total %v\n", res.Total, res.Elapsed, time.Since(t0))
	}
	if res.Total == 0 {
		os.Exit(1)
	}
}

func humanSize(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%d B", b)
}

func loadAny(db string) (*Index, error) {
	ix, err := Load(db, false)
	if err != nil && os.Getenv("LFIND_DB") == "" && db != "/var/cache/lfind/index.db" {
		if ix2, err2 := Load("/var/cache/lfind/index.db", false); err2 == nil {
			return ix2, nil
		}
	}
	return ix, err
}

func makeSearcher(db, sock string, local bool) func(Query) (Result, error) {
	var ix *Index
	useDaemon := !local
	return func(q Query) (Result, error) {
		if useDaemon {
			r, err := remoteSearch(sock, q)
			if err == nil || !isDialErr(err) {
				return r, err
			}
			useDaemon = false
		}
		if ix == nil {
			var err error
			if ix, err = loadAny(db); err != nil {
				return Result{}, fmt.Errorf("no daemon at %s and no cache (%v); run 'lfind index' first", sock, err)
			}
		}
		return ix.Search(q)
	}
}

func isDialErr(err error) bool {
	var oe *net.OpError
	return errors.As(err, &oe) && oe.Op == "dial"
}

func cmdIndex(c *Config) {
	root, _ := filepath.Abs(c.Root)
	sk, _ := BuildSkipper(c.Exclude, c.IncludeFuse)
	t0 := time.Now()
	ix := BuildIndex(root, sk, c.Workers, c.Meta)
	ts := time.Since(t0)
	if os.Getenv("LFIND_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "  total build: %v\n", ts.Round(time.Millisecond))
	}
	t1 := time.Now()
	if err := ix.Save(c.DB); err != nil {
		log.Fatal(err)
	}
	st, _ := os.Stat(c.DB)
	fmt.Printf("indexed %s: %s\nscan %v, save %v, cache %s (%.1f MB)\n", root, ix.Stats(),
		ts.Round(time.Millisecond), time.Since(t1).Round(time.Millisecond), c.DB, float64(st.Size())/1e6)
}
