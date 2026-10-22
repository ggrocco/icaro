package api

import (
	"context"
	"errors"
	"net/http"

	"icaro/internal/service"
	"icaro/internal/store"
	"icaro/internal/usage"
)

// UsageSource builds the usage report for a window ("Nd" or YYYY-MM-DD;
// empty means the default).
type UsageSource func(ctx context.Context, since string) (*usage.Report, error)

// RegisterUsage mounts GET /api/v1/usage, the web UI's view of
// `icaro usage --json` (ADR 0022).
func RegisterUsage(s *Server, src UsageSource) {
	s.Mux().Handle("GET /api/v1/usage", s.Auth(store.ScopeRead, func(w http.ResponseWriter, r *http.Request) {
		rep, err := src(r.Context(), r.URL.Query().Get("since"))
		var bad *usage.BadOption
		if errors.As(err, &bad) {
			err = &service.BadRequest{Msg: bad.Msg}
		}
		if err != nil {
			s.WriteError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, rep)
	}))
}
