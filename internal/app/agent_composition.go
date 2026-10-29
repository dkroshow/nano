package app

import "github.com/dkroshow/nano/internal/agentapi"

func (s *server) agentController() *agentapi.Controller {
	if s.agent == nil {
		panic("agent controller used before construction")
	}
	return s.agent
}

func newAgentController(s *server) *agentapi.Controller {
	sources := agentapi.Sources{
		Hosted: s.hostedController(), Identity: s.hostedIdentity(),
		Mail: s.mail, MailError: s.mailErr, MailEvents: s.mailEvents, Notices: s.notices,
		Sidebar: s.sidebarOwner(), Heartbeat: s.heartbeat, Feedback: s.feedback,
		BuildStamp: buildStamp, BundleName: embeddedBundleName(),
	}
	if s.browserDiagnostics != nil {
		sources.Diagnostics = s.browserDiagnostics
	}
	return agentapi.New(sources)
}
