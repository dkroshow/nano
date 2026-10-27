package httpapi

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// CompressResponses gzips text, JSON and event streams for clients that
// accept gzip, on every listener. JSON compresses about tenfold, which is
// most of what a page moves over a slow remote link. A stream stays live:
// each Flush sends the compressed bytes written so far.
func CompressResponses(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Either encoding may answer this URL, so a cache must key on it.
		w.Header().Add("Vary", "Accept-Encoding")
		// A Range answer covers bytes of the identity body; leave it alone.
		if r.Method == http.MethodHead || r.Header.Get("Range") != "" || !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		cw := &compressWriter{ResponseWriter: w}
		defer cw.close()
		next.ServeHTTP(cw, r)
	})
}

func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(coding), "gzip") {
			continue
		}
		q, weighted := strings.CutPrefix(strings.TrimSpace(params), "q=")
		if !weighted {
			return true
		}
		weight, err := strconv.ParseFloat(strings.TrimSpace(q), 64)
		return err == nil && weight > 0
	}
	return false
}

func compressible(contentType string) bool {
	media, _, _ := strings.Cut(contentType, ";")
	media = strings.TrimSpace(strings.ToLower(media))
	return strings.HasPrefix(media, "text/") || media == "application/json" ||
		media == "application/javascript" || media == "image/svg+xml"
}

var gzipWriters = sync.Pool{New: func() any {
	gz, _ := gzip.NewWriterLevel(nil, gzip.DefaultCompression)
	return gz
}}

// compressWriter decides at the status line whether to compress, from the
// handler's own headers, and sniffs the type first when the handler set none.
type compressWriter struct {
	http.ResponseWriter
	wroteHeader bool
	gz          *gzip.Writer
}

func (w *compressWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	if status < http.StatusOK {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.wroteHeader = true
	header := w.Header()
	if status != http.StatusNoContent && status != http.StatusNotModified && status != http.StatusPartialContent &&
		header.Get("Content-Encoding") == "" && compressible(header.Get("Content-Type")) {
		header.Del("Content-Length")
		header.Set("Content-Encoding", "gzip")
		w.gz = gzipWriters.Get().(*gzip.Writer)
		w.gz.Reset(w.ResponseWriter)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *compressWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", http.DetectContentType(p))
		}
		w.WriteHeader(http.StatusOK)
	}
	if w.gz != nil {
		return w.gz.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

func (w *compressWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.gz != nil {
		_ = w.gz.Flush()
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap lets http.ResponseController reach the connection for deadlines.
func (w *compressWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *compressWriter) close() {
	if w.gz == nil {
		return
	}
	_ = w.gz.Close()
	w.gz.Reset(nil)
	gzipWriters.Put(w.gz)
	w.gz = nil
}
