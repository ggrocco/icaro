// Package authoring exposes workflow authoring to AI agents as MCP tools
// (architecture §10): the schema, integrations, secret names, the
// validator, create/update/get/list, run, and a size-capped run view. It
// is a thin layer over the service layer; there are no destructive tools
// and no tool ever returns a secret value.
package authoring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"icaro/internal/integration"
	"icaro/internal/mcp"
	"icaro/internal/secrets"
	"icaro/internal/service"
	"icaro/internal/workflow"
)

// Tool names.
const (
	ToolGetSchema       = "get_workflow_schema"
	ToolListIntegration = "list_integrations"
	ToolGetIntegration  = "get_integration"
	ToolListSecrets     = "list_secrets"
	ToolValidate        = "validate_workflow"
	ToolCreate          = "create_workflow"
	ToolUpdate          = "update_workflow"
	ToolGet             = "get_workflow"
	ToolList            = "list_workflows"
	ToolRun             = "run_workflow"
	ToolGetRun          = "get_run"
)

// Workflows is the part of the service layer the tools use.
type Workflows interface {
	ValidateWorkflow(ctx context.Context, yaml string) (*workflow.Spec, workflow.Issues, error)
	ApplyWorkflow(ctx context.Context, yaml, actor string) (*service.WorkflowInfo, error)
	GetWorkflow(ctx context.Context, name string) (*service.WorkflowInfo, error)
	ListWorkflows(ctx context.Context) ([]*service.WorkflowInfo, error)
	StartRun(ctx context.Context, name string, trigger map[string]any, actor string) (*service.RunInfo, error)
	GetRun(ctx context.Context, id string) (*service.RunInfo, error)
	StepLogs(ctx context.Context, runID, stepID string, attempt int, offset, tail int64) (*service.LogPage, error)
}

// Integrations is the part of the integration hub the tools use. A nil
// Integrations makes the integration tools report that none are loaded.
type Integrations interface {
	Integrations() []integration.IntegrationInfo
	Integration(name string) (*integration.IntegrationInfo, error)
	ActionInputSchemas() map[string]map[string]any
}

// SecretNames lists secret names and host bindings only.
type SecretNames interface {
	List(ctx context.Context) ([]secrets.Info, error)
}

// Deps are the tools' collaborators.
type Deps struct {
	Workflows    Workflows
	Integrations Integrations
	Secrets      SecretNames
}

// Log tail limits for get_run, so debugging does not flood the agent's
// context.
const (
	defaultTail = 4 << 10
	maxTail     = 32 << 10
)

var readOnly = &mcp.Annotations{ReadOnlyHint: mcp.Bool(true), DestructiveHint: mcp.Bool(false), IdempotentHint: mcp.Bool(true), OpenWorldHint: mcp.Bool(false)}

func writes(idempotent bool) *mcp.Annotations {
	return &mcp.Annotations{ReadOnlyHint: mcp.Bool(false), DestructiveHint: mcp.Bool(false), IdempotentHint: mcp.Bool(idempotent), OpenWorldHint: mcp.Bool(false)}
}

func schema(s string) json.RawMessage { return json.RawMessage(s) }

const (
	noArgs   = `{"type":"object","additionalProperties":false,"properties":{}}`
	yamlArgs = `{"type":"object","additionalProperties":false,"required":["yaml"],"properties":{"yaml":{"type":"string","description":"The complete workflow document (YAML or JSON)."}}}`
	nameArgs = `{"type":"object","additionalProperties":false,"required":["name"],"properties":{"name":{"type":"string"}}}`
)

