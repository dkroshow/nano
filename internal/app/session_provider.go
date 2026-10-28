package app

import (
	"context"
	"errors"
	"log"
)

// beginProviderSpawn hands the session to its provider. What it returns is what
// the POST answers with, so only the failures a caller can act on — an
// already-attached thread, a resume ladder that found nothing to attach to —
// come back as errors here. Handshake failures land on the session itself.
func (sess *session) beginProviderSpawn(req spawnRequest, daemon *codexDaemon, discard func()) error {
	if err := sess.ensureCoreSession(); err != nil {
		return err
	}
	if req.Provider == "claude" && req.Mode == "resume" {
		// Claude attach waits for replay to pass through the session mailbox.
		// Run its orchestration outside that mailbox so it can make progress.
		// Manual resumes still return their synchronous HTTP refusal errors;
		// boot and external resumes retain their existing asynchronous shells.
		return sess.beginProviderSpawnLegacy(req, daemon, discard)
	}
	operation := func(context.Context) error { return sess.beginProviderSpawnLegacy(req, daemon, discard) }
	if req.Mode == "resume" {
		return sess.core.ResumeLegacy(context.Background(), operation)
	}
	return sess.core.StartLegacy(context.Background(), operation)
}

// beginProviderSpawnLegacy is the c2: asynchronous attachment shim. Provider
// adapters replace it when their Start and Resume operations return evidence.
func (sess *session) beginProviderSpawnLegacy(req spawnRequest, daemon *codexDaemon, discard func()) error {
	if req.Provider == "codex" {
		return startCodexSession(sess, daemon, req)
	}
	if req.Provider == "grok" {
		startGrokSession(sess, req)
		return nil
	}
	if req.Mode == "resume" {
		if req.bootReattach != nil {
			go func() {
				defer req.bootReattach.holdHandshakeSlot(sess)()
				sess.stateMu.Lock()
				closed := sess.closed
				sess.stateMu.Unlock()
				if closed {
					sess.recordRestartFailure("closed")
					return
				}
				if err := sess.resumeClaudeForSpawn(req); err != nil {
					if errors.Is(err, errSpawnSessionClosed) {
						sess.recordRestartFailure("closed")
						log.Printf("claude boot session %s was closed before its transport came up", sess.id)
						return
					}
					sess.recordRestartFailure("handshake_failed")
					log.Printf("boot re-attach %s/%s failed (left detached): %v", req.Provider, req.ThreadID, err)
					discard()
				}
			}()
			return nil
		}
		if req.externalResumePosture {
			go func() {
				if err := sess.resumeClaudeForSpawn(req); err != nil {
					if errors.Is(err, errSpawnSessionClosed) {
						return
					}
					sess.failSpawn(err.Error())
				}
			}()
			return nil
		}
		return sess.resumeClaudeForSpawn(req)
	}
	go func() {
		if _, err := startClaudeSession(sess, req); err != nil {
			if errors.Is(err, errSpawnSessionClosed) {
				log.Printf("claude session %s was closed before its transport came up", sess.id)
				return // closed by the operator: reporting a failure would resurrect it
			}
			log.Printf("start claude session %s: %v", sess.id, err)
			sess.failSpawn(err.Error())
		}
	}()
	return nil
}

func (sess *session) resumeClaudeForSpawn(req spawnRequest) error {
	// The manual path calls this synchronously because its rungs decide WHICH
	// session the operator gets (attach, respawn, or refuse), and the refusal is
	// the frozen 409 the frontend routes on. Boot runs the same work behind its
	// already-registered shell because no HTTP caller is waiting for that choice.
	_, err := resumeClaudeSession(sess, req)
	if err == nil {
		sess.recordRestartStep(restartAttached)
	}
	if errors.Is(err, errClaudeProvisionalGone) && sess.forgetAttachment != nil {
		// The never-prompted thread's child is gone and it has no transcript
		// to come back from. Retire the record: an empty conversation lost is
		// nothing lost, while a surviving record would sit in the sidebar as
		// a detached row that can never resume.
		sess.forgetAttachment("claude", req.ThreadID)
	}
	return err
}
