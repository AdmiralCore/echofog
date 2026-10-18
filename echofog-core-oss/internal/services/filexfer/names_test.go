package filexfer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSafeNameKeepsFilesInTheFolder(t *testing.T) {
	cases := map[string]string{
		"report.pdf":                   "report.pdf",
		"../../etc/passwd":             "passwd",
		`..\..\Windows\System32\x.dll`: "x.dll",
		"/abs/path/file.txt":           "file.txt",
		`C:\Users\a\b.txt`:             "b.txt",
		"..":                           "file",
		".":                            "file",
		"":                             "file",
		"   ":                          "file",
		"a<b>c:d\"e|f?g*h.txt":         "a_b_c_d_e_f_g_h.txt",
		"name. . .":                    "name",
		"CON":                          "_CON",
		"nul.txt":                      "_nul.txt",
		"com1.log":                     "_com1.log",
		"COM10.log":                    "COM10.log",
		"tab\there\x00.txt":            "tabhere.txt",
		"evil\u202etxt.exe":            "eviltxt.exe",
		"Привет мир.docx":              "Привет мир.docx",
	}
	for in, want := range cases {
		assert.Equal(t, want, SafeName(in), "SafeName(%q)", in)
	}
	long := strings.Repeat("я", 300) + ".tar.gz"
	got := SafeName(long)
	assert.LessOrEqual(t, len(got), maxNameBytes)
	assert.True(t, strings.HasSuffix(got, ".gz"))
	assert.True(t, strings.ToValidUTF8(got, "") == got)
}

func TestFreeNameNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	p, err := freeName(dir, "a.txt")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "a.txt"), p)
	require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
	p2, err := freeName(dir, "a.txt")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "a (1).txt"), p2)

	assert.True(t, inside(dir, filepath.Join(dir, "x")))
	assert.False(t, inside(dir, filepath.Join(dir, "..", "x")))
	assert.False(t, inside(dir, dir))
}
