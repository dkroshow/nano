package app

import hostedstore "github.com/dkroshow/nano/internal/hosted/store"

type hostedCloseStars struct{ store *threadStarsStore }

func (a hostedCloseStars) IsStarred(provider, native string) bool {
	return a.store.isStarred(provider, native)
}
func (a hostedCloseStars) UnstarMany(keys []hostedstore.AttachmentKey) error {
	return a.store.unstarMany(keys)
}
