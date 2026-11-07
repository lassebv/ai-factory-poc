package metainfo

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// reserved lists the Windows device names, compared case-insensitively
// against the part of a component before its first dot.
var reserved = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com0": true, "com1": true, "com2": true, "com3": true, "com4": true,
	"com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
	"lpt0": true, "lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true,
	"lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// sanitize makes a validated path component safe on every platform (A3):
// invalid UTF-8, control characters and <>:"|?* become "_", trailing dots
// and spaces become "_", and a reserved device name gets "_" appended to
// its stem ("CON" → "CON_", "nul.txt" → "nul_.txt").
func sanitize(c string) string {
	if !utf8.ValidString(c) {
		c = strings.ToValidUTF8(c, "_")
	}
	b := []byte(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"|?*`, r) {
			return '_'
		}
		return r
	}, c))
	for i := len(b) - 1; i >= 0 && (b[i] == '.' || b[i] == ' '); i-- {
		b[i] = '_'
	}
	c = string(b)
	stem, ext, hasExt := strings.Cut(c, ".")
	if reserved[strings.ToLower(stem)] {
		c = stem + "_"
		if hasExt {
			c += "." + ext
		}
	}
	return c
}

// foldKey is the key used to detect duplicate paths: NFC-normalized and
// case-folded (A3).
func foldKey(p string) string {
	return cases.Fold().String(norm.NFC.String(p))
}
