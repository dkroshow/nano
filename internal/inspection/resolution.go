package inspection

// ResolvedValue reports a passive configuration observation and its source.
type ResolvedValue struct {
	State  string `json:"state"`
	Source string `json:"source,omitempty"`
	Value  any    `json:"value,omitempty"`
}

// CurrentResolution is the passive environment inspection shown for one thread.
type CurrentResolution struct {
	State           string                   `json:"state"`
	ResolverVersion string                   `json:"resolverVersion"`
	ResolvedAt      string                   `json:"resolvedAt,omitempty"`
	TargetCWD       string                   `json:"targetCwd,omitempty"`
	Stale           bool                     `json:"stale,omitempty"`
	Values          map[string]ResolvedValue `json:"values"`
	Diagnostics     []string                 `json:"diagnostics"`
}
