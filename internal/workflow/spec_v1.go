package workflow

import (
	"context"
	"fmt"
	"strings"
)

// The types below are the blueprint workflow contract
// (blueprint/docs/schemas/workflow.schema.json). They replace the legacy
// Workflow/Step types above, which remain only until the Phase 2 engine
// lands.

// Spec is a workflow v1 document.
type Spec struct {
	Version     int               `json:"version" yaml:"version"`
	Name        string            `json:"name" yaml:"name"`
	Description string            `json:"description,omitempty" yaml:"description,omitempty"`
	Concurrency string            `json:"concurrency,omitempty" yaml:"concurrency,omitempty"`
	Env         map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	Triggers    []TriggerSpec     `json:"triggers,omitempty" yaml:"triggers,omitempty"`
	Steps       []StepSpec        `json:"steps" yaml:"steps"`
}

// Concurrency policies (architecture §6).
const (
	ConcurrencyForbid  = "forbid"
	ConcurrencyQueue   = "queue"
	ConcurrencyReplace = "replace"
	ConcurrencyAllow   = "allow"
)

// TriggerSpec is either a builtin trigger (Type manual|cron) or an
// integration trigger (Integration + Event).
type TriggerSpec struct {
	Type        string `json:"type,omitempty" yaml:"type,omitempty"`
	Cron        string `json:"cron,omitempty" yaml:"cron,omitempty"`
	Integration string `json:"integration,omitempty" yaml:"integration,omitempty"`
	Event       string `json:"event,omitempty" yaml:"event,omitempty"`
	Filter      string `json:"filter,omitempty" yaml:"filter,omitempty"`
}

// Builtin trigger types.
const (
	TriggerManual = "manual"
	TriggerCron   = "cron"
)

// RepositoryRef selects a repository resolved once per (ref, revision).
type RepositoryRef struct {
	Ref      string `json:"ref" yaml:"ref"`
	Revision string `json:"revision,omitempty" yaml:"revision,omitempty"`
}

