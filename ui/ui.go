// Package ui serves the built web interface from inside the binary.
//
// Embedding the assets is what makes the container a single artifact: no
// sidecar web server, no volume of static files that can drift out of step with
// the API it talks to.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// buildTime is the modification time reported for the app shell. It is fixed at
// process start rather than taken from the embedded files, whose timestamps
// embed.FS reports as zero.
var buildTime = time.Now()

// dist holds the Vite build output. The all: prefix is required so that files
// beginning with an underscore — which Vite emits — are included.
//
//go:embed all:dist
var dist embed.FS

// Handler serves the single-page app.
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// Only reachable if the embed directive and this path disagree,
		// which is a build-time mistake rather than a runtime condition.
		panic("ui: dist directory is missing from the binary: " + err.Error())
	}

	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")

		if name == "" {
			serveIndex(w, r, sub)
			return
		}
		f, err := sub.Open(name)
		if err != nil {
			// Any unknown path is a client-side route, so the app shell is
			// returned and the router sorts it out.
			serveIndex(w, r, sub)
			return
		}
		info, statErr := f.Stat()
		f.Close()
		if statErr != nil || info.IsDir() {
			serveIndex(w, r, sub)
			return
		}

		// Vite fingerprints everything under /assets, so those are safe to
		// cache indefinitely. The libav build under /libav carries its version
		// in the filename, which amounts to the same guarantee. Nothing else
		// is safe to cache.
		if strings.HasPrefix(name, "assets/") || strings.HasPrefix(name, "libav/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, sub fs.FS) {
	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.Error(w, "the web interface was not built into this binary", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The shell must never be cached, or a deploy leaves browsers loading
	// asset hashes that no longer exist.
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	http.ServeContent(w, r, "index.html", buildTime, strings.NewReader(string(data)))
}
