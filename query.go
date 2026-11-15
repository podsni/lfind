package main

import (
	"bytes"
	"fmt"
	"regexp"
	"regexp/syntax"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type Query struct {
	Q         string `json:"q"`
	Limit     int    `json:"limit,omitempty"`
	Case      bool   `json:"case,omitempty"`
	Regex     bool   `json:"regex,omitempty"`
	WholeWord bool   `json:"ww,omitempty"`
	MatchPath bool   `json:"mp,omitempty"`
	Sort      string `json:"sort,omitempty"` // name (default) | path | size | dm
	Desc      bool   `json:"desc,omitempty"`
	Count     bool   `json:"count,omitempty"`
	Long      bool   `json:"long,omitempty"`
}

const (
	tSub = iota
	tGlob
	tExt
	tRegex
	tSize
	tDate
	tAll
	tParent
)

type term struct {
	kind   int
	neg    bool
	cs     bool // case: (exact match)
	diac   bool // diacritics: (case-insensitive, accent-sensitive)
	ww     bool
	inPath bool
	only   int // 1 files, 2 folders
	val    []byte
	raw    []byte
	dval   []byte
	exts   [][]byte
	re     *regexp.Regexp
	lit    []byte // literal every regex match must contain (folded)
	lo, hi int64
}

type tok struct {
	s  string
	or bool
}

func tokenize(q string) []tok {
	var out []tok
	var cur strings.Builder
	inQ, has := false, false
	flush := func() {
		if has {
			out = append(out, tok{s: cur.String()})
		}
		cur.Reset()
		has = false
	}
	for _, r := range q {
		switch {
		case r == '"':
			inQ = !inQ
			has = true
		case (r == ' ' || r == '\t') && !inQ:
			flush()
		case r == '|' && !inQ:
			flush()
			out = append(out, tok{or: true})
		default:
			cur.WriteRune(r)
			has = true
		}
	}
	flush()
	return out
}

// parseQuery returns AND-ed groups of OR-ed terms (Everything: OR binds tighter than AND).
func parseQuery(q Query) ([][]term, error) {
	if q.Regex {
		t := term{kind: tRegex, cs: q.Case, inPath: q.MatchPath}
		re, err := mkRegex(q.Q, q.Case)
		if err != nil {
			return nil, err
		}
		t.re, t.lit = re, regexLiteral(q.Q)
		return [][]term{{t}}, nil
	}
	var groups [][]term
	or := false
	for _, tk := range tokenize(q.Q) {
		if tk.or {
			or = true
			continue
		}
		t, ok, err := parseTerm(tk.s, q)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if or && len(groups) > 0 {
			groups[len(groups)-1] = append(groups[len(groups)-1], t)
		} else {
			groups = append(groups, []term{t})
		}
		or = false
	}
	return groups, nil
}

// regexLiteral returns the longest literal that every match must contain (folded),
// used as the SIMD prefilter for regex searches.
func regexLiteral(s string) []byte {
	re, err := syntax.Parse(s, syntax.Perl)
	if err != nil {
		return nil
	}
	re = re.Simplify()
	var best []rune
	switch re.Op {
	case syntax.OpLiteral:
		best = re.Rune
	case syntax.OpConcat:
		for _, sub := range re.Sub {
			if sub.Op == syntax.OpLiteral && len(sub.Rune) > len(best) {
				best = sub.Rune
			}
		}
	}
	return fold([]byte(string(best)))
}

func mkRegex(s string, cs bool) (*regexp.Regexp, error) {
	if !cs {
		s = "(?i)" + s
	}
	return regexp.Compile(s)
}

var modifiers = []struct {
	p string
	f func(t *term)
}{
	{"case:", func(t *term) { t.cs = true }},
	{"nocase:", func(t *term) { t.cs = false }},
	{"wholeword:", func(t *term) { t.ww = true }},
	{"ww:", func(t *term) { t.ww = true }},
	{"nowholeword:", func(t *term) { t.ww = false }},
	{"path:", func(t *term) { t.inPath = true }},
	{"nopath:", func(t *term) { t.inPath = false }},
	{"files:", func(t *term) { t.only = 1 }},
	{"file:", func(t *term) { t.only = 1 }},
	{"folders:", func(t *term) { t.only = 2 }},
	{"folder:", func(t *term) { t.only = 2 }},
	{"diacritics:", func(t *term) { t.diac = true }},
	{"nodiacritics:", func(t *term) { t.diac = false }},
}

func parseTerm(s string, q Query) (term, bool, error) {
	t := term{cs: q.Case, ww: q.WholeWord, inPath: q.MatchPath}
	for {
		if len(s) > 1 && s[0] == '!' {
			t.neg = !t.neg
			s = s[1:]
			continue
		}
		ls := strings.ToLower(s)
		hit := false
		for _, m := range modifiers {
			if strings.HasPrefix(ls, m.p) {
				m.f(&t)
				s = s[len(m.p):]
				hit = true
				break
			}
		}
		if !hit {
			break
		}
	}
	ls := strings.ToLower(s)
	var err error
	switch {
	case strings.HasPrefix(ls, "ext:"):
		t.kind = tExt
		for _, e := range strings.FieldsFunc(s[4:], func(r rune) bool { return r == ';' || r == ',' }) {
			e = strings.TrimPrefix(e, ".")
			if t.cs {
				t.exts = append(t.exts, []byte(e))
			} else {
				t.exts = append(t.exts, fold([]byte(e)))
			}
		}
		return t, true, nil
	case strings.HasPrefix(ls, "regex:"):
		t.kind = tRegex
		t.lit = regexLiteral(s[6:])
		t.re, err = mkRegex(s[6:], t.cs)
		return t, err == nil, err
	case strings.HasPrefix(ls, "size:"):
		t.kind = tSize
		t.lo, t.hi, err = parseSize(s[5:])
		return t, err == nil, err
	case strings.HasPrefix(ls, "dm:") || strings.HasPrefix(ls, "datemodified:"):
		t.kind = tDate
		t.lo, t.hi, err = parseDate(s[strings.IndexByte(s, ':')+1:], time.Now())
		return t, err == nil, err
	case strings.HasPrefix(ls, "parent:") || strings.HasPrefix(ls, "infolder:"):
		t.kind = tParent
		v := strings.TrimRight(s[strings.IndexByte(s, ':')+1:], "/")
		if v == "" {
			v = "/"
		}
		t.val, t.raw = fold([]byte(v)), []byte(v)
		return t, true, nil
	}
	if s == "" {
		if t.only != 0 {
			t.kind = tAll
			return t, true, nil
		}
		return t, false, nil
	}
	if strings.Contains(s, "/") {
		t.inPath = true
	}
	t.kind = tSub
	if strings.ContainsAny(s, "*?") {
		t.kind = tGlob
	}
	t.raw = []byte(s)
	t.val = fold(t.raw)
	t.dval = foldCase(t.raw)
	return t, true, nil
}

// ---- size: ----

func parseBytes(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	mult := int64(1)
	for _, u := range []struct {
		s string
		m int64
	}{{"tb", 1 << 40}, {"gb", 1 << 30}, {"mb", 1 << 20}, {"kb", 1 << 10}, {"t", 1 << 40}, {"g", 1 << 30}, {"m", 1 << 20}, {"k", 1 << 10}, {"b", 1}} {
		if strings.HasSuffix(s, u.s) {
			s, mult = strings.TrimSpace(s[:len(s)-len(u.s)]), u.m
			break
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return int64(f * float64(mult)), nil
}

const maxI = int64(1<<62 - 1)

func parseSize(v string) (int64, int64, error) {
	K, M := int64(1<<10), int64(1<<20)
	switch strings.ToLower(v) {
	case "empty":
		return 0, 0, nil
	case "tiny":
		return 0, 10 * K, nil
	case "small":
		return 10*K + 1, 100 * K, nil
	case "medium":
		return 100*K + 1, M, nil
	case "large":
		return M + 1, 16 * M, nil
	case "huge":
		return 16*M + 1, 128 * M, nil
	case "gigantic":
		return 128*M + 1, maxI, nil
	}
	if a, b, ok := strings.Cut(v, ".."); ok {
		lo, err := parseBytes(a)
		if err != nil {
			return 0, 0, err
		}
		hi, err := parseBytes(b)
		return lo, hi, err
	}
	op, rest := splitOp(v)
	x, err := parseBytes(rest)
	if err != nil {
		return 0, 0, err
	}
	switch op {
	case ">":
		return x + 1, maxI, nil
	case ">=":
		return x, maxI, nil
	case "<":
		return 0, x - 1, nil
	case "<=":
		return 0, x, nil
	}
	return x, x, nil
}

func splitOp(v string) (string, string) {
	for _, op := range []string{">=", "<=", ">", "<", "="} {
		if strings.HasPrefix(v, op) {
			return op, v[len(op):]
		}
	}
	return "", v
}

// ---- dm: ----  returns [lo, hi) in unix seconds

func period(s string, now time.Time) (time.Time, time.Time, error) {
	loc := now.Location()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	wd := (int(day.Weekday()) + 6) % 7 // Monday = 0
	week := day.AddDate(0, 0, -wd)
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	year := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, loc)
	ls := strings.ToLower(s)
	switch ls {
	case "today":
		return day, day.AddDate(0, 0, 1), nil
	case "yesterday":
		return day.AddDate(0, 0, -1), day, nil
	case "thisweek":
		return week, week.AddDate(0, 0, 7), nil
	case "lastweek":
		return week.AddDate(0, 0, -7), week, nil
	case "thismonth":
		return month, month.AddDate(0, 1, 0), nil
	case "lastmonth":
		return month.AddDate(0, -1, 0), month, nil
	case "thisyear":
		return year, year.AddDate(1, 0, 0), nil
	case "lastyear":
		return year.AddDate(-1, 0, 0), year, nil
	}
	// lastNdays / lastNweeks / lastNmonths / lastNyears / lastNhours / lastNminutes
	if strings.HasPrefix(ls, "last") || strings.HasPrefix(ls, "past") {
		r := ls[4:]
		i := 0
		for i < len(r) && r[i] >= '0' && r[i] <= '9' {
			i++
		}
		if i > 0 {
			nn, _ := strconv.Atoi(r[:i])
			u := strings.TrimSuffix(r[i:], "s")
			var from time.Time
			switch u {
			case "minute", "min":
				from = now.Add(-time.Duration(nn) * time.Minute)
			case "hour":
				from = now.Add(-time.Duration(nn) * time.Hour)
			case "day":
				from = now.AddDate(0, 0, -nn)
			case "week":
				from = now.AddDate(0, 0, -7*nn)
			case "month":
				from = now.AddDate(0, -nn, 0)
			case "year":
				from = now.AddDate(-nn, 0, 0)
			default:
				return time.Time{}, time.Time{}, fmt.Errorf("bad date %q", s)
			}
			return from, now.Add(time.Hour * 24 * 365 * 100), nil
		}
	}
	s2 := strings.ReplaceAll(s, "/", "-")
	for _, l := range []struct {
		f    string
		next func(time.Time) time.Time
	}{
		{"2006-01-02T15:04", func(t time.Time) time.Time { return t.Add(time.Minute) }},
		{"2006-01-02", func(t time.Time) time.Time { return t.AddDate(0, 0, 1) }},
		{"2006-01", func(t time.Time) time.Time { return t.AddDate(0, 1, 0) }},
		{"2006", func(t time.Time) time.Time { return t.AddDate(1, 0, 0) }},
	} {
		if t, err := time.ParseInLocation(l.f, s2, loc); err == nil {
			return t, l.next(t), nil
		}
	}
	return time.Time{}, time.Time{}, fmt.Errorf("bad date %q (use today, yesterday, thisweek, lastmonth, last3days, 2026, 2026-11, 2026-11-16, a..b, >x)", s)
}

func parseDate(v string, now time.Time) (int64, int64, error) {
	if a, b, ok := strings.Cut(v, ".."); ok {
		lo, _, err := period(a, now)
		if err != nil {
			return 0, 0, err
		}
		_, hi, err := period(b, now)
		return lo.Unix(), hi.Unix(), err
	}
	op, rest := splitOp(v)
	lo, hi, err := period(rest, now)
	if err != nil {
		return 0, 0, err
	}
	switch op {
	case ">":
		return hi.Unix(), maxI, nil
	case ">=":
		return lo.Unix(), maxI, nil
	case "<":
		return 0, lo.Unix(), nil
	case "<=":
		return 0, hi.Unix(), nil
	}
	return lo.Unix(), hi.Unix(), nil
}

// ---- matching helpers ----

// globMatch: '*' any run, '?' exactly one character (UTF-8 aware); whole-string match.
func globMatch(p, s []byte) bool {
	px, sx, nextP, nextS := 0, 0, -1, -1
	for px < len(p) || sx < len(s) {
		if px < len(p) {
			switch c := p[px]; c {
			case '*':
				_, sz := utf8.DecodeRune(s[sx:])
				nextP, nextS = px, sx+sz
				if sx >= len(s) {
					nextS = len(s) + 1
				}
				px++
				continue
			case '?':
				if sx < len(s) {
					_, sz := utf8.DecodeRune(s[sx:])
					px++
					sx += sz
					continue
				}
			default:
				if sx < len(s) && s[sx] == c {
					px++
					sx++
					continue
				}
			}
		}
		if nextS > 0 && nextS <= len(s) {
			px, sx = nextP, nextS
			continue
		}
		return false
	}
	return true
}

func longestLiteral(p []byte) []byte {
	var best []byte
	for _, part := range bytes.FieldsFunc(p, func(r rune) bool { return r == '*' || r == '?' }) {
		if len(part) > len(best) {
			best = part
		}
	}
	return best
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func containsWord(s, w []byte) bool {
	for off := 0; ; {
		k := bytes.Index(s[off:], w)
		if k < 0 {
			return false
		}
		a, b := off+k, off+k+len(w)
		okL, okR := true, true
		if a > 0 {
			r, _ := utf8.DecodeLastRune(s[:a])
			okL = !isWordRune(r)
		}
		if b < len(s) {
			r, _ := utf8.DecodeRune(s[b:])
			okR = !isWordRune(r)
		}
		if okL && okR {
			return true
		}
		off = a + 1
	}
}

func extOf(name []byte) []byte {
	i := bytes.LastIndexByte(name, '.')
	if i < 0 {
		return nil
	}
	return name[i+1:]
}
