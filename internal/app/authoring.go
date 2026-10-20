package app

import (
	"context"

	"icaro/internal/authoring"
)

func init() {
	// After memory (40) creates the host MCP server and workflows (60)
	// attach their collaborators to the service.
	RegisterWirer(70, "authoring", func(_ context.Context, a *App) error {
		if a.MCP == nil {
			return nil
		}
		d := authoring.Deps{Workflows: a.Service}
		if hub := IntegrationHub(a); hub != nil {
			d.Integrations = hub
		}
		if a.Secrets != nil {
			d.Secrets = a.Secrets
		}
		authoring.Register(a.MCP, d)
		return nil
	})
}