// StepSpec is one DAG node: exactly one of Image or Uses.
type StepSpec struct {
	ID         string            `json:"id" yaml:"id"`
	Image      string            `json:"image,omitempty" yaml:"image,omitempty"`
	Uses       string            `json:"uses,omitempty" yaml:"uses,omitempty"`
	Needs      []string          `json:"needs,omitempty" yaml:"needs,omitempty"`
	Repository *RepositoryRef    `json:"repository,omitempty" yaml:"repository,omitempty"`
	Timeout    string            `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	Retry      int               `json:"retry,omitempty" yaml:"retry,omitempty"`
	Command    string            `json:"command,omitempty" yaml:"command,omitempty"`
	Env        map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	Input      map[string]any    `json:"input,omitempty" yaml:"input,omitempty"`
	Outputs    map[string]string `json:"outputs,omitempty" yaml:"outputs,omitempty"`
	When       string            `json:"when,omitempty" yaml:"when,omitempty"`
}

// Expression roots available to workflows (ADR 0009).
var (
	// RootsStep are the names a step's env/input/command/when may reference.
	RootsStep = []string{"steps", "trigger", "env", "secrets", "run", "workflow"}
	// RootsFilter are the names a trigger filter may reference.
	RootsFilter = []string{"trigger"}
)

// ActionInfo describes an integration action for validation and for
// rendering forms (compiled from its manifest, ADR 0007).
type ActionInfo struct {
	Ref         string               `json:"ref"` // integration.action
	Description string               `json:"description,omitempty"`
	Inputs      map[string]InputInfo `json:"inputs"`
	Outputs     []string             `json:"outputs"`
	Hosts       []string             `json:"hosts"`
}

// InputInfo is one typed action input.
type InputInfo struct {
	Type        string `json:"type"` // string | number | boolean | object | array
	Required    bool   `json:"required,omitempty"`
	Description string `json:"description,omitempty"`
	Default     any    `json:"default,omitempty"`
}

// EventInfo describes an integration trigger event.
type EventInfo struct {
	Integration string   `json:"integration"`
	Event       string   `json:"event"`
	Description string   `json:"description,omitempty"`
	Fields      []string `json:"fields"` // normalized trigger.* payload keys
}

// Catalog resolves integration actions and events. Implemented by the
// integration package; a nil Catalog skips those checks.
type Catalog interface {
	Action(ref string) (ActionInfo, bool)
	Event(integration, event string) (EventInfo, bool)
	ActionRefs() []string
	EventNames(integration string) []string
	Integrations() []string
}

// ActionExecutor runs a `uses:` step in-process (integration package).
type ActionExecutor interface {
	Execute(ctx context.Context, ref string, input map[string]any) (outputs map[string]any, err error)
}

// SecretResolver resolves ${{ secrets.<name> }} for image steps. Values
// never reach logs: the engine registers them with the log redactor.
// Hosts returns a secret's host binding: a bound secret is an integration
// credential, which only uses: actions may attach (03-security).
type SecretResolver interface {
	Resolve(ctx context.Context, name string) (string, error)
	Names(ctx context.Context) ([]string, error)
	Hosts(ctx context.Context, name string) ([]string, error)
}

// IntegrationCredential reports whether a secret belongs to an
// integration: it is bound to hosts, or it is named <integration>.<cred>
// for an installed integration (webhook verification secrets have no
// binding). Such a secret never enters a container or a template; the
// integration's uses: actions attach it through the manifest's auth.
func IntegrationCredential(name string, bound bool, integrations []string) bool {
	if bound {
		return true
	}
	integ, _, ok := strings.Cut(name, ".")
	return ok && contains(integrations, integ)
}

// IntegrationCredentialHint tells an author what to do instead.
func IntegrationCredentialHint(name string) string {
	if integ, _, ok := strings.Cut(name, "."); ok {
		return fmt.Sprintf("call %s through its uses: actions (icaro integration show %s), which attach the credential to its bound hosts", integ, integ)
	}
	return "call the integration through its uses: actions, which attach the credential to its bound hosts; store a container secret without --host"
}

// CronFields are the trigger.* fields of a cron run (scheduler.fire):
// the scheduled and actual fire times (RFC 3339) and whether the fire
// was more than a minute late.
var CronFields = []string{"fired_at", "late", "scheduled_at"}

// Trigger kinds recorded on runs.
const (
	RunTriggerManual      = "manual"
	RunTriggerCron        = "cron"
	RunTriggerIntegration = "integration"
	RunTriggerRedeliver   = "redeliver"
)

// EnqueueRequest asks the engine to start a run. The engine applies the
// workflow's concurrency policy and deduplicates on IdempotencyKey.
type EnqueueRequest struct {
	Workflow    string
	TriggerKind string
	// Trigger is the normalized ${{ trigger.* }} payload.
	Trigger        map[string]any
	IdempotencyKey string // e.g. "<integration>:<delivery id>" or "cron:<workflow>:<fire time>"
	Actor          string
	DeliveryID     string
}

// EnqueueResult reports what happened to an EnqueueRequest.
type EnqueueResult struct {
	RunID        string `json:"run_id,omitempty"`
	Deduplicated bool   `json:"deduplicated,omitempty"` // an earlier run had the same key
	Skipped      string `json:"skipped,omitempty"`      // reason when no run was created (e.g. concurrency forbid)
}

// Enqueuer is implemented by the workflow service; integrations and the
// scheduler depend only on this.
type Enqueuer interface {
	EnqueueRun(ctx context.Context, req EnqueueRequest) (EnqueueResult, error)
}

// SpecSource lists the active workflows, for trigger routing and cron.
type SpecSource interface {
	ActiveSpecs(ctx context.Context) ([]*Spec, error)
}
