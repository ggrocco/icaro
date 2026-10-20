package app

import (
	"context"
)

// The glue wirers connect areas that were built independently: the
// session spine and integrations feed the workflow engine (order 55,
// before it is built at 60), and the built engine feeds webhooks and cron
// (order 65).
func init() {
	RegisterWirer(55, "workflow-collaborators", func(_ context.Context, a *App) error {
		w := &a.Workflows
		if a.Sessions != nil {
			w.Sessions = a.Sessions
		}
		if a.Egress != nil {
			w.Egress = a.Egress
		}
		if hub := IntegrationHub(a); hub != nil {
			w.Actions = hub.Executor()
			w.Catalog = hub.Catalog()
		}
		return nil
	})
	RegisterWirer(65, "workflow-triggers", func(_ context.Context, a *App) error {
		SetWorkflowEngine(a, a.Service, a.Service)
		if sched := Scheduler(a); sched != nil {
			a.Service.OnWorkflowChange(sched.Wake)
		}
		return nil
	})
}
