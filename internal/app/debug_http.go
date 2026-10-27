package app

import (
	"net/http"
	"net/http/pprof"

	"github.com/dkroshow/nano/internal/httpapi/remoteauth"
)

// listenerHandlersWithLoopback adds process diagnostics
// routes to the trusted listener. The protected remote listener receives
// only the application handler.
func listenerHandlersWithLoopback(application http.Handler, remoteGuard func(http.Handler) http.Handler, allowDevOrigin bool) (http.Handler, http.Handler) {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.Handle("/", application)
	return remoteauth.GuardLocal(mux, allowDevOrigin), remoteGuard(application)
}
