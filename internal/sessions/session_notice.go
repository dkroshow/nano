package sessions

func (sess *Delivery) promptWithUnreadFooter(prompt string) (string, error) {
	if sess == nil || sess.identity.hostedThreadID == "" {
		return prompt, nil
	}
	if sess.appendUnread == nil {
		return prompt, nil
	}
	return sess.appendUnread(sess.identity.hostedThreadID, prompt), nil
}
