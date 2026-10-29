package sessions

func newDeliveryTestProjection(provider string) *LiveProjection {
	return NewLiveProjection(provider, "", false)
}

func newBlockedProjection(provider string) *LiveProjection {
	return newDeliveryTestProjection(provider)
}
