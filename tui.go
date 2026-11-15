package main

import (
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

func RunTUI(search func(Query) (Result, error), q0 Query) error {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer tty.Close()
	fd := int(tty.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return err
	}
	raw := *old
	raw.Iflag &^= unix.ICRNL | unix.IXON | unix.BRKINT | unix.INPCK | unix.ISTRIP
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 1, 0
	unix.IoctlSetTermios(fd, unix.TCSETS, &raw)
	fmt.Fprint(tty, "\x1b[?1049h")
	restore := func() {
		fmt.Fprint(tty, "\x1b[?1049l")
		unix.IoctlSetTermios(fd, unix.TCSETS, old)
	}
	query := []rune(q0.Q)
	sel := 0
	var res Result
	var serr error
	chosen := ""
	run := func() {
		ws, _ := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
		rows := 24
		if ws != nil && ws.Row > 3 {
			rows = int(ws.Row)
		}
		q := q0
		q.Q, q.Limit, q.Long = string(query), rows-2, true
		if strings.TrimSpace(q.Q) == "" {
			res, serr = Result{}, nil
			return
		}
		res, serr = search(q)
	}
	render := func() {
		ws, _ := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
		cols := 80
		if ws != nil && ws.Col > 10 {
			cols = int(ws.Col)
		}
		var b strings.Builder
		b.WriteString("\x1b[H\x1b[2J")
		if serr != nil {
			fmt.Fprintf(&b, "\x1b[2;1H\x1b[31m%s\x1b[0m", serr)
		} else if strings.TrimSpace(string(query)) != "" {
			fmt.Fprintf(&b, "\x1b[2;1H\x1b[2m%d matches in %.2f ms  (↑↓ select, Enter print, Esc quit)\x1b[0m", res.Total, float64(res.Elapsed.Microseconds())/1000)
		}
		if sel >= len(res.Paths) {
			sel = len(res.Paths) - 1
		}
		if sel < 0 {
			sel = 0
		}
		for i, p := range res.Paths {
			pre := ""
			if i < len(res.Sizes) && cols > 60 {
				sz := humanSize(res.Sizes[i])
				if res.Dirs[i] {
					sz = "<DIR>"
				}
				pre = fmt.Sprintf("%10s  %s  ", sz, time.Unix(res.Mtimes[i], 0).Format("2006-01-02 15:04"))
			}
			w := cols - len(pre)
			if utf8.RuneCountInString(p) > w {
				r := []rune(p)
				p = "…" + string(r[len(r)-w+1:])
			}
			p = pre + p
			fmt.Fprintf(&b, "\x1b[%d;1H", i+3)
			if i == sel {
				b.WriteString("\x1b[7m" + p + "\x1b[0m")
			} else {
				b.WriteString(p)
			}
		}
		fmt.Fprintf(&b, "\x1b[1;1H> %s", string(query))
		tty.WriteString(b.String())
	}
	run()
	render()
	buf := make([]byte, 256)
loop:
	for {
		n, err := tty.Read(buf)
		if err != nil {
			break
		}
		in := buf[:n]
		changed := false
		for len(in) > 0 {
			c := in[0]
			switch {
			case c == 27 && len(in) >= 3 && in[1] == '[':
				switch in[2] {
				case 'A':
					sel--
				case 'B':
					sel++
				}
				in = in[3:]
				continue
			case c == 27 || c == 3 || c == 4:
				break loop
			case c == 13 || c == 10:
				if sel < len(res.Paths) && len(res.Paths) > 0 {
					chosen = res.Paths[sel]
				}
				break loop
			case c == 127 || c == 8:
				if len(query) > 0 {
					query = query[:len(query)-1]
					changed = true
				}
			case c == 21:
				query, changed = nil, true
			case c == 23:
				s := strings.TrimRight(string(query), " ")
				if i := strings.LastIndex(s, " "); i >= 0 {
					query = []rune(s[:i+1])
				} else {
					query = nil
				}
				changed = true
			case c >= 32:
				r, sz := utf8.DecodeRune(in)
				query = append(query, r)
				in = in[sz:]
				changed = true
				continue
			}
			in = in[1:]
		}
		if changed {
			sel = 0
			run()
		}
		render()
	}
	restore()
	if chosen != "" {
		fmt.Println(chosen)
	}
	return nil
}
