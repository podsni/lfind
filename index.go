package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	flagDir  = 1
	flagDead = 2
	flagOrig = 4 // exact name differs from folded name; stored in Orig
	noParent = ^uint32(0)
)

// Index is the compact in-RAM filename index.
//
// The "base" part is sorted by folded name (then by parent path), exactly the
// order Everything shows by default, so search results come out pre-sorted and
// a (parent,name) lookup is a binary search: no hash table is needed.
// Per entry the base costs: parent 4 + name offset 4 + flags 1 (+ size 8 +
// mtime 4 with metadata) bytes, plus the folded name. Exact names are stored
// only for entries whose folded form differs (mixed case, accents).
//
// Entries added by live updates go to a small unsorted "delta" which is
// merged into the base (re-sorted) when the index is saved.
type Index struct {
	P       []uint32
	Off     []uint32
	Fl      []uint8
	Sz      []uint64
	Mt      []uint32
	Names   []byte // folded names, NUL-terminated, in id order
	OrigID  []uint32
	OrigOff []uint32
	Orig    []byte
	Root    uint32
	Meta    bool

	dP     []uint32
	dFl    []uint8
	dSz    []uint64
	dMt    []uint32
	dName  []string // folded
	dExact []string
	dLook  map[string]uint32
	dead   int
	mm     []byte
}

func (ix *Index) nBase() int { return len(ix.P) }
func (ix *Index) Len() int   { return len(ix.P) + len(ix.dP) }
func (ix *Index) Delta() int { return len(ix.dP) }

func (ix *Index) par(id uint32) uint32 {
	if int(id) < len(ix.P) {
		return ix.P[id]
	}
	return ix.dP[int(id)-len(ix.P)]
}
func (ix *Index) fl(id uint32) uint8 {
	if int(id) < len(ix.P) {
		return ix.Fl[id]
	}
	return ix.dFl[int(id)-len(ix.P)]
}
func (ix *Index) orFl(id uint32, f uint8) {
	if int(id) < len(ix.P) {
		ix.Fl[id] |= f
	} else {
		ix.dFl[int(id)-len(ix.P)] |= f
	}
}
func (ix *Index) IsDir(id uint32) bool { return ix.fl(id)&flagDir != 0 }

func (ix *Index) size(id uint32) uint64 {
	if !ix.Meta {
		return 0
	}
	if int(id) < len(ix.P) {
		return ix.Sz[id]
	}
	return ix.dSz[int(id)-len(ix.P)]
}
func (ix *Index) mtime(id uint32) uint32 {
	if !ix.Meta {
		return 0
	}
	if int(id) < len(ix.P) {
		return ix.Mt[id]
	}
	return ix.dMt[int(id)-len(ix.P)]
}
func (ix *Index) setMeta(id uint32, sz uint64, mt uint32) {
	if !ix.Meta {
		return
	}
	if int(id) < len(ix.P) {
		ix.Sz[id], ix.Mt[id] = sz, mt
	} else {
		ix.dSz[int(id)-len(ix.P)], ix.dMt[int(id)-len(ix.P)] = sz, mt
	}
}

func sbytes(s string) []byte { return unsafe.Slice(unsafe.StringData(s), len(s)) }

func (ix *Index) end(i uint32) uint32 {
	if int(i)+1 < len(ix.Off) {
		return ix.Off[i+1] - 1
	}
	return uint32(len(ix.Names)) - 1
}

// fname: folded name (search form).
func (ix *Index) fname(id uint32) []byte {
	if int(id) < len(ix.P) {
		return ix.Names[ix.Off[id]:ix.end(id)]
	}
	return sbytes(ix.dName[int(id)-len(ix.P)])
}

// ename: exact on-disk name.
func (ix *Index) ename(id uint32) []byte {
	if int(id) < len(ix.P) {
		if ix.Fl[id]&flagOrig == 0 {
			return ix.Names[ix.Off[id]:ix.end(id)]
		}
		k := sort.Search(len(ix.OrigID), func(i int) bool { return ix.OrigID[i] >= id })
		s := ix.Orig[ix.OrigOff[k]:]
		return s[:bytes.IndexByte(s, 0)]
	}
	return sbytes(ix.dExact[int(id)-len(ix.P)])
}

func (ix *Index) Alive(id uint32) bool {
	for id != noParent {
		if ix.fl(id)&flagDead != 0 {
			return false
		}
		id = ix.par(id)
	}
	return true
}

func (ix *Index) appendPath(buf []byte, id uint32, exact bool) []byte {
	var st [128]uint32
	s := st[:0]
	for id != noParent {
		s = append(s, id)
		id = ix.par(id)
	}
	nm := ix.fname
	if exact {
		nm = ix.ename
	}
	rn := nm(s[len(s)-1])
	if len(s) == 1 {
		if len(rn) == 0 {
			return append(buf, '/')
		}
		return append(buf, rn...)
	}
	buf = append(buf, rn...)
	for j := len(s) - 2; j >= 0; j-- {
		buf = append(buf, '/')
		buf = append(buf, nm(s[j])...)
	}
	return buf
}

