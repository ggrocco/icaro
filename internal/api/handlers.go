package api

import (
	"net/http"
	"strconv"

	"icaro/internal/service"
	"icaro/internal/workflow"
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Store().DB().PingContext(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "db unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleSchema(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/schema+json")
	_, _ = w.Write(workflow.SchemaJSON())
}

func (s *Server) handleListWorkflows(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.ListWorkflows(r.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflows": out})
}

func (s *Server) handleApplyWorkflow(w http.ResponseWriter, r *http.Request) {
	yaml, err := readYAML(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	info, err := s.svc.ApplyWorkflow(r.Context(), yaml, "token:"+TokenFrom(r.Context()).Name)
	if err != nil {
		s.writeError(w, err)
		return
	}
	status := http.StatusOK
	if info.Version == 1 {
		status = http.StatusCreated
	}
	writeJSON(w, status, info)
}

func (s *Server) handleValidateWorkflow(w http.ResponseWriter, r *http.Request) {
	yaml, err := readYAML(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	wf, issues, err := s.svc.ValidateWorkflow(r.Context(), yaml)
	if err != nil {
		s.writeError(w, err)
		return
	}
	resp := map[string]any{"valid": len(issues) == 0, "issues": issues}
	if wf != nil {
		resp["name"] = wf.Name
	}
	if issues == nil {
		resp["issues"] = []workflow.Issue{}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGetWorkflow(w http.ResponseWriter, r *http.Request) {
	info, err := s.svc.GetWorkflow(r.Context(), r.PathValue("name"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleDeleteWorkflow(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteWorkflow(r.Context(), r.PathValue("name")); err != nil {
		s.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSetStatus(status string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := s.svc.SetWorkflowStatus(r.Context(), r.PathValue("name"), status); err != nil {
			s.writeError(w, err)
			return
		}
		info, err := s.svc.GetWorkflow(r.Context(), r.PathValue("name"))
		if err != nil {
			s.writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, info)
	}
}

func (s *Server) handleListVersions(w http.ResponseWriter, r *http.Request) {
	vs, err := s.svc.ListWorkflowVersions(r.Context(), r.PathValue("name"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": vs})
}

func (s *Server) handleGetVersion(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("version"))
	if err != nil {
		s.writeError(w, &service.BadRequest{Msg: "version must be an integer"})
		return
	}
	v, err := s.svc.GetWorkflowVersion(r.Context(), r.PathValue("name"), n)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleEnqueueRun(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Input map[string]any `json:"input"`
	}
	if err := readJSON(r, &in); err != nil {
		s.writeError(w, err)
		return
	}
	run, err := s.svc.EnqueueRun(r.Context(), r.PathValue("name"), in.Input, service.EnqueueOptions{})
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = r.URL.Query().Get("workflow")
	}
	runs, err := s.svc.ListRuns(r.Context(), name, r.URL.Query().Get("status"), queryInt(r, "limit", 50))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.svc.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.svc.CancelRun(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleStepLogs(w http.ResponseWriter, r *http.Request) {
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		s.writeError(w, &service.BadRequest{Msg: "step index must be an integer"})
		return
	}
	b, err := s.svc.StepLogs(r.Context(), r.PathValue("id"), idx, int64(queryInt(r, "tail", 0)))
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) handleListConnections(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.ListConnections(r.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"connections": out})
}

func (s *Server) handleUpsertConnection(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name   string            `json:"name"`
		Type   string            `json:"type"`
		Fields map[string]string `json:"fields"`
	}
	if err := readJSON(r, &in); err != nil {
		s.writeError(w, err)
		return
	}
	info, err := s.svc.UpsertConnection(r.Context(), in.Name, in.Type, in.Fields)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

func (s *Server) handleDeleteConnection(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteConnection(r.Context(), r.PathValue("name")); err != nil {
		s.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
