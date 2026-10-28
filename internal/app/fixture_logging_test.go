package app

import (
	fixture "github.com/dkroshow/nano/internal/testsupport"
	shimfixture "github.com/dkroshow/nano/internal/testsupport/claudeshim"
)

type syncBuffer = fixture.LogBuffer

var fakeClaudeBinary = shimfixture.FakeBinary
