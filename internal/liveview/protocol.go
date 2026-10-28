package liveview

const (
	// Version identifies the transcript-view wire format.
	Version = 1
	// SnapshotOp replaces the current view with a full snapshot.
	SnapshotOp = "snapshot"
	// TurnUpsertOp carries a changed turn and optional runtime state.
	TurnUpsertOp = "turn.upsert"
	// InteractionOp adds or updates one pending provider request.
	InteractionOp = "interaction.upsert"
	// InteractionRemoveOp removes a resolved provider request.
	InteractionRemoveOp = "interaction.remove"
	// MetaRefreshOp asks consumers to reload metadata.
	MetaRefreshOp = "meta.refresh"
	// Full includes every conversation item.
	Full View = "full"
	// Messages selects the message-only conversation view.
	Messages View = "messages"
	// FirstPaintRows is the initial saved-row page size.
	FirstPaintRows = 20
	// PageMaxRows bounds each conversation page.
	PageMaxRows = 100
)
