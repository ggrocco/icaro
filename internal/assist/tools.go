package assist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"icaro/internal/mcp"
	"icaro/internal/session"
	"icaro/internal/store"
)

// Tool names the assistant gets besides the read-only authoring tools.
const (
	ToolGetDraft = "get_draft"
	ToolPropose  = "propose_workflow"
	ToolRun      = "run_workflow"
)

// Instructions are sent to the assistant's CLI in initialize.
const Instructions = "Ícaro workflow assistant tools. Read the editor's draft with get_draft, check the schema and integrations, validate until clean, then propose_workflow with the complete YAML; a person applies it. run_workflow waits for a person to approve the run."

// Server builds the MCP server assist sessions are served: register adds
// the read-only authoring and memory tools (see app wiring), and the
// assistant's own draft and run tools are added here.
func (s *Service) Server(version string, register func(*mcp.Server)) *mcp.Server {
	srv := mcp.NewServer("icaro", version, Instructions)
	if register != nil {
		register(srv)
	}
	s.registerTools(srv)
	return srv
}

// Attacher serves srv on each assist session's socket.
func (s *Service) Attacher(srv *mcp.Server) session.AssistAttacher { return attacher{srv} }

type attacher struct{ srv *mcp.Server }

// ServeAssist implements session.AssistAttacher. The caller is read-only
// for every tool that checks scope (memory writes, the host authoring
// writes); the assistant's writes are its own tools below, which find the
// chat from the session id and never from arguments.
func (a attacher) ServeAssist(ctx context.Context, id session.Identity, socketPath string) (func(), error) {
	caller := mcp.Caller{ProjectID: id.ProjectID, Agent: id.Agent, SessionID: id.SessionID, Principal: "assist:" + id.SessionID, Scope: "read"}
	return mcp.ServeUnix(ctx, a.srv, caller, socketPath)
}

var (
	readOnly = &mcp.Annotations{ReadOnlyHint: mcp.Bool(true), DestructiveHint: mcp.Bool(false), IdempotentHint: mcp.Bool(true), OpenWorldHint: mcp.Bool(false)}
	writes   = &mcp.Annotations{ReadOnlyHint: mcp.Bool(false), DestructiveHint: mcp.Bool(false), IdempotentHint: mcp.Bool(false), OpenWorldHint: mcp.Bool(false)}
)

// callerChat resolves the chat of the calling assist session.
func (s *Service) callerChat(ctx context.Context) (*store.Chat, error) {
	c, _ := mcp.CallerFrom(ctx)
	if !strings.HasPrefix(c.Principal, "assist:") || c.SessionID == "" {
		return nil, errors.New("these tools answer only inside an assistant chat")
	}
	chat, err := s.o.Store.GetChatBySession(ctx, c.SessionID)
	if err != nil {
		return nil, errors.New("this assistant session has no chat")
	}
	return chat, nil
}

