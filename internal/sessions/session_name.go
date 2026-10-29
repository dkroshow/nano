package sessions

// StageNameBeforeBinding keeps a pre-native rename for the identity bind.
func (sess *State) StageNameBeforeBinding(name string) (provider, nativeID string, staged bool) {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	provider, nativeID = sess.identity.provider, sess.threadID
	if nativeID == "" {
		sess.pendingName, sess.displayName = name, name
		staged = true
	}
	return
}

// StageProvisionalName stages only if the probed identity is still current.
func (sess *State) StageProvisionalName(nativeID, name string) (current string, staged bool) {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	if sess.threadID == nativeID {
		sess.pendingName, sess.displayName = name, name
		staged = true
	}
	return sess.threadID, staged
}

// SetDisplayName updates the attachment display name under its state lock.
func (sess *State) SetDisplayName(name string) {
	sess.stateMu.Lock()
	sess.pendingName, sess.displayName = "", name
	sess.stateMu.Unlock()
}

// applyPendingName publishes the highest available not-yet-durable name once
// a native id is known: a staged operator rename first, then the immutable
// creation fallback. The derived projection ranks manual and imported truth
// above creation, so an existing durable name remains authoritative.
func (sess *State) applyPendingName(nativeThreadID string) {
	if nativeThreadID == "" {
		return // no id yet — keep the name for a later bind site
	}
	sess.stateMu.Lock()
	name := sess.pendingName
	sess.pendingName = ""
	sess.stateMu.Unlock()
	if name != "" {
		if sess.view.nameThread != nil {
			sess.view.nameThread(sess.identity.provider, nativeThreadID, name)
		}
		return
	}
	if sess.view.creationName == nil {
		return
	}
	name = sess.view.creationName()
	if name == "" {
		return
	}
	if sess.view.publishCreationName != nil {
		sess.view.publishCreationName(sess.identity.provider, nativeThreadID, name)
	} else if sess.view.nameThread != nil {
		// A narrow fallback for legacy embedders without a derived projection.
		sess.view.nameThread(sess.identity.provider, nativeThreadID, name)
	}
}
