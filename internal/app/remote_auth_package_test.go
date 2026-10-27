package app

import (
	"net/http"
	"testing"

	"github.com/dkroshow/nano/internal/httpapi/remoteauth"
)

func TestRemoteAuthPackageOwnsTheRemoteEdge(t *testing.T) {
	store, err := remoteauth.OpenStore(t.TempDir() + "/remote-auth.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	auth := remoteauth.New(store, "nano.example.ts.net:7690")
	mux := http.NewServeMux()
	auth.RegisterRoutes(mux)
	if auth.RemoteHost() != "nano.example.ts.net:7690" || auth.Guard(mux) == nil {
		t.Fatal("remote authentication edge is incomplete")
	}
}
