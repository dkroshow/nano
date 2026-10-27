package app

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/dkroshow/nano/internal/features/feedback"
	"github.com/dkroshow/nano/internal/features/heartbeat"
	"github.com/dkroshow/nano/internal/features/mail"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/httpapi/remoteauth"
	"github.com/dkroshow/nano/internal/usage"
)

// Each production store must protect the database and its live SQLite sidecars,
// including history's transcript search index, on first open.
func TestFreshStoresKeepSQLiteFilesPrivate(t *testing.T) {
	for _, name := range []string{"history", "threads", "mail", "heartbeat", "feedback", "stars", "pins", "remote-auth", "usage"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name+".sqlite")
			var store io.Closer
			var err error
			switch name {
			case "history":
				store, err = history.Open(path)
			case "threads":
				store, err = openControlMetadataStore(path)
			case "mail":
				store, _, err = mail.OpenStore(path, "")
			case "heartbeat":
				store, err = heartbeat.OpenStore(path, nil)
			case "feedback":
				store, err = feedback.Open(path)
			case "stars":
				store, err = openThreadStarsStore(path)
			case "pins":
				store, err = openThreadPinsStore(path)
			case "remote-auth":
				store, err = remoteauth.OpenStore(path)
			case "usage":
				store, err = usage.Open(path)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			for _, suffix := range []string{"", "-wal", "-shm"} {
				info, err := os.Stat(path + suffix)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0600 {
					t.Errorf("%s mode = %04o, want 0600", suffix, info.Mode().Perm())
				}
			}
		})
	}
}
