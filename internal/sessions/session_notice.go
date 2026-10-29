package sessions

func (sess *Delivery) promptWithUnreadFooter(prompt string) (string, error) {
	if sess == nil || sess.session.hostedThreadID == "" {
		return prompt, nil
	}
	if sess.appendUnread == nil {
		return prompt, nil
	}
	return sess.appendUnread(sess.session.hostedThreadID, prompt), nil
}
