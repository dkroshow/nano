package app

import (
	"testing"

	fixture "github.com/dkroshow/nano/internal/testsupport/codexserver"
)

type fakeAppServer struct{ *fixture.Server }

func newFakeAppServer(t testing.TB) *fakeAppServer { return &fakeAppServer{fixture.NewServer(t)} }
