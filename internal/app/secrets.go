package app

import (
	"context"

	"icaro/internal/secrets"
)

func init() {
	RegisterWirer(10, "secrets", func(_ context.Context, a *App) error {
		a.Secrets = secrets.New(a.Store, a.Key)
		return nil
	})
}