func (ix *Index) Path(id uint32) string { return string(ix.appendPath(nil, id, true)) }

func (ix *Index) RootPath() string {
	if ix.Len() == 0 {
		return "/"
	}
	if r := string(ix.ename(ix.Root)); r != "" {
		return r
	}
	return "/"
}

func skey(p uint32, name []byte) string {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], p)
	return string(b[:]) + string(name)
}

// Lookup finds the live child `name` (exact bytes) of directory p.
func (ix *Index) Lookup(p uint32, name []byte) (uint32, bool) {
	f := fold(name)
	n := ix.nBase()
	i := sort.Search(n, func(i int) bool { return bytes.Compare(ix.Names[ix.Off[i]:ix.end(uint32(i))], f) >= 0 })
	for ; i < n; i++ {
		id := uint32(i)
		if !bytes.Equal(ix.Names[ix.Off[id]:ix.end(id)], f) {
			break
		}
		if ix.P[id] == p && ix.Fl[id]&flagDead == 0 && bytes.Equal(ix.ename(id), name) {
			return id, true
		}
	}
	if id, ok := ix.dLook[skey(p, name)]; ok && ix.fl(id)&flagDead == 0 {
		return id, true
	}
	return 0, false
}

func (ix *Index) Resolve(path string) (uint32, bool) {
	root := ix.RootPath()
	path = filepath.Clean(path)
	var rest string
	switch {
	case root == "/":
		rest = path
	case path == root:
		return ix.Root, true
	case strings.HasPrefix(path, root+"/"):
		rest = path[len(root):]
	default:
		return 0, false
	}
	id := ix.Root
	for _, c := range strings.Split(rest, "/") {
		if c == "" {
			continue
		}
		var ok bool
		if id, ok = ix.Lookup(id, []byte(c)); !ok {
			return 0, false
		}
	}
	return id, true
}

// Add appends a live-update entry to the delta.
func (ix *Index) Add(parent uint32, name []byte, dir bool, sz uint64, mt uint32) uint32 {
	id := uint32(ix.Len())
	var f uint8
	if dir {
		f = flagDir
	}
	ix.dP = append(ix.dP, parent)
	ix.dFl = append(ix.dFl, f)
	if ix.Meta {
		ix.dSz = append(ix.dSz, sz)
		ix.dMt = append(ix.dMt, mt)
	}
	ix.dName = append(ix.dName, string(fold(name)))
	ix.dExact = append(ix.dExact, string(name))
	if ix.dLook == nil {
		ix.dLook = map[string]uint32{}
	}
	ix.dLook[skey(parent, name)] = id
	return id
}

func (ix *Index) Delete(id uint32) {
	if ix.fl(id)&flagDead == 0 {
		ix.orFl(id, flagDead)
		ix.dead++
	}
}

// toFlat exports all live entries (exact names) for a rebuild.
func (ix *Index) toFlat() *flat {
	n := ix.Len()
	state := make([]uint8, n) // 1 alive 2 dead
	var stack []uint32
	for i := 0; i < n; i++ {
		if state[i] != 0 {
			continue
		}
		stack = stack[:0]
		id, res := uint32(i), uint8(1)
		for id != noParent {
			if state[id] != 0 {
				res = state[id]
				break
			}
			stack = append(stack, id)
			if ix.fl(id)&flagDead != 0 {
				res = 2
				break
			}
			id = ix.par(id)
		}
		for j := len(stack) - 1; j >= 0; j-- {
			if ix.fl(stack[j])&flagDead != 0 {
				res = 2
			}
			state[stack[j]] = res
		}
	}
	newID := make([]uint32, n)
	m := 0
	for i := 0; i < n; i++ {
		if state[i] == 1 {
			newID[i] = uint32(m)
			m++
		}
	}
	f := &flat{P: make([]uint32, 0, m), Fl: make([]uint8, 0, m), ROff: make([]uint32, 0, m),
		Raw: make([]byte, 0, len(ix.Names)+len(ix.Orig)), Meta: ix.Meta}
	if ix.Meta {
		f.Sz, f.Mt = make([]uint64, 0, m), make([]uint32, 0, m)
	}
	for i := 0; i < n; i++ {
		if state[i] != 1 {
			continue
		}
		id := uint32(i)
		p := ix.par(id)
		if p != noParent {
			p = newID[p]
		} else {
			f.Root = newID[id]
		}
		f.P = append(f.P, p)
		f.Fl = append(f.Fl, ix.fl(id)&flagDir)
		if ix.Meta {
			f.Sz = append(f.Sz, ix.size(id))
			f.Mt = append(f.Mt, ix.mtime(id))
		}
		f.ROff = append(f.ROff, uint32(len(f.Raw)))
		f.Raw = append(append(f.Raw, ix.ename(id)...), 0)
	}
	return f
}

