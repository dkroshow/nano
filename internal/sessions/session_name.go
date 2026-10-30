package sessions

// StageNameBeforeBinding keeps a pre-native rename for the identity bind.
func (attachmentState *State) StageNameBeforeBinding(name string) (provider, nativeID string, staged bool) {
	attachmentState.stateMu.Lock()
	defer attachmentState.stateMu.Unlock()
	provider, nativeID = attachmentState.identity.provider, attachmentState.threadID
	if nativeID == "" {
		attachmentState.pendingName, attachmentState.displayName = name, name
		staged = true
	}
	return
}

// StageProvisionalName stages only if the probed identity is still current.
func (attachmentState *State) StageProvisionalName(nativeID, name string) (current string, staged bool) {
	attachmentState.stateMu.Lock()
	defer attachmentState.stateMu.Unlock()
	if attachmentState.threadID == nativeID {
		attachmentState.pendingName, attachmentState.displayName = name, name
		staged = true
	}
	return attachmentState.threadID, staged
}

// SetDisplayName updates the attachment display name under its state lock.
func (attachmentState *State) SetDisplayName(name string) {
	attachmentState.stateMu.Lock()
	attachmentState.pendingName, attachmentState.displayName = "", name
	attachmentState.stateMu.Unlock()
}

// applyPendingName publishes the highest available not-yet-durable name once
// a native id is known: a staged operator rename first, then the immutable
// creation fallback. The derived projection ranks manual and imported truth
// above creation, so an existing durable name remains authoritative.
func (attachmentState *State) applyPendingName(nativeThreadID string) {
	if nativeThreadID == "" {
		return // no id yet — keep the name for a later bind site
	}
	// The creation-name projection reads claimed identities only, so a publish
	// ahead of the claim would miss and wait for an unrelated names refresh.
	attachmentState.claimNativeID()
	attachmentState.stateMu.Lock()
	name := attachmentState.pendingName
	attachmentState.pendingName = ""
	attachmentState.stateMu.Unlock()
	attachmentState.view.publishPendingName(attachmentState.identity.provider, nativeThreadID, name)
}

func (views *View) publishPendingName(provider, nativeThreadID, name string) {
	if name != "" {
		if views.nameThread != nil {
			views.nameThread(provider, nativeThreadID, name)
		}
		return
	}
	if views.creationName == nil {
		return
	}
	name = views.creationName()
	if name == "" {
		return
	}
	if views.publishCreationName != nil {
		views.publishCreationName(provider, nativeThreadID, name)
	} else if views.nameThread != nil {
		// A narrow fallback for legacy embedders without a derived projection.
		views.nameThread(provider, nativeThreadID, name)
	}
}
