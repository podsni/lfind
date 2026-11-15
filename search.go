package main

import (
	"bytes"
	"runtime"
	"slices"
	"sync"
	"time"
)

type Result struct {
	Total   int
	Paths   []string
	Sizes   []uint64
	Mtimes  []int64
	Dirs    []bool
	Elapsed time.Duration
}

// per-goroutine evaluation context with lazily built name/path variants
type ctx struct {
	ix                         *Index
	id                         uint32
	fn                         []byte
	en, fp, ep, dn, dp, pp     []byte
	hasEn, hasFp, hasEp, hasDn bool
	hasDp, hasPp               bool
}

func (c *ctx) reset(id uint32, fn []byte) {
	c.id, c.fn = id, fn
	c.hasEn, c.hasFp, c.hasEp, c.hasDn, c.hasDp, c.hasPp = false, false, false, false, false, false
}
func (c *ctx) ename() []byte {
	if !c.hasEn {
		c.en, c.hasEn = c.ix.ename(c.id), true
	}
	return c.en
}
func (c *ctx) fpath() []byte {
	if !c.hasFp {
		c.fp, c.hasFp = c.ix.appendPath(c.fp[:0], c.id, false), true
	}
	return c.fp
}
func (c *ctx) epath() []byte {
	if !c.hasEp {
		c.ep, c.hasEp = c.ix.appendPath(c.ep[:0], c.id, true), true
	}
	return c.ep
}
func (c *ctx) dname() []byte {
	if !c.hasDn {
		c.dn, c.hasDn = foldCaseInto(c.dn[:0], c.ename()), true
	}
	return c.dn
}
func (c *ctx) dpath() []byte {
	if !c.hasDp {
		c.dp, c.hasDp = foldCaseInto(c.dp[:0], c.epath()), true
	}
	return c.dp
}
func (c *ctx) parentPath() []byte {
	if !c.hasPp {
		p := c.ix.par(c.id)
		c.pp = c.pp[:0]
		if p != noParent {
			c.pp = c.ix.appendPath(c.pp, p, false)
		}
		c.hasPp = true
	}
	return c.pp
}

func (c *ctx) target(t *term) ([]byte, []byte) {
	switch {
	case t.cs:
		if t.inPath {
			return c.epath(), t.raw
		}
		return c.ename(), t.raw
	case t.diac:
		if t.inPath {
			return c.dpath(), t.dval
		}
		return c.dname(), t.dval
	case t.inPath:
		return c.fpath(), t.val
	}
	return c.fn, t.val
}

func (c *ctx) match(t *term) bool {
	ix := c.ix
	if t.only != 0 {
		dir := ix.fl(c.id)&flagDir != 0
		if (t.only == 1) == dir {
			return false
		}
	}
	switch t.kind {
	case tAll:
		return true
	case tSub:
		s, v := c.target(t)
		if t.ww {
			return containsWord(s, v)
		}
		return bytes.Contains(s, v)
	case tGlob:
		s, v := c.target(t)
		return globMatch(v, s)
	case tExt:
		n := c.fn
		if t.cs {
			n = c.ename()
		}
		e := extOf(n)
		if e == nil {
			return false
		}
		for _, x := range t.exts {
			if bytes.Equal(e, x) {
				return true
			}
		}
		return false
	case tRegex:
		// case-insensitive regexes run on the folded form (also diacritic-insensitive,
		// like Everything's defaults); case: regexes see the exact name
		if t.inPath {
			if t.cs {
				return t.re.Match(c.epath())
			}
			return t.re.Match(c.fpath())
		}
		if t.cs {
			return t.re.Match(c.ename())
		}
		return t.re.Match(c.fn)
	case tSize:
		if ix.fl(c.id)&flagDir != 0 || !ix.Meta {
			return false
		}
		s := int64(ix.size(c.id))
		return s >= t.lo && s <= t.hi
	case tDate:
		if !ix.Meta {
			return false
		}
		m := int64(ix.mtime(c.id))
		return m >= t.lo && m < t.hi
	case tParent:
		return bytes.Equal(c.parentPath(), t.val)
	}
	return false
}

