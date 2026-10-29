package sessions

func (delivery *Delivery) promptWithUnreadFooter(prompt string) (string, error) {
	if delivery == nil || delivery.identity.hostedThreadID == "" {
		return prompt, nil
	}
	if delivery.appendUnread == nil {
		return prompt, nil
	}
	return delivery.appendUnread(delivery.identity.hostedThreadID, prompt), nil
}
