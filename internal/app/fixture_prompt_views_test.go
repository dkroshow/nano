package app

import fixture "github.com/dkroshow/nano/internal/testsupport"

var newTestQueuedTurnStore = fixture.NewQueue
var newTestSendAttemptStore = fixture.NewJournal
var mustQueuedTurnStore = fixture.MustQueue
var mustBindTransactionalTurnStore = fixture.BindPromptStore
