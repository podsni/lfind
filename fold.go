package main

import (
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Folding used for the search arena: full Unicode simple case folding
// (ToLower(ToUpper(r)), so ς→σ, ſ→s, K→k) plus diacritic removal
// (é→e, Å→a; bare combining marks are dropped), matching Everything's
// defaults "Match Case" off and "Match Diacritics" off.

const tabN = 0x3000

var fullTab, caseTab [tabN]rune // -1 = drop
var tabOnce sync.Once

// built lazily: only names/queries with non-ASCII characters need it,
// so plain CLI queries don't pay for it at process start.
func initTabs() {
	for r := rune(0); r < tabN; r++ {
		c := r
		if r >= 0x80 {
			c = unicode.ToLower(unicode.ToUpper(r))
		} else if r >= 'A' && r <= 'Z' {
			c = r + 32
		}
		caseTab[r] = c
		fullTab[r] = c
		if r < 0x80 {
			continue
		}
		if unicode.Is(unicode.Mn, r) {
			fullTab[r] = -1
			continue
		}
		d := norm.NFD.String(string(c))
		base, sz := utf8.DecodeRuneInString(d)
		ok := sz < len(d)
		for _, m := range d[sz:] {
			if !unicode.Is(unicode.Mn, m) {
				ok = false
			}
		}
		if ok {
			fullTab[r] = unicode.ToLower(base)
		}
	}
}

func foldRune(r rune, tab *[tabN]rune) rune {
	if r < tabN {
		tabOnce.Do(initTabs)
		return tab[r]
	}
	return unicode.ToLower(unicode.ToUpper(r))
}

func foldWith(dst, s []byte, tab *[tabN]rune) []byte {
	for i := 0; i < len(s); {
		c := s[i]
		if c < 0x80 {
			if c >= 'A' && c <= 'Z' {
				c += 32
			}
			dst = append(dst, c)
			i++
			continue
		}
		r, sz := utf8.DecodeRune(s[i:])
		i += sz
		if r == utf8.RuneError && sz == 1 {
			dst = append(dst, c) // invalid UTF-8: keep byte
			continue
		}
		if f := foldRune(r, tab); f >= 0 {
			dst = utf8.AppendRune(dst, f)
		}
	}
	return dst
}

// foldInto appends the search form (case + diacritics folded).
func foldInto(dst, s []byte) []byte { return foldWith(dst, s, &fullTab) }

// foldCaseInto folds case only (used by the diacritics: modifier).
func foldCaseInto(dst, s []byte) []byte { return foldWith(dst, s, &caseTab) }

func fold(s []byte) []byte     { return foldInto(nil, s) }
func foldCase(s []byte) []byte { return foldCaseInto(nil, s) }

func isASCIILowerOnly(s []byte) bool {
	for _, c := range s {
		if c >= 0x80 || (c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}
