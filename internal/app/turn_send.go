package app

// publishProviderRuntime republishes the runtime status of every session of
// provider, so each pane sees that provider's delivery expiry count.
func (s *server) publishProviderRuntime(provider string) {
	for _, sess := range s.SnapshotSessions() {
		if sess.Provider() == provider {
			sess.PublishProjectionRuntime()
		}
	}
}

// sendAttemptStore is the prompt owner's compatibility journal view.
