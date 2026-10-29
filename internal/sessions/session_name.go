package sessions

// StageNameBeforeBinding keeps a pre-native rename for the identity bind.
func (sess *Session) StageNameBeforeBinding(name string) (provider, nativeID string, staged bool) {
	sess.stateOwner.stateMu.Lock()
	defer sess.stateOwner.stateMu.Unlock()
	provider, nativeID = sess.provider, sess.stateOwner.threadID
	if nativeID == "" {
		sess.stateOwner.pendingName, sess.stateOwner.displayName = name, name
		staged = true
	}
	return
}

// StageProvisionalName stages only if the probed identity is still current.
func (sess *Session) StageProvisionalName(nativeID, name string) (current string, staged bool) {
	sess.stateOwner.stateMu.Lock()
	defer sess.stateOwner.stateMu.Unlock()
	if sess.stateOwner.threadID == nativeID {
		sess.stateOwner.pendingName, sess.stateOwner.displayName = name, name
		staged = true
	}
	return sess.stateOwner.threadID, staged
}

// SetDisplayName updates the attachment display name under its state lock.
func (sess *Session) SetDisplayName(name string) {
	sess.stateOwner.stateMu.Lock()
	sess.stateOwner.pendingName, sess.stateOwner.displayName = "", name
	sess.stateOwner.stateMu.Unlock()
}
