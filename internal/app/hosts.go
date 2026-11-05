package app

import (
	"context"

	"icaro/internal/api"
	"icaro/internal/hosts"
)

func init() {
	// Remote hosts (ADR 0033): the daemon signs in to other daemons and
	// lists their workflows next to its own.
	RegisterWirer(72, "hosts", func(_ context.Context, a *App) error {
		reg := hosts.New(hosts.Options{Store: a.Store, Key: a.Key, Local: a.Service.ListWorkflows, Log: a.Log})
		api.RegisterHosts(a.API, reg)
		return nil
	})
}
