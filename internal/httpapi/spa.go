// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// SPAHandler serves the built single-page application from an fs.FS.
//
// Hashed assets under /assets are immutable and cached aggressively; every
// other path falls back to index.html so client-side routing works on reload.
func SPAHandler(dist fs.FS) http.Handler {
	files := http.FileServer(http.FS(dist))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if upath == "" {
			upath = "index.html"
		}

		if f, err := dist.Open(upath); err == nil {
			f.Close()
			if strings.HasPrefix(upath, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			files.ServeHTTP(w, r)
			return
		}

		// Unknown path: hand the route to the SPA router.
		index, err := dist.Open("index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		index.Close()
		w.Header().Set("Cache-Control", "no-cache")
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		files.ServeHTTP(w, r2)
	})
}
