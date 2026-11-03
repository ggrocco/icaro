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
	info, err := s.svc.ApplyWorkflow(r.Context(), yaml, actor(r))
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

func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Trigger map[string]any `json:"trigger"`
	}
	if err := readJSON(r, &in); err != nil {
		s.writeError(w, err)
		return
	}
	run, err := s.svc.StartRun(r.Context(), r.PathValue("name"), in.Trigger, actor(r))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func actor(r *http.Request) string {
	if t := TokenFrom(r.Context()); t != nil {
		return "token:" + t.Name
	}
	return ""
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

// handleRerun starts a run again on the version it ran with.
func (s *Server) handleRerun(w http.ResponseWriter, r *http.Request) {
	run, err := s.svc.RerunRun(r.Context(), r.PathValue("id"), actor(r))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.svc.CancelRun(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// handleDecide approves or rejects a waiting approval step, recording the
// caller as the approver. The body is optional: {"comment": "..."}.
func (s *Server) handleDecide(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Comment string `json:"comment"`
		}
		if err := readJSON(r, &in); err != nil {
			s.writeError(w, err)
			return
		}
		run, err := s.svc.DecideApproval(r.Context(), r.PathValue("id"), r.PathValue("step"), approve, actor(r), in.Comment)
		if err != nil {
			s.writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, run)
	}
}

// handleStepLogs serves a step attempt's log as text: ?attempt= (default
// latest), ?offset= to continue from a byte offset, ?tail= for the last N
// bytes. X-Icaro-Log-Offset / X-Icaro-Log-Size describe the slice.
func (s *Server) handleStepLogs(w http.ResponseWriter, r *http.Request) {
	page, err := s.svc.StepLogs(r.Context(), r.PathValue("id"), r.PathValue("step"),
		queryInt(r, "attempt", 0), int64(queryInt(r, "offset", 0)), int64(queryInt(r, "tail", 0)))
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Icaro-Log-Offset", strconv.FormatInt(page.Offset, 10))
	w.Header().Set("X-Icaro-Log-Size", strconv.FormatInt(page.Size, 10))
	w.Header().Set("X-Icaro-Log-Attempt", strconv.Itoa(page.Attempt))
	_, _ = w.Write(page.Data)
}
