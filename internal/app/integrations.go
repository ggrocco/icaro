package app

import (
	"context"
	"sync"

	"icaro/internal/config"
	"icaro/internal/integration"
	"icaro/internal/scheduler"
	"icaro/internal/workflow"
)

// integrationsArea holds the Phase 3 components of one App.
type integrationsArea struct {
	hub   *integration.Hub
	sched *scheduler.Scheduler
}

var integrationAreas sync.Map // *App -> *integrationsArea

func init() {
	RegisterWirer(50, "integrations", func(_ context.Context, a *App) error {
		settings := func(name string) config.IntegrationConfig { return a.Cfg.Integrations[name] }
		hub := integration.NewHub(integration.HubOptions{
			Dir: a.Layout.Integrations(), Store: a.Store, Secrets: a.Secrets,
			Webhooks: a.Cfg.Webhooks, Settings: settings, Log: a.Log.With("component", "integrations"),
		})
		sched := scheduler.New(scheduler.Options{Store: a.Store, Log: a.Log.With("component", "scheduler")})
		sched.SetSpecSource(integration.StoreSpecs{Store: a.Store})
		integrationAreas.Store(a, &integrationsArea{hub: hub, sched: sched})
		a.Add(hub)
		a.Add(sched)
		a.API.IntegrationRoutes(hub, a.Secrets, sched, a.Cfg.Daemon.PublicURL)
		for _, c := range integration.DoctorChecks(a.Layout.Integrations(), a.Store) {
			a.AddCheck(c)
		}
		return nil
	})
}

// IntegrationHub returns the app's integration hub: workflow.Catalog via
// Catalog() and the `uses:` executor via Executor().
func IntegrationHub(a *App) *integration.Hub {
	if v, ok := integrationAreas.Load(a); ok {
		return v.(*integrationsArea).hub
	}
	return nil
}

// Scheduler returns the app's cron scheduler.
func Scheduler(a *App) *scheduler.Scheduler {
	if v, ok := integrationAreas.Load(a); ok {
		return v.(*integrationsArea).sched
	}
	return nil
}

// SetWorkflowEngine connects the workflow service to the webhook router
// and the scheduler. Until it is called, deliveries are recorded with
// "workflow engine is not wired" and cron fires wait. src may be nil to
// keep reading active workflows from the store.
func SetWorkflowEngine(a *App, e workflow.Enqueuer, src workflow.SpecSource) {
	v, ok := integrationAreas.Load(a)
	if !ok {
		return
	}
	area := v.(*integrationsArea)
	area.hub.SetEnqueuer(e)
	area.sched.SetEnqueuer(e)
	if src != nil {
		area.hub.SetSpecSource(src)
		area.sched.SetSpecSource(src)
	}
}
