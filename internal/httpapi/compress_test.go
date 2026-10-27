package httpapi

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Catches the two ways compression breaks a page: an event stream whose
// events wait inside the compressor until the handler ends (the page stalls),
// and a response that is not compressed, or is compressed for a client that
// did not ask for it.
func TestCompressResponsesKeepsStreamsLiveAndHonorsAcceptEncoding(t *testing.T) {
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := fmt.Sprintf(`{"rows":"%s"}`, strings.Repeat("row ", 1000))
		w.Header().Set("Content-Length", fmt.Sprint(len(body))) // the identity length
		fmt.Fprint(w, body)
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, "data: second\n\n")
	})
	server := httptest.NewServer(CompressResponses(mux))
	defer server.Close()
	defer close(release) // before Close, so a stalled stream fails instead of hanging
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	get := func(path, accept string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, server.URL+path, nil)
		if accept != "" {
			req.Header.Set("Accept-Encoding", accept)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	res := get("/json", "gzip, deflate, br")
	gz, err := gzip.NewReader(res.Body)
	if err != nil || res.Header.Get("Content-Encoding") != "gzip" || res.Header.Get("Content-Length") == "4011" {
		t.Fatalf("json: encoding %q, length %q, err %v", res.Header.Get("Content-Encoding"), res.Header.Get("Content-Length"), err)
	}
	if body, _ := io.ReadAll(gz); !strings.HasPrefix(string(body), `{"rows":"row row`) || len(body) != 4011 {
		t.Fatalf("json body: %d bytes", len(body))
	}
	res.Body.Close()

	for _, accept := range []string{"", "gzip;q=0"} {
		res = get("/json", accept)
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.Header.Get("Content-Encoding") != "" || !strings.HasPrefix(string(body), `{"rows"`) {
			t.Fatalf("Accept-Encoding %q: encoding %q", accept, res.Header.Get("Content-Encoding"))
		}
	}

	res = get("/events", "gzip")
	defer res.Body.Close()
	first := make(chan string, 1)
	go func() {
		gz, err := gzip.NewReader(res.Body)
		if err != nil {
			first <- err.Error()
			return
		}
		line, _ := bufio.NewReader(gz).ReadString('\n')
		first <- line
	}()
	select {
	case line := <-first:
		if line != "data: first\n" {
			t.Fatalf("first event = %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first event did not arrive while the stream was open")
	}
}