func (c *ctx) check(groups [][]term) bool {
	for gi := range groups {
		g := groups[gi]
		ok := false
		for ti := range g {
			if c.match(&g[ti]) != g[ti].neg {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func (ix *Index) Search(q Query) (Result, error) {
	t0 := time.Now()
	groups, err := parseQuery(q)
	if err != nil {
		return Result{}, err
	}
	// driver literal for the SIMD arena sweep (must be implied by every match)
	var driver []byte
	for _, g := range groups {
		if len(g) != 1 || g[0].neg || g[0].inPath {
			continue
		}
		t := &g[0]
		var lit []byte
		switch t.kind {
		case tSub:
			lit = t.val
		case tGlob:
			lit = longestLiteral(t.val)
		case tRegex:
			lit = t.lit
		case tExt:
			if len(t.exts) == 1 && !t.cs {
				lit = append([]byte{'.'}, t.exts[0]...)
			}
		}
		if len(lit) > len(driver) {
			driver = lit
		}
	}
	// a plain single substring needs no re-check: the arena hit is the match
	simple := len(groups) == 1 && len(groups[0]) == 1 && ix.dead == 0
	if simple {
		t := &groups[0][0]
		simple = t.kind == tSub && !t.neg && !t.cs && !t.diac && !t.ww && !t.inPath && t.only == 0
	}
	sortMode := q.Sort
	if sortMode == "" {
		sortMode = "name"
	}
	needAll := sortMode != "name" || q.Desc
	limit := q.Limit
	keep := func(cnt int) bool { return !q.Count && (needAll || limit <= 0 || cnt < limit) }

	n := ix.nBase()
	P := runtime.GOMAXPROCS(0)
	nch := P * 4
	if n < 1<<14 {
		nch = 1
	}
	type chunkRes struct {
		count int
		ids   []uint32
	}
	res := make([]chunkRes, nch)
	rootEmpty := len(ix.fname(ix.Root)) == 0
	var wg sync.WaitGroup
	for c := 0; c < nch; c++ {
		a, b := c*n/nch, (c+1)*n/nch
		if a >= b {
			continue
		}
		wg.Add(1)
		go func(c, a, b int) {
			defer wg.Done()
			cr := &res[c]
			cx := &ctx{ix: ix}
			test := func(id uint32) {
				if simple {
					cr.count++
					if keep(len(cr.ids)) {
						cr.ids = append(cr.ids, id)
					}
					return
				}
				if ix.Fl[id]&flagDead != 0 || (id == ix.Root && rootEmpty) {
					return
				}
				cx.reset(id, ix.Names[ix.Off[id]:ix.end(id)])
				if !cx.check(groups) {
					return
				}
				if ix.dead > 0 && !ix.Alive(id) {
					return
				}
				cr.count++
				if keep(len(cr.ids)) {
					cr.ids = append(cr.ids, id)
				}
			}
			if len(driver) == 0 {
				for id := a; id < b; id++ {
					test(uint32(id))
				}
				return
			}
			arena := ix.Names
			hi := len(arena)
			if b < n {
				hi = int(ix.Off[b])
			}
			off := ix.Off
			pos := int(off[a])
			cur := a // galloping cursor: hits arrive in increasing order
			for pos < hi {
				k := bytes.Index(arena[pos:hi], driver)
				if k < 0 {
					break
				}
				p := uint32(pos + k)
				step := 1
				for cur+step < b && off[cur+step] <= p {
					cur += step
					step *= 2
				}
				for step > 1 {
					step /= 2
					if cur+step < b && off[cur+step] <= p {
						cur += step
					}
				}
				test(uint32(cur))
				pos = int(ix.end(uint32(cur))) + 1
				cur++
			}
		}(c, a, b)
	}
	// delta (live additions since last rebuild), unsorted: scan directly
	var dIDs []uint32
	dCount := 0
	if ix.Delta() > 0 {
		cx := &ctx{ix: ix}
		for i := n; i < ix.Len(); i++ {
			id := uint32(i)
			if ix.fl(id)&flagDead != 0 {
				continue
			}
			cx.reset(id, ix.fname(id))
			if (driver != nil && !bytes.Contains(cx.fn, driver)) || !cx.check(groups) || !ix.Alive(id) {
				continue
			}
			dCount++
			if !q.Count {
				dIDs = append(dIDs, id)
			}
		}
	}
	wg.Wait()
	r := Result{Total: dCount}
	var ids []uint32
	for _, cr := range res {
		r.Total += cr.count
		if keep(len(ids)) {
			ids = append(ids, cr.ids...)
		}
	}
	if !q.Count {
		if needAll {
			ids = append(ids, dIDs...)
			if sortMode == "name" {
				ix.sortIDs(ids, "name")
				slices.Reverse(ids)
			} else {
				ids = ix.sortTop(ids, sortMode, q.Desc, limit)
			}
		} else if len(dIDs) > 0 {
			if limit > 0 && len(ids) > limit {
				ids = ids[:limit]
			}
			ix.sortIDs(dIDs, "name")
			ids = ix.mergeByName(ids, dIDs)
		}
		if limit > 0 && len(ids) > limit {
			ids = ids[:limit]
		}
		r.Paths = make([]string, len(ids))
		var buf []byte
		for i, id := range ids {
			buf = ix.appendPath(buf[:0], id, true)
			r.Paths[i] = string(buf)
		}
		if q.Long {
			r.Sizes, r.Mtimes, r.Dirs = make([]uint64, len(ids)), make([]int64, len(ids)), make([]bool, len(ids))
			for i, id := range ids {
				r.Sizes[i], r.Mtimes[i], r.Dirs[i] = ix.size(id), int64(ix.mtime(id)), ix.IsDir(id)
			}
		}
	}
	r.Elapsed = time.Since(t0)
	return r, nil
}

func (ix *Index) nameLess(a, b uint32) int {
	if c := bytes.Compare(ix.fname(a), ix.fname(b)); c != 0 {
		return c
	}
	return bytes.Compare(ix.appendPath(nil, a, false), ix.appendPath(nil, b, false))
}

func (ix *Index) sortIDs(ids []uint32, mode string) {
	// name: base ids are already in name order; delta ids need a real compare
	if len(ids) > 0 && int(ids[len(ids)-1]) >= ix.nBase() {
		slices.SortStableFunc(ids, ix.nameLess)
	}
}

type kv struct {
	k  uint64
	id uint32
}

// tie: name order (base ids are name-ordered already)
func (ix *Index) tie(a, b uint32) int {
	nb := uint32(ix.nBase())
	if a >= nb || b >= nb {
		if c := bytes.Compare(ix.fname(a), ix.fname(b)); c != 0 {
			return c
		}
	}
	if a < b {
		return -1
	} else if a > b {
		return 1
	}
	return 0
}

// sortTop orders ids by size, date or path (name as tie-break) and returns the
// first `limit`, using a bounded heap when only a page of results is needed.
func (ix *Index) sortTop(ids []uint32, mode string, desc bool, limit int) []uint32 {
	items := make([]kv, len(ids))
	switch mode {
	case "size":
		for i, id := range ids {
			items[i] = kv{ix.size(id), id}
		}
	case "dm", "date":
		for i, id := range ids {
			items[i] = kv{uint64(ix.mtime(id)), id}
		}
	case "path":
		// rank the distinct parent folders by path ('/' sorts first), then (parent, name)
		pidx := map[uint32]int{}
		var parents []uint32
		for _, id := range ids {
			p := ix.par(id)
			if _, ok := pidx[p]; !ok {
				pidx[p] = 0
				parents = append(parents, p)
			}
		}
		keys := make([][]byte, len(parents))
		for i, p := range parents {
			if p != noParent {
				k := ix.appendPath(nil, p, false)
				for j := range k {
					if k[j] == '/' {
						k[j] = 0
					}
				}
				keys[i] = k
			}
		}
		ord := make([]int, len(parents))
		for i := range ord {
			ord[i] = i
		}
		slices.SortFunc(ord, func(a, b int) int { return bytes.Compare(keys[a], keys[b]) })
		for r, i := range ord {
			pidx[parents[i]] = r
		}
		for i, id := range ids {
			items[i] = kv{uint64(pidx[ix.par(id)]), id}
		}
	default:
		return ids
	}
	if desc {
		for i := range items {
			items[i].k = ^items[i].k
		}
	}
	cmp := func(x, y kv) int {
		if x.k != y.k {
			if x.k < y.k {
				return -1
			}
			return 1
		}
		return ix.tie(x.id, y.id)
	}
	if limit > 0 && limit*8 < len(items) {
		// bounded max-heap of the best `limit` items
		h := make([]kv, 0, limit)
		down := func(i int) {
			for {
				l, m := 2*i+1, i
				if l < len(h) && cmp(h[l], h[m]) > 0 {
					m = l
				}
				if r := l + 1; r < len(h) && cmp(h[r], h[m]) > 0 {
					m = r
				}
				if m == i {
					return
				}
				h[i], h[m] = h[m], h[i]
				i = m
			}
		}
		for _, it := range items {
			if len(h) < limit {
				h = append(h, it)
				for i := len(h) - 1; i > 0; {
					p := (i - 1) / 2
					if cmp(h[i], h[p]) <= 0 {
						break
					}
					h[i], h[p] = h[p], h[i]
					i = p
				}
			} else if cmp(it, h[0]) < 0 {
				h[0] = it
				down(0)
			}
		}
		items = h
	}
	slices.SortFunc(items, cmp)
	out := ids[:0]
	for _, it := range items {
		out = append(out, it.id)
	}
	return out
}

func (ix *Index) mergeByName(a, b []uint32) []uint32 {
	out := make([]uint32, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if ix.nameLess(a[i], b[j]) <= 0 {
			out = append(out, a[i])
			i++
		} else {
			out = append(out, b[j])
			j++
		}
	}
	out = append(out, a[i:]...)
	return append(out, b[j:]...)
}
