package app

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/dkroshow/nano/internal/features/subscriptions"
	"github.com/dkroshow/nano/internal/interact"
)

// serve answers HTTP at once and reattaches stored threads behind it, so a
// restart is seconds of closed port rather than minutes. It returns the first
// serving error.
func (a *application) serve(listener net.Listener, localHandler, remoteHandler http.Handler) error {
	cfg, s := a.config, a.server
	serveErrors := make(chan error, 3)
	go func() { serveErrors <- http.Serve(listener, localHandler) }()
	s.restart.MarkHTTPOpen()
	go func() {
		s.attachmentCoordinator().ReattachOnBoot()
		// Features that prompt hosted threads by themselves start after every
		// stored thread is back. HTTP opens first so requests can prioritize them.
		if s.agentExecutions != nil && s.agentExecutions.Control() != nil {
			d := subscriptions.NewDispatcher(s.agentExecutions.Control().CanonicalDatabase(), interact.NewHTTPClient(s.defaults.APIOrigin, nil), s.hostedIdentity().DisplayAgent, log.Printf)
			s.agentController().StartSubscriptions(d)
		}
		if s.heartbeat != nil {
			s.heartbeat.Start(context.Background())
		}
	}()
	if cfg.RemoteAuthPort != 0 {
		if a.remoteAuth.RemoteHost() == "" {
			log.Fatal("-remote-auth-host is required when -remote-auth-port is enabled")
		}
		remoteAddress := fmt.Sprintf("127.0.0.1:%d", cfg.RemoteAuthPort)
		log.Printf("nano protected remote listener on http://%s for host %s", remoteAddress, a.remoteAuth.RemoteHost())
		remoteServer := &http.Server{
			Addr:              remoteAddress,
			Handler:           remoteHandler,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       2 * time.Minute,
		}
		go func() { serveErrors <- remoteServer.ListenAndServe() }()
	}
	if cfg.LocalTLSPort != 0 {
		// The same trusted handler over TLS, so browsers get HTTP/2: one
		// connection carries every pane's stream instead of hitting the
		// six-connection limit browsers keep per HTTP/1.1 host.
		tlsAddress := fmt.Sprintf("127.0.0.1:%d", cfg.LocalTLSPort)
		tlsDir := filepath.Join(cfg.Paths.DataDir, "tls")
		log.Printf("nano trusted local listener on https://%s", tlsAddress)
		tlsServer := &http.Server{Addr: tlsAddress, Handler: localHandler}
		go func() {
			serveErrors <- tlsServer.ListenAndServeTLS(filepath.Join(tlsDir, "localhost.pem"), filepath.Join(tlsDir, "localhost-key.pem"))
		}()
	}
	return <-serveErrors
}
