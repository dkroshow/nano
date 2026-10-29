package sessions

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type repairCatalog struct {
	waits atomic.Int32
	wait  func(<-chan struct{}) bool
}

func (*repairCatalog) BindAttachedSource(string, string, string, string, string, bool) {}
func (*repairCatalog) ReleaseAttachedSource(string)                                    {}
func (c *repairCatalog) AwaitAttachedCurrent(_ string, _ bool, _ func() bool, cancel <-chan struct{}) bool {
	c.waits.Add(1)
	return c.wait(cancel)
}

// A Stop that never reaches an idle provider fails the action before any
// catch-up, so no repair is claimed. Boundary: the repair action.
func TestTranscriptRepairFailsWhenStopDoesNotReachIdle(t *testing.T) {
	catalog := &repairCatalog{wait: func(<-chan struct{}) bool { return true }}
	sess, _ := readyTransactionalSendTestSession(t, nil)
	sess.stateOwner.catalogSources = catalog
	sess.stateOwner.lifecycleState = testLifecycleSnapshot("ready", true)
	_, err := sess.delivery.stopDiscardAndRepairWithin(context.Background(), "repair", 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "stop did not reach an idle provider boundary") || catalog.waits.Load() != 0 {
		t.Fatalf("stop failure = %v after %d catch-ups", err, catalog.waits.Load())
	}
}
