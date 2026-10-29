package app

import "github.com/dkroshow/nano/internal/agentapi"

func (s *server) agentController() *agentapi.Controller {
	s.agentOnce.Do(func() {
		sources := agentapi.Sources{
			Hosted: s.hostedController(), Identity: s.hostedIdentity(),
			Mail: s.mail, MailError: s.mailErr, MailEvents: s.mailEvents, Notices: s.notices,
			Sidebar: s.sidebarOwner(), Heartbeat: s.heartbeat, Feedback: s.feedback,
			BuildStamp: buildStamp, BundleName: embeddedBundleName(),
		}
		if s.browserDiagnostics != nil {
			sources.Diagnostics = s.browserDiagnostics
		}
		s.agent = agentapi.New(sources)
	})
	return s.agent
}
