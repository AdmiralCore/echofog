package selfcover

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strings"
)

// ErrNoContent reports a Content without a folder and without a proxy target. The package never
// invents a default site: thousands of nodes serving one page would be a fingerprint of their own.
var ErrNoContent = errors.New("selfcover: no website content: set a folder or a local application to proxy")

// Content is the website of the cover: the operator's own pages.
type Content struct {
	// Dir is a folder of static files (index.html and the rest of the site).
	Dir string
	// Proxy is the base URL of a local application that serves the site, for example
	// http://127.0.0.1:8080. Exactly one of Dir and Proxy is set.
	Proxy string
}

// handler builds the website handler. warnings lists what the operator should fix without it
// stopping the node: an empty folder serves nothing worth a visitor and is the first thing a
// prober's second request would notice.
func (c Content) handler() (h http.Handler, warnings []string, err error) {
	switch {
	case c.Dir == "" && c.Proxy == "":
		return nil, nil, ErrNoContent
	case c.Dir != "" && c.Proxy != "":
		return nil, nil, errors.New("selfcover: set either a folder or a proxy target, not both")
	case c.Proxy != "":
		u, err := url.Parse(c.Proxy)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, nil, fmt.Errorf("selfcover: %q is not an http(s) URL", c.Proxy)
		}
		return httputil.NewSingleHostReverseProxy(u), nil, nil
	}
	st, err := os.Stat(c.Dir)
	if err != nil {
		return nil, nil, fmt.Errorf("selfcover: website folder: %w", err)
	}
	if !st.IsDir() {
		return nil, nil, fmt.Errorf("selfcover: %q is not a folder", c.Dir)
	}
	files := countFiles(c.Dir)
	if files == 0 {
		warnings = append(warnings, fmt.Sprintf("the website folder %q is empty: visitors and probers will see a site with no pages; put the real site there", c.Dir))
	} else if _, err := os.Stat(path.Join(c.Dir, "index.html")); err != nil {
		warnings = append(warnings, fmt.Sprintf("the website folder %q has no index.html: its home page is a 404", c.Dir))
	}
	return http.FileServer(siteFS{http.Dir(c.Dir)}), warnings, nil
}

func countFiles(dir string) int {
	n := 0
	_ = fs.WalkDir(os.DirFS(dir), ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !hidden(p) {
			n++
		}
		return nil
	})
	return n
}

// hidden reports whether a path has a dot-file or dot-folder segment (.git, .env, ...).
func hidden(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, ".") && seg != "." && seg != ".." {
			return true
		}
	}
	return false
}

// siteFS hides dot-files and folder listings: a site that lists its directories or serves its
// .git is not the usual production website.
type siteFS struct{ http.FileSystem }

func (s siteFS) Open(name string) (http.File, error) {
	if hidden(name) {
		return nil, fs.ErrNotExist
	}
	f, err := s.FileSystem.Open(name)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if st.IsDir() {
		idx, err := s.FileSystem.Open(path.Join(name, "index.html"))
		if err != nil {
			_ = f.Close()
			return nil, fs.ErrNotExist
		}
		_ = idx.Close()
	}
	return f, nil
}
