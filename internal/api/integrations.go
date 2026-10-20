package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"icaro/internal/integration"
	"icaro/internal/secrets"
	"icaro/internal/service"
	"icaro/internal/store"
)

// Schedules lists persisted cron state (implemented by the scheduler).
type Schedules interface {
	Next(ctx context.Context) ([]*store.CronState, error)
}

// IntegrationRoutes registers the Phase 3 routes: the unauthenticated,
// manifest-verified webhook endpoints and the authenticated integration,
// credential, delivery and schedule API.
func (s *Server) IntegrationRoutes(hub *integration.Hub, creds integration.CredentialSetter, sched Schedules, publicURL string) {
	m := s.mux
	// Inbound webhooks authenticate per manifest, never by API token.
	m.HandleFunc("POST /hooks/{integration}", hub.ServeHook)
	m.HandleFunc("POST /hooks/webhook/{token}", hub.ServeHookToken)

	m.HandleFunc("GET /api/v1/schema/integration-manifest", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/schema+json")
		_, _ = w.Write(integration.SchemaJSON())
	})
	m.Handle("GET /api/v1/integrations", s.auth(store.ScopeRead, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"integrations": orEmptySlice(hub.Integrations())})
	}))
	m.Handle("GET /api/v1/integrations/status", s.auth(store.ScopeRead, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, hub.Status(r.Context()))
	}))
	m.Handle("POST /api/v1/integrations/reload", s.auth(store.ScopeWrite, func(w http.ResponseWriter, r *http.Request) {
		if err := hub.Registry().Reload(r.Context()); err != nil {
			s.writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, hub.Status(r.Context()))
	}))
	m.Handle("GET /api/v1/integrations/{name}", s.auth(store.ScopeRead, func(w http.ResponseWriter, r *http.Request) {
		info, err := hub.Integration(r.PathValue("name"))
		if err != nil {
			s.writeError(w, mapIntegrationErr(err))
			return
		}
		writeJSON(w, http.StatusOK, info)
	}))
	m.Handle("PUT /api/v1/integrations/{name}/manifest", s.auth(store.ScopeWrite, func(w http.ResponseWriter, r *http.Request) {
		src, err := readYAML(r)
		if err != nil {
			s.writeError(w, err)
			return
		}
		commit, err := hub.EditManifest(r.Context(), r.PathValue("name"), []byte(src), "token:"+TokenFrom(r.Context()).Name)
		var ve *integration.ValidationError
		if errors.As(err, &ve) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "validation failed", "issues": ve.Issues})
			return
		}
		if err != nil {
			s.writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"pack": integration.PackLocal, "commit": commit, "changed": commit != ""})
	}))
	m.Handle("GET /api/v1/actions/{ref}/schema", s.auth(store.ScopeRead, func(w http.ResponseWriter, r *http.Request) {
		schema, ok := hub.Registry().Snapshot().ActionInputSchema(r.PathValue("ref"))
		if !ok {
			s.writeError(w, service.ErrNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/schema+json")
		writeJSON(w, http.StatusOK, schema)
	}))

	m.Handle("GET /api/v1/credentials", s.auth(store.ScopeRead, func(w http.ResponseWriter, r *http.Request) {
		out, err := hub.Credentials(r.Context(), creds)
		if err != nil {
			s.writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"credentials": orEmptySlice(out)})
	}))
	m.Handle("GET /api/v1/credentials/plan", s.auth(store.ScopeRead, func(w http.ResponseWriter, r *http.Request) {
		plan, err := hub.PlanCredential(r.URL.Query().Get("integration"), r.URL.Query().Get("name"))
		if err != nil {
			s.writeError(w, mapIntegrationErr(err))
			return
		}
		writeJSON(w, http.StatusOK, plan)
	}))
	m.Handle("POST /api/v1/credentials", s.auth(store.ScopeAdmin, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Integration string   `json:"integration"`
			Name        string   `json:"name"`
			Value       string   `json:"value"`
			Hosts       []string `json:"hosts"`
		}
		if err := readJSON(r, &in); err != nil {
			s.writeError(w, err)
			return
		}
		plan, err := hub.AddCredential(r.Context(), creds, in.Integration, in.Name, in.Value, in.Hosts)
		if err != nil {
			s.writeError(w, mapIntegrationErr(err))
			return
		}
		writeJSON(w, http.StatusCreated, plan)
	}))
	m.Handle("DELETE /api/v1/credentials/{name}", s.auth(store.ScopeAdmin, func(w http.ResponseWriter, r *http.Request) {
		if err := creds.Delete(r.Context(), r.PathValue("name")); err != nil {
			s.writeError(w, mapIntegrationErr(err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	m.Handle("GET /api/v1/deliveries", s.auth(store.ScopeRead, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		out, err := hub.Deliveries(r.Context(), q.Get("integration"), q.Get("status"), queryInt(r, "limit", 50))
		if err != nil {
			s.writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deliveries": orEmptySlice(out)})
	}))
	m.Handle("GET /api/v1/deliveries/{id}", s.auth(store.ScopeRead, func(w http.ResponseWriter, r *http.Request) {
		d, err := hub.Delivery(r.Context(), r.PathValue("id"))
		if err != nil {
			s.writeError(w, mapIntegrationErr(err))
			return
		}
		writeJSON(w, http.StatusOK, d)
	}))
	m.Handle("POST /api/v1/deliveries/{id}/redeliver", s.auth(store.ScopeWrite, func(w http.ResponseWriter, r *http.Request) {
		out, err := hub.Redeliver(r.Context(), r.PathValue("id"))
		if err != nil {
			s.writeError(w, mapIntegrationErr(err))
			return
		}
		writeJSON(w, http.StatusAccepted, out)
	}))

	tokenView := func(t *store.WebhookToken) map[string]string {
		path := "/hooks/webhook/" + t.Token
		v := map[string]string{"workflow": t.Workflow, "token": t.Token, "path": path, "created_at": t.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")}
		if publicURL != "" {
			v["url"] = strings.TrimRight(publicURL, "/") + path
		}
		return v
	}
	m.Handle("GET /api/v1/webhook-tokens", s.auth(store.ScopeAdmin, func(w http.ResponseWriter, r *http.Request) {
		toks, err := hub.SyncWebhookTokens(r.Context())
		if err != nil {
			s.writeError(w, err)
			return
		}
		out := []map[string]string{}
		for _, t := range toks {
			out = append(out, tokenView(t))
		}
		writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
	}))
	m.Handle("POST /api/v1/webhook-tokens/{workflow}/rotate", s.auth(store.ScopeAdmin, func(w http.ResponseWriter, r *http.Request) {
		t, err := hub.RotateWebhookToken(r.Context(), r.PathValue("workflow"))
		if err != nil {
			s.writeError(w, mapIntegrationErr(err))
			return
		}
		writeJSON(w, http.StatusOK, tokenView(t))
	}))

	if sched != nil {
		m.Handle("GET /api/v1/schedules", s.auth(store.ScopeRead, func(w http.ResponseWriter, r *http.Request) {
			states, err := sched.Next(r.Context())
			if err != nil {
				s.writeError(w, err)
				return
			}
			out := []map[string]any{}
			for _, c := range states {
				v := map[string]any{"workflow": c.Workflow, "trigger": c.Idx, "cron": c.Expr, "next_at": c.NextAt.UTC().Format("2006-01-02T15:04:05Z")}
				if c.LastFiredAt != nil {
					v["last_fired_at"] = c.LastFiredAt.UTC().Format("2006-01-02T15:04:05Z")
				}
				out = append(out, v)
			}
			writeJSON(w, http.StatusOK, map[string]any{"schedules": out})
		}))
	}
}

// mapIntegrationErr maps integration-layer errors onto API statuses.
func mapIntegrationErr(err error) error {
	switch {
	case errors.Is(err, integration.ErrNotFound), errors.Is(err, store.ErrNotFound), errors.Is(err, secrets.ErrNotFound):
		return service.ErrNotFound
	case errors.Is(err, context.Canceled):
		return err
	}
	// Remaining integration errors are caller mistakes (unknown credential,
	// invalid host, unverified delivery).
	return &service.BadRequest{Msg: err.Error()}
}

func orEmptySlice[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
