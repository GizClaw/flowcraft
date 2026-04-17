package api

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}

	if s.config.WebFS != nil {
		ServeSPAFromFS(s.config.WebFS).ServeHTTP(w, r)
		return
	}

	webDir := s.config.WebDir
	if webDir == "" {
		webDir = "web/dist"
	}

	fpath := filepath.Join(webDir, filepath.Clean(r.URL.Path))
	if info, err := os.Stat(fpath); err == nil && !info.IsDir() {
		http.ServeFile(w, r, fpath)
		return
	}

	indexPath := filepath.Join(webDir, "index.html")
	if _, err := os.Stat(indexPath); err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, indexPath)
}

// ServeSPAFromFS creates a handler for embedded filesystem SPA.
func ServeSPAFromFS(fsys fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := fs.Stat(fsys, path); err != nil {
			path = "index.html"
		}
		http.ServeFileFS(w, r, fsys, path)
	})
}
