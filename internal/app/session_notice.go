package app

func (sess *session) promptWithUnreadFooter(prompt string) (string, error) {
	if sess == nil || sess.hostedThreadID == "" {
		return prompt, nil
	}
	if sess.appendUnread == nil {
		return prompt, nil
	}
	return sess.appendUnread(sess.hostedThreadID, prompt), nil
}
