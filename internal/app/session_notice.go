package app

func (sess *session) promptWithUnreadFooter(prompt string) (string, error) {
	if sess == nil || sess.hostedThreadID == "" {
		return prompt, nil
	}
	return sess.notices.Append(sess.hostedThreadID, prompt), nil
}
