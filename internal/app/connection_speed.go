package app

import (
	"math/rand/v2"
	"net/http"
	"strconv"
)

// connectionTestPayload is what the browser's connection indicator downloads
// to measure throughput between it and Nano. Pseudo-random bytes so no
// compressing hop shrinks them; made once, and the request touches no store
// or session, so a sample costs Nano nothing but the write.
var connectionTestPayload = func() []byte {
	payload := make([]byte, 64<<10)
	source := rand.NewChaCha8([32]byte{'n', 'a', 'n', 'o'})
	_, _ = source.Read(payload)
	return payload
}()

func handleConnectionTest(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(connectionTestPayload)))
	_, _ = w.Write(connectionTestPayload)
}
