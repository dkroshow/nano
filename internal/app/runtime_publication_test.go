package app

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sidebar"
)

// These tests pause the real sidebar read pool after seeding its first
// snapshot. The next joined-session publication must wait for this read,
// while registration and provider handshakes remain free to finish.
type runtimeReadGate struct {
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *runtimeReadGate) wait() {
	if g.armed.Load() {
		g.once.Do(func() { close(g.entered) })
		<-g.release
	}
}

type runtimeReadConnector struct {
	driver driver.Driver
	dsn    string
	gate   *runtimeReadGate
}

func (c runtimeReadConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return runtimeReadConn{Conn: conn, gate: c.gate}, nil
}
func (c runtimeReadConnector) Driver() driver.Driver { return c.driver }

type runtimeReadConn struct {
	driver.Conn
	gate *runtimeReadGate
}

func (c runtimeReadConn) Prepare(query string) (driver.Stmt, error) {
	stmt, err := c.Conn.Prepare(query)
	if err != nil {
		return nil, err
	}
	return runtimeReadStmt{Stmt: stmt, gate: c.gate}, nil
}

type runtimeReadStmt struct {
	driver.Stmt
	gate *runtimeReadGate
}

func (s runtimeReadStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.gate.wait()
	return s.Stmt.Query(args)
}
func blockRuntimePublication(t *testing.T, s *server) func() {
	t.Helper()
	db := s.controlMetadata.CanonicalDatabase()
	var path string
	var seq int
	var name string
	if err := db.QueryRow("PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "rw")
	q.Set("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()
	gate := &runtimeReadGate{entered: make(chan struct{}), release: make(chan struct{})}
	pool := sql.OpenDB(runtimeReadConnector{driver: db.Driver(), dsn: u.String(), gate: gate})
	t.Cleanup(func() { _ = pool.Close() })
	metadata, err := hostedstore.NewMetadataStore(db, pool)
	if err != nil {
		t.Fatal(err)
	}
	s.sidebar = sidebar.New(sidebar.Sources{Catalog: s.hostedCatalog(), Registry: &s.Registry, Executions: s.agentExecutions, Metadata: metadata, MetadataError: s.controlMetadataErr, Attachments: s.attachments, Boot: &s.bootAttachments, Stars: s.threadStars, Mail: s.mail, MailError: s.mailErr, RuntimeEvents: s.runtimeEvents})
	runtimeSnapshotForTest(t, s)
	gate.armed.Store(true)
	var releaseOnce sync.Once
	return func() {
		releaseOnce.Do(func() {
			select {
			case <-gate.entered:
			case <-time.After(5 * time.Second):
				t.Error("runtime publication never reached its paused metadata read")
			}
			close(gate.release)
		})
	}
}
