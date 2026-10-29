package sessions

// StageNameBeforeBinding keeps a pre-native rename for the identity bind.
func (sess *View) StageNameBeforeBinding(name string) (provider, nativeID string, staged bool) {
	sess.session.stateOwner.stateMu.Lock()
	defer sess.session.stateOwner.stateMu.Unlock()
	provider, nativeID = sess.session.provider, sess.session.stateOwner.threadID
	if nativeID == "" {
		sess.session.stateOwner.pendingName, sess.session.stateOwner.displayName = name, name
		staged = true
	}
	return
}

// StageProvisionalName stages only if the probed identity is still current.
func (sess *View) StageProvisionalName(nativeID, name string) (current string, staged bool) {
	sess.session.stateOwner.stateMu.Lock()
	defer sess.session.stateOwner.stateMu.Unlock()
	if sess.session.stateOwner.threadID == nativeID {
		sess.session.stateOwner.pendingName, sess.session.stateOwner.displayName = name, name
		staged = true
	}
	return sess.session.stateOwner.threadID, staged
}

// SetDisplayName updates the attachment display name under its state lock.
func (sess *View) SetDisplayName(name string) {
	sess.session.stateOwner.stateMu.Lock()
	sess.session.stateOwner.pendingName, sess.session.stateOwner.displayName = "", name
	sess.session.stateOwner.stateMu.Unlock()
}

// applyPendingName publishes the highest available not-yet-durable name once
// a native id is known: a staged operator rename first, then the immutable
// creation fallback. The derived projection ranks manual and imported truth
// above creation, so an existing durable name remains authoritative.
func (sess *View) applyPendingName(nativeThreadID string) {
	if nativeThreadID == "" {
		return // no id yet — keep the name for a later bind site
	}
	sess.session.stateOwner.stateMu.Lock()
	name := sess.session.stateOwner.pendingName
	sess.session.stateOwner.pendingName = ""
	sess.session.stateOwner.stateMu.Unlock()
	if name != "" {
		if sess.nameThread != nil {
			sess.nameThread(sess.session.provider, nativeThreadID, name)
		}
		return
	}
	if sess.creationName == nil {
		return
	}
	name = sess.creationName()
	if name == "" {
		return
	}
	if sess.publishCreationName != nil {
		sess.publishCreationName(sess.session.provider, nativeThreadID, name)
	} else if sess.nameThread != nil {
		// A narrow fallback for legacy embedders without a derived projection.
		sess.nameThread(sess.session.provider, nativeThreadID, name)
	}
}