// Register adds the authoring tools and resources to s.
func Register(s *mcp.Server, d Deps) {
	s.AddResource(mcp.Resource{
		URI: "icaro://schemas/workflow", Name: "workflow-schema", Title: "Workflow JSON Schema",
		Description: "The published workflow v1 contract. Re-read it before writing a workflow.", MimeType: "application/schema+json",
		Read: func(context.Context) (string, error) { return string(workflow.SchemaJSON()), nil },
	})
	s.AddResource(mcp.Resource{
		URI: "icaro://schemas/actions", Name: "action-input-schemas", Title: "Integration action input schemas",
		Description: "One JSON Schema per installed integration action (integration.action -> schema of its `input`).", MimeType: "application/json",
		Read: func(context.Context) (string, error) {
			out := map[string]map[string]any{}
			if d.Integrations != nil {
				out = d.Integrations.ActionInputSchemas()
			}
			b, err := json.MarshalIndent(out, "", "  ")
			return string(b), err
		},
	})

	s.AddTool(mcp.Tool{
		Name: ToolGetSchema, Title: "Get the workflow schema", InputSchema: schema(noArgs), Annotations: readOnly,
		Description: "Return the workflow v1 JSON Schema. Always fetch it before drafting; never write fields from memory.",
		Handler: func(context.Context, json.RawMessage) (*mcp.ToolResult, error) {
			return &mcp.ToolResult{Content: []mcp.Content{{Type: "text", Text: string(workflow.SchemaJSON())}}}, nil
		},
	})

	s.AddTool(mcp.Tool{
		Name: ToolListIntegration, Title: "List integrations", InputSchema: schema(noArgs), Annotations: readOnly,
		Description: "List installed integrations with their trigger events and actions. Use get_integration for an action's typed inputs.",
		Handler: func(context.Context, json.RawMessage) (*mcp.ToolResult, error) {
			out := []integration.IntegrationSummary{}
			if d.Integrations != nil {
				for _, i := range d.Integrations.Integrations() {
					out = append(out, i.IntegrationSummary)
				}
			}
			return mcp.JSONResult(map[string]any{"integrations": out}), nil
		},
	})

	s.AddTool(mcp.Tool{
		Name: ToolGetIntegration, Title: "Get an integration", InputSchema: schema(nameArgs), Annotations: readOnly,
		Description: "Return an integration's events (with the trigger.* fields they provide) and actions (typed inputs, outputs, hosts).",
		Handler: func(_ context.Context, args json.RawMessage) (*mcp.ToolResult, error) {
			var in struct {
				Name string `json:"name"`
			}
			if err := mcp.DecodeArgs(args, &in); err != nil {
				return nil, err
			}
			if d.Integrations == nil {
				return nil, errors.New("no integrations are loaded")
			}
			info, err := d.Integrations.Integration(in.Name)
			if err != nil {
				names := []string{}
				for _, i := range d.Integrations.Integrations() {
					names = append(names, i.Name)
				}
				return nil, fmt.Errorf("unknown integration %q (installed: %s)", in.Name, strings.Join(names, ", "))
			}
			return mcp.JSONResult(map[string]any{
				"name": info.Name, "description": info.Description, "pack": info.Pack, "hosts": info.Hosts, "auth": info.Auth,
				"events": info.EventsFull, "actions": info.ActionsFull,
			}), nil
		},
	})

	s.AddTool(mcp.Tool{
		Name: ToolListSecrets, Title: "List secret names", InputSchema: schema(noArgs), Annotations: readOnly,
		Description: "List secret names and the hosts each is bound to. Values are never returned. Ask the human to create missing ones (icaro secret set NAME, or icaro credential add <integration>).",
		Handler: func(ctx context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
			out := []map[string]any{}
			if d.Secrets != nil {
				infos, err := d.Secrets.List(ctx)
				if err != nil {
					return nil, err
				}
				for _, i := range infos {
					out = append(out, map[string]any{"name": i.Name, "hosts": i.Hosts})
				}
			}
			return mcp.JSONResult(map[string]any{"secrets": out}), nil
		},
	})

	s.AddTool(mcp.Tool{
		Name: ToolValidate, Title: "Validate a workflow", InputSchema: schema(yamlArgs), Annotations: readOnly,
		Description: "Validate a workflow without saving it. Returns structured issues: a JSON Pointer `path`, a `message` and often a `hint`. Fix exactly what each path points at and validate again until `valid` is true.",
		Handler: func(ctx context.Context, args json.RawMessage) (*mcp.ToolResult, error) {
			var in struct {
				YAML string `json:"yaml"`
			}
			if err := mcp.DecodeArgs(args, &in); err != nil {
				return nil, err
			}
			spec, issues, err := d.Workflows.ValidateWorkflow(ctx, in.YAML)
			if err != nil {
				return nil, err
			}
			out := map[string]any{"valid": len(issues) == 0, "issues": orEmpty(issues)}
			if spec != nil && spec.Name != "" {
				out["name"] = spec.Name
			}
			return mcp.JSONResult(out), nil
		},
	})

	apply := func(create bool) mcp.Handler {
		return func(ctx context.Context, args json.RawMessage) (*mcp.ToolResult, error) {
			caller, err := needWrite(ctx)
			if err != nil {
				return nil, err
			}
			var in struct {
				YAML string `json:"yaml"`
			}
			if err := mcp.DecodeArgs(args, &in); err != nil {
				return nil, err
			}
			spec, issues, err := d.Workflows.ValidateWorkflow(ctx, in.YAML)
			if err != nil {
				return nil, err
			}
			if len(issues) > 0 {
				return &mcp.ToolResult{IsError: true, Content: []mcp.Content{{Type: "text", Text: mustJSON(map[string]any{"error": "the workflow has issues; fix them with validate_workflow first", "issues": issues})}}}, nil
			}
			_, getErr := d.Workflows.GetWorkflow(ctx, spec.Name)
			exists := getErr == nil
			switch {
			case create && exists:
				return nil, fmt.Errorf("workflow %q already exists; use update_workflow", spec.Name)
			case !create && !exists:
				return nil, fmt.Errorf("workflow %q does not exist; use create_workflow", spec.Name)
			}
			info, err := d.Workflows.ApplyWorkflow(ctx, in.YAML, "mcp:"+principal(caller))
			if err != nil {
				return nil, err
			}
			return mcp.JSONResult(summary(info)), nil
		}
	}
	s.AddTool(mcp.Tool{
		Name: ToolCreate, Title: "Create a workflow", InputSchema: schema(yamlArgs), Annotations: writes(false),
		Description: "Create a new workflow from a document that validate_workflow accepts. Fails if a workflow with that name exists.",
		Handler:     apply(true),
	})
	s.AddTool(mcp.Tool{
		Name: ToolUpdate, Title: "Update a workflow", InputSchema: schema(yamlArgs), Annotations: writes(true),
		Description: "Save a new version of an existing workflow (the name in the document selects it). Earlier versions are kept.",
		Handler:     apply(false),
	})

	s.AddTool(mcp.Tool{
		Name: ToolGet, Title: "Get a workflow", InputSchema: schema(nameArgs), Annotations: readOnly,
		Description: "Return a workflow's current YAML, version, status and the trigger.* fields manual runs must supply.",
		Handler: func(ctx context.Context, args json.RawMessage) (*mcp.ToolResult, error) {
			var in struct {
				Name string `json:"name"`
			}
			if err := mcp.DecodeArgs(args, &in); err != nil {
				return nil, err
			}
			info, err := d.Workflows.GetWorkflow(ctx, in.Name)
			if err != nil {
				return nil, notFound("workflow", in.Name, err)
			}
			out := summary(info)
			out["yaml"] = info.YAML
			return mcp.JSONResult(out), nil
		},
	})

	s.AddTool(mcp.Tool{
		Name: ToolList, Title: "List workflows", InputSchema: schema(noArgs), Annotations: readOnly,
		Description: "List workflows with version, status, steps and triggers.",
		Handler: func(ctx context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
			ws, err := d.Workflows.ListWorkflows(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]map[string]any, 0, len(ws))
			for _, w := range ws {
				out = append(out, summary(w))
			}
			return mcp.JSONResult(map[string]any{"workflows": out}), nil
		},
	})

	s.AddTool(mcp.Tool{
		Name: ToolRun, Title: "Run a workflow", Annotations: writes(false),
		InputSchema: schema(`{"type":"object","additionalProperties":false,"required":["name"],"properties":{"name":{"type":"string"},"trigger":{"type":"object","description":"The trigger.* payload. Supply every field listed in get_workflow's trigger_fields.required."}}}`),
		Description: "Start a manual run. Returns the run id; follow it with get_run.",
		Handler: func(ctx context.Context, args json.RawMessage) (*mcp.ToolResult, error) {
			caller, err := needWrite(ctx)
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
			run, err := d.Workflows.StartRun(ctx, in.Name, in.Trigger, "mcp:"+principal(caller))
			if err != nil {
				return nil, notFound("workflow", in.Name, err)
			}
			return mcp.JSONResult(map[string]any{"run_id": run.ID, "status": run.Status, "workflow": run.Workflow, "version": run.Version}), nil
		},
	})

	s.AddTool(mcp.Tool{
		Name: ToolGetRun, Title: "Get a run", Annotations: readOnly,
		InputSchema: schema(`{"type":"object","additionalProperties":false,"required":["id"],"properties":{"id":{"type":"string"},"log_tail_bytes":{"type":"integer","minimum":0,"maximum":32768,"description":"Bytes of each step's log tail to include (default 4096)."},"step":{"type":"string","description":"Only include this step's log."}}}`),
		Description: "Return a run's status, per-step status, outputs and errors, and a size-capped tail of each step's log (secret values are redacted).",
		Handler: func(ctx context.Context, args json.RawMessage) (*mcp.ToolResult, error) {
			var in struct {
				ID       string `json:"id"`
				LogTail  *int64 `json:"log_tail_bytes"`
				OnlyStep string `json:"step"`
			}
			if err := mcp.DecodeArgs(args, &in); err != nil {
				return nil, err
			}
			tail := int64(defaultTail)
			if in.LogTail != nil {
				tail = min(max(*in.LogTail, 0), maxTail)
			}
			run, err := d.Workflows.GetRun(ctx, in.ID)
			if err != nil {
				return nil, notFound("run", in.ID, err)
			}
			steps := make([]map[string]any, 0, len(run.Steps))
			for _, st := range run.Steps {
				entry := map[string]any{"id": st.ID, "kind": st.Kind, "status": st.Status, "attempt": st.Attempt, "outputs": st.Outputs}
				if st.ExitCode != nil {
					entry["exit_code"] = *st.ExitCode
				}
				if st.Error != "" {
					entry["error"] = st.Error
				}
				if tail > 0 && st.LogSize > 0 && (in.OnlyStep == "" || in.OnlyStep == st.ID) {
					if page, err := d.Workflows.StepLogs(ctx, run.ID, st.ID, 0, 0, tail); err == nil {
						entry["log_tail"] = string(page.Data)
						entry["log_truncated"] = page.Offset > 0
					}
				}
				steps = append(steps, entry)
			}
			out := map[string]any{"id": run.ID, "workflow": run.Workflow, "version": run.Version, "status": run.Status, "trigger_kind": run.TriggerKind, "steps": steps}
			if run.Error != "" {
				out["error"] = run.Error
			}
			return mcp.JSONResult(out), nil
		},
	})
}

func summary(w *service.WorkflowInfo) map[string]any {
	return map[string]any{
		"name": w.Name, "description": w.Description, "version": w.Version, "status": w.Status,
		"steps": w.Steps, "triggers": w.Triggers, "trigger_fields": w.TriggerFields,
	}
}

func needWrite(ctx context.Context) (mcp.Caller, error) {
	c, _ := mcp.CallerFrom(ctx)
	if c.Scope == "read" {
		return c, errors.New("this token is read-only; creating, updating and running workflows needs write scope")
	}
	return c, nil
}

func principal(c mcp.Caller) string {
	if c.Principal != "" {
		return c.Principal
	}
	return "agent"
}

func notFound(what, name string, err error) error {
	if errors.Is(err, service.ErrNotFound) {
		return fmt.Errorf("%s %q not found", what, name)
	}
	return err
}

func orEmpty(is workflow.Issues) workflow.Issues {
	if is == nil {
		return workflow.Issues{}
	}
	sort.SliceStable(is, func(i, j int) bool { return is[i].Path < is[j].Path })
	return is
}

func mustJSON(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
