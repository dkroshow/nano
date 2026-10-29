package sessions

import (
	"testing"

	fixture "github.com/dkroshow/nano/internal/testsupport/codexserver"
)

type fakeAppServer struct{ *fixture.Server }

func newFakeAppServer(t testing.TB) *fakeAppServer { return &fakeAppServer{fixture.NewServer(t)} }

func (f *fakeAppServer) daemon() *CodexDaemon {
	return NewCodexDaemon(f.WSURL(), supervisedDaemonLabel, func() bool { return true })
}
