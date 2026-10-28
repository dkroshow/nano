package sessions

// StageNameBeforeBinding keeps a pre-native rename for the identity bind.
func (sess *Session) StageNameBeforeBinding(name string) (provider, nativeID string, staged bool) {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	provider, nativeID = sess.provider, sess.threadID
	if nativeID == "" {
		sess.pendingName, sess.displayName = name, name
		staged = true
	}
	return
}

// StageProvisionalName stages only if the probed identity is still current.
func (sess *Session) StageProvisionalName(nativeID, name string) (current string, staged bool) {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	if sess.threadID == nativeID {
		sess.pendingName, sess.displayName = name, name
		staged = true
	}
	return sess.threadID, staged
}

// SetDisplayName updates the attachment display name under its state lock.
func (sess *Session) SetDisplayName(name string) {
	sess.stateMu.Lock()
	sess.pendingName, sess.displayName = "", name
	sess.stateMu.Unlock()
}
