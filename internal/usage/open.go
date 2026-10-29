package usage

// OpenConfigured opens the selected usage store only when polling is enabled.
func OpenConfigured(enabled bool, path string) (*Store, string, error) {
	if !enabled || path == "" {
		return nil, "", nil
	}
	store, err := Open(path)
	return store, path, err
}
