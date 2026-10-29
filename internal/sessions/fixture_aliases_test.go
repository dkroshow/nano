package sessions

import (
	promptsmodel "github.com/dkroshow/nano/internal/core/prompts"
	testfixture "github.com/dkroshow/nano/internal/testsupport"
	shimfixture "github.com/dkroshow/nano/internal/testsupport/claudeshim"
)

var recvFrame = shimfixture.RecvFrame
var sendFrame = shimfixture.SendFrame
var claudeTestSpoolBytes = shimfixture.SpoolBytes
var writeClaudeTestSpool = shimfixture.WriteSpool
var messageStart = shimfixture.StartMessage
var textDelta = shimfixture.TextDelta
var writeTestFile = testfixture.WriteFile
var testLifecycleSnapshot = testfixture.LifecycleSnapshot

const testMessageStart = shimfixture.MessageStart
const testMessageStop = shimfixture.MessageStop
const testUserPrompt = shimfixture.UserPrompt
const claudeInitLine = shimfixture.InitLine
const claudeReplayResultLine = shimfixture.ReplayResultLine

var testTurnDatabase = testfixture.TurnDatabase
var testPromptStore = testfixture.PromptStore
var newQueuedTurnStore = promptsmodel.NewLegacyQueue
var newSendAttemptStore = promptsmodel.NewLegacyJournal
var QueueSnapshot = testfixture.QueueSnapshot
var QueueLegacyRows = testfixture.QueueLegacyRows
var QueueSaveLegacyRows = testfixture.QueueSaveLegacyRows
var QueuePutSubmission = testfixture.QueuePutSubmission
var QueueEnqueue = testfixture.QueueEnqueue
var QueueSeedAttempt = testfixture.QueueSeedAttempt
var QueueSetState = testfixture.QueueSetState
var QueueBindProviderTurn = testfixture.QueueBindProviderTurn
var QueueRemove = testfixture.QueueRemove
var QueueMigrateThread = testfixture.QueueMigrateThread
var JournalSnapshot = testfixture.JournalSnapshot
var JournalLegacyRows = testfixture.JournalLegacyRows
var JournalPut = testfixture.JournalPut
var JournalBindCanonicalMessage = testfixture.JournalBindCanonicalMessage
var JournalConfirm = testfixture.JournalConfirm
var JournalConfirmOnce = testfixture.JournalConfirmOnce
var JournalConfirmClaudeExactCompleted = testfixture.JournalConfirmClaudeExactCompleted
var JournalConfirmRecord = testfixture.JournalConfirmRecord
var JournalSaveLegacyRows = testfixture.JournalSaveLegacyRows
var JournalMigrateThread = testfixture.JournalMigrateThread
