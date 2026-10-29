package app

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func (a *application) run(localHandler, remoteHandler http.Handler) {
	cfg, s := a.config, a.server
	stopSignals := make(chan os.Signal, 1)
	signal.Notify(stopSignals, syscall.SIGTERM, os.Interrupt)
	go func() {
		<-stopSignals
		started := time.Now()
		report := s.stop(stopDeliveryWait, stopDeadline)
		s.restart.WriteUnsettled("interrupted", true)
		log.Printf("nano stopped in %s: %s", time.Since(started).Round(time.Millisecond), report)
		os.Exit(0)
	}()
	listenAddress := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	log.Printf("nano listening on http://%s (default cwd %s, codex daemon %s, claude shim %s, grok %s)",
		listenAddress, a.absCwd, cfg.CodexWS, s.defaults.ClaudeShimWS, s.defaults.GrokBin)
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		log.Fatal(err)
	}
	serveErr := a.serve(listener, localHandler, remoteHandler)
	if s.heartbeat != nil {
		s.heartbeat.Close()
		if err := s.heartbeat.Store.Close(); err != nil {
			log.Printf("close heartbeat store: %v", err)
		}
	}
	if s.mail != nil {
		if err := s.mail.Close(); err != nil {
			log.Printf("close mail store: %v", err)
		}
	}
	if a.catalogService != nil {
		a.catalogService.Close()
	}
	if a.catalogStore != nil {
		if err := a.catalogStore.Close(); err != nil {
			log.Printf("close thread catalog: %v", err)
		}
	}
	if err := a.remoteAuthStore.Close(); err != nil {
		log.Printf("close remote authentication store: %v", err)
	}
	if a.serverLog != nil {
		_ = a.serverLog.Close()
	}
	log.Fatal(serveErr)
}
