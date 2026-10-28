package app

import (
	"github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/sessions"
)

func runtimeCreationRequest(req hosted.CreationRequest) spawnRequest {
	return spawnRequest{ProviderRequest: sessions.ProviderRequest{
		Provider:              req.Provider,
		Mode:                  req.Mode,
		ThreadID:              req.ThreadID,
		Cwd:                   req.Cwd,
		Model:                 req.Model,
		Effort:                req.Effort,
		ServiceTier:           req.ServiceTier,
		Sandbox:               req.Sandbox,
		Approval:              req.Approval,
		PermissionMode:        req.PermissionMode,
		AllowedTools:          req.AllowedTools,
		Personality:           req.Personality,
		ApprovalsReviewer:     req.ApprovalsReviewer,
		Ephemeral:             req.Ephemeral,
		BaseInstructions:      req.BaseInstructions,
		DeveloperInstructions: req.DeveloperInstructions,
		ModelProvider:         req.ModelProvider,
		Config:                req.Config,
		ThreadSource:          req.ThreadSource,
		SessionStartSource:    req.SessionStartSource,
		ServiceName:           req.ServiceName,
	},
		ClientSessionID:  req.ClientSessionID,
		InvokerThreadRef: req.InvokerThreadRef,
		Subscriptions:    req.Subscriptions,
		Assignment:       req.Assignment,
		PresentationMode: req.PresentationMode,
		SidebarPlacement: req.SidebarPlacement,
		Name:             req.Name,
	}
}

func (s *server) prepareHostedCreation(req hosted.CreationRequest) (hosted.CreationPlan, error) {
	resolved, err := s.resolveSpawn(runtimeCreationRequest(req))
	if err != nil {
		return hosted.CreationPlan{}, err
	}
	return hosted.CreationPlan{ClientSessionID: resolved.ClientSessionID, Provider: resolved.Provider, Mode: resolved.Mode, PresentationMode: resolved.PresentationMode, SidebarPlacement: resolved.SidebarPlacement, Name: resolved.Name,
		Attach: func(record agentSessionRecord, resume bool) (string, error) {
			if resume {
				resolved.Provider = record.Provider
				resolved.Mode = "resume"
				resolved.ThreadID = record.NativeThreadID
				resolved.HostedThreadID = record.ID
				resolved.PresentationMode = record.PresentationMode
			}
			resolved.InvokerHostedThreadID = record.InvokerHostedThreadID
			resolved.InvokerNativeThreadID = record.InvokerNativeThreadID
			resolved.InvokerProvider = record.InvokerProvider
			resolved.HostedThreadID = record.ID
			sess, err := s.spawnSession(resolved)
			if err != nil {
				return "", err
			}
			return sess.HostedThreadID(), nil
		},
	}, nil
}
