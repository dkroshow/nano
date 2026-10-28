package testsupport

type Sources struct {
	Binds    []string
	Releases int
	OnBind   func()
}

func (r *Sources) BindAttachedSource(owner, provider, nativeID, cwd, known string, background bool) {
	bind := provider + "/" + nativeID + "@" + cwd + "|" + known
	if background {
		bind += "|background"
	}
	r.Binds = append(r.Binds, bind)
	if r.OnBind != nil {
		r.OnBind()
	}
}

func (r *Sources) ReleaseAttachedSource(string) { r.Releases++ }

func (r *Sources) AwaitAttachedCurrent(string, bool, func() bool, <-chan struct{}) bool {
	return true
}