func (s *Service) registerTools(srv *mcp.Server) {
	srv.AddTool(mcp.Tool{
		Name: ToolGetDraft, Title: "Get the editor's draft", Annotations: readOnly,
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{}}`),
		Description: "Return the workflow draft open in the person's editor: its YAML, version, the validator's issues and who wrote it last. Read it before proposing changes; the person may have edited it since your last proposal.",
		Handler: func(ctx context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
			c, err := s.callerChat(ctx)
			if err != nil {
				return nil, err
			}
			out := map[string]any{"workflow": c.Workflow, "version": 0, "yaml": "", "issues": []any{}, "source": ""}
			d, err := s.o.Store.GetChatDraft(ctx, c.ID)
			switch {
			case errors.Is(err, store.ErrNotFound):
				out["note"] = "The editor is empty: draft a new workflow."
			case err != nil:
				return nil, err
			default:
				out["version"], out["yaml"], out["source"] = d.Version, d.YAML, d.Source
				out["issues"] = json.RawMessage(d.Issues)
			}
			return mcp.JSONResult(out), nil
		},
	})

	srv.AddTool(mcp.Tool{
		Name: ToolPropose, Title: "Propose a workflow", Annotations: writes,
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["yaml"],"properties":{"yaml":{"type":"string","description":"The complete workflow document (YAML)."},"summary":{"type":"string","maxLength":500,"description":"One or two sentences on what changed, shown to the person."}}}`),
		Description: "Put a complete workflow into the person's editor as a proposal. It is validated and shown as a diff; the person accepts or discards it and saves it themselves. Nothing is saved by this call. Validate first; a proposal with issues is still shown, with its issues.",
		Handler: func(ctx context.Context, args json.RawMessage) (*mcp.ToolResult, error) {
			c, err := s.callerChat(ctx)
			if err != nil {
				return nil, err
			}
			var in struct {
				YAML    string `json:"yaml"`
				Summary string `json:"summary"`
			}
			if err := mcp.DecodeArgs(args, &in); err != nil {
				return nil, err
			}
			if strings.TrimSpace(in.YAML) == "" {
				return nil, errors.New("yaml is empty")
			}
			if len(in.YAML) > maxDraft {
				return nil, fmt.Errorf("the workflow is over %d bytes", maxDraft)
			}
			d, err := s.putDraft(ctx, c.ID, in.YAML, "assistant")
			if err != nil {
				return nil, err
			}
			var issues []any
			_ = json.Unmarshal(d.Issues, &issues)
			out := map[string]any{"version": d.Version, "valid": len(issues) == 0, "issues": json.RawMessage(d.Issues),
				"note": "Shown to the person as a diff. It is not saved until they accept it and save it."}
			return mcp.JSONResult(out), nil
		},
	})

	srv.AddTool(mcp.Tool{
		Name: ToolRun, Title: "Run a workflow (needs approval)", Annotations: writes,
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["name"],"properties":{"name":{"type":"string","description":"A saved workflow."},"trigger":{"type":"object","description":"The trigger.* payload; supply every field get_workflow lists in trigger_fields.required."}}}`),
		Description: "Ask the person to start a manual run of a saved workflow. The call waits until they approve or decline it in the chat. On approval it returns the run id (follow it with get_run); on decline, do not ask again unless they tell you to.",
		Handler: func(ctx context.Context, args json.RawMessage) (*mcp.ToolResult, error) {
			c, err := s.callerChat(ctx)
			if err != nil {
				return nil, err
			}
			var in struct {
				Name    string         `json:"name"`
				Trigger map[string]any `json:"trigger"`
			}
			if err := mcp.DecodeArgs(args, &in); err != nil {
				return nil, err
			}
			if in.Trigger == nil {
				in.Trigger = map[string]any{}
			}
			w, err := s.o.Workflows.GetWorkflow(ctx, in.Name)
			if err != nil {
				return nil, fmt.Errorf("workflow %q is not saved; the person must save it before it can run", in.Name)
			}
			return s.gatedRun(ctx, c, w.Name, w.Version, in.Trigger)
		},
	})
}

// gatedRun waits for a person to decide, then starts the run.
func (s *Service) gatedRun(ctx context.Context, c *store.Chat, name string, version int, trigger map[string]any) (*mcp.ToolResult, error) {
	req, _ := json.Marshal(map[string]any{"workflow": name, "version": version, "trigger": trigger})
	ap := &store.ChatApproval{ChatID: c.ID, Tool: ToolRun, Request: s.redactor(ctx).RedactString(string(req)), ExpiresAt: s.o.Now().Add(s.o.Config.Assistant.ApprovalTimeout)}
	ch := make(chan string, 1)
	// Register before the row exists, so a decision cannot slip between.
	id := store.NewID()
	ap.ID = id
	s.waiters.do(func(m map[string]chan string) { m[id] = ch })
	defer s.waiters.do(func(m map[string]chan string) { delete(m, id) })
	if err := s.o.Store.CreateChatApproval(ctx, ap); err != nil {
		return nil, err
	}
	a := s.actor(c.ID)
	info := approvalInfo(ap)
	a.append(entry{Type: EntryApproval, Approval: &info})

	timer := time.NewTimer(s.o.Config.Assistant.ApprovalTimeout)
	defer timer.Stop()
	var decision string
	select {
	case decision = <-ch:
	case <-timer.C:
	case <-ctx.Done():
	}
	if decision == "" {
		// Expire it, unless a decision won the race.
		bg := context.WithoutCancel(ctx)
		if got, err := s.o.Store.DecideChatApproval(bg, id, DecisionExpired, "", ""); err == nil {
			info := approvalInfo(got)
			a.append(entry{Type: EntryApproval, Approval: &info})
			decision = DecisionExpired
		} else if got, gerr := s.o.Store.GetChatApproval(bg, id); gerr == nil {
			decision = got.Decision
		}
		if ctx.Err() != nil && decision != DecisionApproved {
			return nil, ctx.Err()
		}
	}
	switch decision {
	case DecisionApproved:
	case DecisionDeclined:
		return mcp.JSONResult(map[string]any{"status": "declined", "note": "The person declined this run. Do not ask again unless they tell you to."}), nil
	default:
		return mcp.JSONResult(map[string]any{"status": "expired", "note": "Nobody approved the run in time. Ask the person whether they want to run it."}), nil
	}
	got, _ := s.o.Store.GetChatApproval(context.WithoutCancel(ctx), id)
	decider := "someone"
	if got != nil && got.Actor != "" {
		decider = got.Actor
	}
	run, err := s.o.Workflows.StartRun(context.WithoutCancel(ctx), name, trigger, decider+" via assistant "+c.ID)
	if err != nil {
		return nil, fmt.Errorf("the run was approved but did not start: %w", err)
	}
	_ = s.o.Store.SetChatApprovalResult(context.WithoutCancel(ctx), id, run.ID)
	if got != nil {
		got.Result = run.ID
		info := approvalInfo(got)
		a.append(entry{Type: EntryApproval, Approval: &info})
	}
	return mcp.JSONResult(map[string]any{"status": "started", "run_id": run.ID, "workflow": run.Workflow, "version": run.Version}), nil
}