// Rebuild compacts dead entries and merges the delta into a freshly sorted base.
func (ix *Index) Rebuild() *Index { return finalize(ix.toFlat()) }

// MemBytes is the size of the index data structures.
func (ix *Index) MemBytes() int {
	b := 4*len(ix.P) + 4*len(ix.Off) + len(ix.Fl) + 8*len(ix.Sz) + 4*len(ix.Mt) + len(ix.Names) +
		8*len(ix.OrigID) + len(ix.Orig)
	for i := range ix.dName {
		b += 40 + len(ix.dName[i]) + len(ix.dExact[i]) + 32
	}
	return b
}

func (ix *Index) Stats() string {
	dirs := 0
	for _, f := range ix.Fl {
		dirs += int(f & flagDir)
	}
	for _, f := range ix.dFl {
		dirs += int(f & flagDir)
	}
	n := ix.Len()
	per := 0.0
	if n > 0 {
		per = float64(ix.MemBytes()) / float64(n)
	}
	return fmt.Sprintf("entries=%d dirs=%d files=%d delta=%d dead=%d exact_names=%d meta=%v index_mem=%.1fMB (%.1f B/entry) root=%s",
		n, dirs, n-dirs, ix.Delta(), ix.dead, len(ix.OrigID), ix.Meta, float64(ix.MemBytes())/1e6, per, ix.RootPath())
}

// ---- cache file (v2) ----
// header(64) | Sz u64[n] | P u32[n] | Off u32[n] | Mt u32[n] | OrigID u32[k] | OrigOff u32[k] | Fl u8[n] | Names | Orig

var magic = [8]byte{'L', 'F', 'I', 'N', 'D', 0, 2, 0}

func u32b(s []uint32) []byte {
	if len(s) == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&s[0])), len(s)*4)
}
func u64b(s []uint64) []byte {
	if len(s) == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&s[0])), len(s)*8)
}

func (ix *Index) Save(path string) error {
	c := ix
	if ix.dead > 0 || ix.Delta() > 0 {
		c = ix.Rebuild()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	var h [64]byte
	copy(h[:8], magic[:])
	le := binary.LittleEndian
	le.PutUint64(h[8:], uint64(c.nBase()))
	le.PutUint64(h[16:], uint64(len(c.Names)))
	le.PutUint64(h[24:], uint64(len(c.OrigID)))
	le.PutUint64(h[32:], uint64(len(c.Orig)))
	le.PutUint32(h[40:], c.Root)
	if c.Meta {
		h[44] = 1
	}
	w.Write(h[:])
	w.Write(u64b(c.Sz))
	w.Write(u32b(c.P))
	w.Write(u32b(c.Off))
	w.Write(u32b(c.Mt))
	w.Write(u32b(c.OrigID))
	w.Write(u32b(c.OrigOff))
	w.Write(c.Fl)
	w.Write(c.Names)
	w.Write(c.Orig)
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	return os.Rename(tmp, path)
}

// Load maps the cache file; the index slices point straight into the mapping.
func Load(path string, populate bool) (*Index, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() < 64 {
		return nil, errors.New("cache too small")
	}
	flags := unix.MAP_PRIVATE
	if populate {
		flags |= unix.MAP_POPULATE
	}
	b, err := unix.Mmap(int(f.Fd()), 0, int(st.Size()), unix.PROT_READ|unix.PROT_WRITE, flags)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(b[:8], magic[:]) {
		unix.Munmap(b)
		return nil, fmt.Errorf("%s: unknown cache format (rebuild with 'lfind index')", path)
	}
	le := binary.LittleEndian
	n, nb := int(le.Uint64(b[8:])), int(le.Uint64(b[16:]))
	k, ob := int(le.Uint64(b[24:])), int(le.Uint64(b[32:]))
	ix := &Index{mm: b, Root: le.Uint32(b[40:]), Meta: b[44] == 1}
	ms := 0
	if ix.Meta {
		ms = 12
	}
	if int64(64+n*(9+ms)+8*k+nb+ob) != st.Size() {
		unix.Munmap(b)
		return nil, fmt.Errorf("%s: corrupt cache", path)
	}
	o := 64
	u32 := func(cnt int) []uint32 {
		if cnt == 0 {
			return nil
		}
		s := unsafe.Slice((*uint32)(unsafe.Pointer(&b[o])), cnt)[:cnt:cnt]
		o += 4 * cnt
		return s
	}
	if ix.Meta && n > 0 {
		ix.Sz = unsafe.Slice((*uint64)(unsafe.Pointer(&b[o])), n)[:n:n]
		o += 8 * n
	}
	ix.P = u32(n)
	ix.Off = u32(n)
	if ix.Meta {
		ix.Mt = u32(n)
	}
	ix.OrigID = u32(k)
	ix.OrigOff = u32(k)
	ix.Fl = b[o : o+n : o+n]
	o += n
	ix.Names = b[o : o+nb : o+nb]
	o += nb
	ix.Orig = b[o : o+ob : o+ob]
	return ix, nil
}
