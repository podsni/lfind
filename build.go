package main

import (
	"fmt"
	"os"
	"time"
	"bytes"
	"encoding/binary"
	"runtime"
	"slices"
	"sync"
)

func parallelFor(n int, fn func(a, b int)) {
	P := runtime.GOMAXPROCS(0)
	nch := P * 4
	if n < 1<<14 {
		nch = 1
	}
	var wg sync.WaitGroup
	for c := 0; c < nch; c++ {
		a, b := c*n/nch, (c+1)*n/nch
		if a >= b {
			continue
		}
		wg.Add(1)
		go func() { defer wg.Done(); fn(a, b) }()
	}
	wg.Wait()
}

// finalize folds names, sorts entries by (folded name, parent path, exact name)
// and lays out the final compact index.
func finalize(f *flat) *Index {
	n := len(f.P)
	P := runtime.GOMAXPROCS(0)
	tl := newTimer()

	// 1. fold names in parallel chunks
	type chunk struct {
		a, b  int
		arena []byte
		offs  []uint32
		orig  []bool
	}
	nch := P * 4
	if n < 1<<14 {
		nch = 1
	}
	chunks := make([]chunk, nch)
	var wg sync.WaitGroup
	for c := 0; c < nch; c++ {
		ch := &chunks[c]
		ch.a, ch.b = c*n/nch, (c+1)*n/nch
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := ch.b - ch.a
			ch.offs = make([]uint32, m)
			ch.orig = make([]bool, m)
			for i := ch.a; i < ch.b; i++ {
				r := f.raw(uint32(i))
				ch.offs[i-ch.a] = uint32(len(ch.arena))
				if isASCIILowerOnly(r) {
					ch.arena = append(ch.arena, r...)
				} else {
					s := len(ch.arena)
					ch.arena = foldInto(ch.arena, r)
					ch.orig[i-ch.a] = !bytes.Equal(ch.arena[s:], r)
				}
				ch.arena = append(ch.arena, 0)
			}
		}()
	}
	wg.Wait()
	foff := make([]uint32, n+1)
	isOrig := make([]bool, n)
	tot := 0
	for c := range chunks {
		tot += len(chunks[c].arena)
	}
	farena := make([]byte, 0, tot)
	for c := range chunks {
		ch := &chunks[c]
		base := uint32(len(farena))
		for k, o := range ch.offs {
			foff[ch.a+k] = base + o
		}
		copy(isOrig[ch.a:], ch.orig)
		farena = append(farena, ch.arena...)
		chunks[c] = chunk{}
	}
	foff[n] = uint32(len(farena))
	fn := func(i uint32) []byte { return farena[foff[i] : foff[i+1]-1] }

	tl.mark("fold")
	// 2. rank directories by folded full path (tie-break for equal names)
	rank := make([]uint32, n)
	var dirs []uint32
	for i := 0; i < n; i++ {
		if f.Fl[i]&flagDir != 0 {
			dirs = append(dirs, uint32(i))
		}
	}
	dpath := make([][]byte, len(dirs))
	parallelFor(len(dirs), func(a, b int) {
		var st []uint32
		for k := a; k < b; k++ {
			st = st[:0]
			for id := dirs[k]; id != noParent; id = f.P[id] {
				st = append(st, id)
			}
			var p []byte
			for j := len(st) - 1; j >= 0; j-- {
				p = append(append(p, fn(st[j])...), 0) // 0 separator sorts before any char
			}
			dpath[k] = p
		}
	})
	dord := make([]int32, len(dirs))
	for i := range dord {
		dord[i] = int32(i)
	}
	slices.SortFunc(dord, func(a, b int32) int { return bytes.Compare(dpath[a], dpath[b]) })
	for r, k := range dord {
		rank[dirs[k]] = uint32(r)
	}
	dpath = nil

	tl.mark("dirrank")
	// 3. sort (16-byte name prefix keys inline; full compare only on ties), parallel runs + merges
	type skey struct {
		a, b uint64
		id   uint32
	}
	keys := make([]skey, n)
	parallelFor(n, func(lo, hi int) {
		var kb [16]byte
		for i := lo; i < hi; i++ {
			kb = [16]byte{}
			copy(kb[:], fn(uint32(i)))
			keys[i] = skey{binary.BigEndian.Uint64(kb[:8]), binary.BigEndian.Uint64(kb[8:]), uint32(i)}
		}
	})
	prank := func(i uint32) uint32 {
		if p := f.P[i]; p != noParent {
			return rank[p]
		}
		return 0
	}
	cmp := func(x, y skey) int {
		if x.a != y.a {
			if x.a < y.a {
				return -1
			}
			return 1
		}
		if x.b != y.b {
			if x.b < y.b {
				return -1
			}
			return 1
		}
		a, b := x.id, y.id
		if c := bytes.Compare(fn(a), fn(b)); c != 0 {
			return c
		}
		if ra, rb := prank(a), prank(b); ra != rb {
			if ra < rb {
				return -1
			}
			return 1
		}
		if c := bytes.Compare(f.raw(a), f.raw(b)); c != 0 {
			return c
		}
		if a < b {
			return -1
		}
		return 1
	}
	runs := P
	if n < 1<<14 {
		runs = 1
	}
	bounds := make([]int, runs+1)
	for r := 0; r <= runs; r++ {
		bounds[r] = r * n / runs
	}
	var wg2 sync.WaitGroup
	for r := 0; r < runs; r++ {
		wg2.Add(1)
		go func(a, b int) { defer wg2.Done(); slices.SortFunc(keys[a:b], cmp) }(bounds[r], bounds[r+1])
	}
	wg2.Wait()
	tmp := make([]skey, n)
	for len(bounds) > 2 {
		var nb []int
		var wg3 sync.WaitGroup
		for r := 0; r+1 < len(bounds); r += 2 {
			a, m := bounds[r], bounds[r+1]
			b := m
			if r+2 < len(bounds) {
				b = bounds[r+2]
			}
			nb = append(nb, a)
			wg3.Add(1)
			go func(a, m, b int) {
				defer wg3.Done()
				i, j, k := a, m, a
				for i < m && j < b {
					if cmp(keys[i], keys[j]) <= 0 {
						tmp[k] = keys[i]
						i++
					} else {
						tmp[k] = keys[j]
						j++
					}
					k++
				}
				k += copy(tmp[k:], keys[i:m])
				copy(tmp[k:], keys[j:b])
			}(a, m, b)
		}
		nb = append(nb, n)
		wg3.Wait()
		keys, tmp = tmp, keys
		bounds = nb
	}
	tmp = nil
	perm := make([]uint32, n)
	for i := range keys {
		perm[i] = keys[i].id
	}
	keys = nil
	tl.mark("sort")

	// 4. emit in sorted order
	newID := rank // reuse
	for i, old := range perm {
		newID[old] = uint32(i)
	}
	ix := &Index{P: make([]uint32, n), Off: make([]uint32, n), Fl: make([]uint8, n), Names: make([]byte, len(farena)), Meta: f.Meta}
	if f.Meta {
		ix.Sz, ix.Mt = make([]uint64, n), make([]uint32, n)
	}
	// offsets need a prefix sum over sorted order
	pos := uint32(0)
	for i, old := range perm {
		ix.Off[i] = pos
		pos += foff[old+1] - foff[old]
	}
	parallelFor(n, func(a, b int) {
		for i := a; i < b; i++ {
			old := perm[i]
			p := f.P[old]
			if p != noParent {
				p = newID[p]
			}
			ix.P[i] = p
			fl := f.Fl[old] & flagDir
			if isOrig[old] {
				fl |= flagOrig
			}
			ix.Fl[i] = fl
			if f.Meta {
				ix.Sz[i], ix.Mt[i] = f.Sz[old], f.Mt[old]
			}
			copy(ix.Names[ix.Off[i]:], farena[foff[old]:foff[old+1]])
		}
	})
	for i, old := range perm {
		if isOrig[old] {
			ix.OrigID = append(ix.OrigID, uint32(i))
			ix.OrigOff = append(ix.OrigOff, uint32(len(ix.Orig)))
			ix.Orig = append(append(ix.Orig, f.raw(old)...), 0)
		}
	}
	ix.Root = newID[f.Root]
	tl.mark("emit")
	return ix
}

type timer struct {
	on bool
	t  time.Time
}

func newTimer() *timer { return &timer{on: os.Getenv("LFIND_DEBUG") != "", t: time.Now()} }
func (t *timer) mark(s string) {
	if t.on {
		fmt.Fprintf(os.Stderr, "  %s: %v\n", s, time.Since(t.t).Round(time.Millisecond))
		t.t = time.Now()
	}
}
