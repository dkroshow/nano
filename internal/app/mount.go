package app

import (
	"io/fs"
	"log"
	"net/http"
	"strings"

	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/web"
)

func (a *application) mount() (http.Handler, http.Handler) {
	mux := http.NewServeMux()
	a.remoteAuth.RegisterRoutes(mux)
	mountRoutes(mux, applicationRoutes(a.server))
	dist, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		log.Fatal(err)
	}
	// Cache discipline for the embedded SPA: the shell (index.html) must
	// revalidate on every load — the embed FS carries no modtimes, so without
	// this a browser can heuristically pin a stale bundle across deploys and
	// run old code indefinitely. Hash-named assets are immutable by
	// construction and cache forever.
	fileServer := http.FileServer(http.FS(dist))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	}))
	return listenerHandlersWithLoopback(httpapi.CompressResponses(a.server.refuseWritesWhileStopping(mux)), a.remoteAuth.Guard)
}
