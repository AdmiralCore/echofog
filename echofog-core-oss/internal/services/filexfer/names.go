package filexfer

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxNameBytes keeps names well below every file system's limit, with room for " (99)" and the
// partial-download suffix.
const maxNameBytes = 180

// SafeName turns the name a peer sent into a plain file name that cannot leave the download folder:
// no directories (either kind of slash), no "." or "..", no characters Windows refuses, no control
// characters or bidirectional overrides (which can disguise "exe.txt" as "txt.exe"), no device names,
// no trailing dots or spaces. What is left is never empty.
func SafeName(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == utf8.RuneError, r < 0x20, r == 0x7f:
		case strings.ContainsRune(`<>:"|?*`, r):
			b.WriteRune('_')
		case unicode.Is(unicode.Bidi_Control, r), r == 0x200b, r == 0xfeff:
		default:
			b.WriteRune(r)
		}
	}
	name = strings.TrimRight(strings.TrimSpace(b.String()), ". ")
	name = strings.TrimLeft(name, " ")
	if name == "" || strings.Trim(name, ".") == "" {
		return "file"
	}
	stem, ext := splitExt(name)
	if reservedDevice(stem) {
		stem = "_" + stem
	}
	if len(ext) > 32 {
		ext = ""
	}
	for len(stem)+len(ext) > maxNameBytes {
		_, size := utf8.DecodeLastRuneInString(stem)
		stem = stem[:len(stem)-size]
	}
	if stem == "" {
		stem = "file"
	}
	return stem + ext
}

func splitExt(name string) (stem, ext string) {
	i := strings.LastIndexByte(name, '.')
	if i <= 0 {
		return name, ""
	}
	return name[:i], name[i:]
}

// reservedDevice: names Windows maps to devices whatever their extension ("CON", "nul.txt").
func reservedDevice(stem string) bool {
	s := strings.ToUpper(strings.TrimRight(stem, " "))
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	switch s {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if len(s) == 4 && (strings.HasPrefix(s, "COM") || strings.HasPrefix(s, "LPT")) && s[3] >= '0' && s[3] <= '9' {
		return true
	}
	return false
}

// inside reports whether path lies within dir.
func inside(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// freeName finds a name in dir no file has yet: "a.txt", "a (1).txt", "a (2).txt" ...
func freeName(dir, name string) (string, error) {
	stem, ext := splitExt(name)
	for k := 0; k < 1000; k++ {
		cand := name
		if k > 0 {
			cand = stem + " (" + strconv.Itoa(k) + ")" + ext
		}
		p := filepath.Join(dir, cand)
		if !inside(dir, p) {
			return "", errors.New("unsafe file name")
		}
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			return p, nil
		}
	}
	return "", errors.New("no free file name")
}
