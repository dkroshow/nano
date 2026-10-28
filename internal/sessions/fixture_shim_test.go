package sessions

import fixture "github.com/dkroshow/nano/internal/testsupport/claudeshim"

type fakeShim = fixture.Server
type shimCall = fixture.Call
type shimConn = fixture.Conn

var newFakeShim = fixture.NewServer
