package app

import (
	"context"
	"io/fs"
	"path/filepath"

	"icaro/internal/api"
	"icaro/internal/assist"
	"icaro/internal/authoring"
	"icaro/internal/mcp"
	"icaro/internal/memory"
	"icaro/skills"
)

func init() {
	// After authoring (70): the assistant serves the same read-only
	// authoring tools on each assist session's socket, plus its own draft
	// and approval-gated run tools (ADR 0035).
	RegisterWirer(71, "assistant", func(_ context.Context, a *App) error {
		if a.Sessions == nil {
			return nil
		}
		o := assist.Options{
			Store: a.Store, Config: a.Cfg, Sessions: a.Sessions, Workflows: a.Service, Events: a.Workflows.Events,
			Log: a.Log, LogsDir: filepath.Join(a.Layout.Logs(), "assist"),
		}
		if a.Secrets != nil {
			o.Secrets = a.Secrets
		}
		a.Assist = assist.New(o)
		deps := authoring.Deps{Workflows: a.Service}
		if hub := IntegrationHub(a); hub != nil {
			deps.Integrations = hub
		}
		if a.Secrets != nil {
			deps.Secrets = a.Secrets
		}
		srv := a.Assist.Server(buildVersion(), func(s *mcp.Server) {
			authoring.RegisterRead(s, deps)
			if a.Memory != nil {
				// The caller is read-only, so memory_write refuses.
				memory.RegisterTools(s, a.Memory)
			}
		})
		skill, _ := fs.ReadFile(skills.FS, "icaro-workflows/SKILL.md")
		a.Sessions.SetAssist(a.Assist.Attacher(srv), assist.Preamble(string(skill)))
		api.RegisterChats(a.API, a.Assist, a.Workflows.Events)
		a.Add(a.Assist)
		return nil
	})
}
