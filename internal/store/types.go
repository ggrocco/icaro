package store

import "time"

// Workflow statuses.
const (
	WorkflowActive   = "active"
	WorkflowDisabled = "disabled"
	WorkflowDraft    = "draft"
)

// Run statuses.
const (
	RunQueued    = "queued"
	RunRunning   = "running"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
	RunCancelled = "cancelled"
)

// Step statuses.
const (
	StepPending   = "pending"
	StepRunning   = "running"
	StepSucceeded = "succeeded"
	StepFailed    = "failed"
	StepSkipped   = "skipped"
)

// Trigger kinds.
const (
	TriggerAPI      = "api"
	TriggerWebhook  = "webhook"
	TriggerSchedule = "schedule"
)

// Token scopes, ordered by privilege.
const (
	ScopeRead  = "read"
	ScopeWrite = "write"
	ScopeAdmin = "admin"
)

// Workflow is a stored workflow definition at its current version.
type Workflow struct {
	ID              string
	Name            string
	Version         int
	Status          string
	SpecYAML        string
	SpecJSON        string
	WebhookToken    string
	WebhookSecretCT []byte
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// WorkflowVersion is one historical definition.
type WorkflowVersion struct {
	WorkflowID string
	Version    int
	SpecYAML   string
	Actor      string
	CreatedAt  time.Time
}

// Run is one execution of a workflow version.
type Run struct {
	ID                string
	WorkflowID        string
	WorkflowName      string
	WorkflowVersion   int
	Status            string
	TriggerKind       string
	InputJSON         string
	RootRunID         string
	ParentRunID       string
	Depth             int
	RerunOf           string
	LibraryHash       string
	ClaimedBy         string
	ClaimedAt         *time.Time
	HeartbeatAt       *time.Time
	StartedAt         *time.Time
	FinishedAt        *time.Time
	CancelRequestedAt *time.Time
	Error             string
	CreatedAt         time.Time
}

// RunStep is the persisted state of one step of a run.
type RunStep struct {
	RunID       string
	Idx         int
	Name        string
	Status      string
	Attempt     int
	ContainerID string
	ExitCode    *int
	OutputJSON  string
	LogPath     string
	LogSize     int64
	StartedAt   *time.Time
	FinishedAt  *time.Time
	Error       string
}

// Connection is a named, typed credential bundle (fields encrypted).
type Connection struct {
	ID         string
	Name       string
	Type       string
	FieldsCT   []byte
	FieldNames []string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// APIToken is a bearer token record (hash only).
type APIToken struct {
	ID         string
	Name       string
	TokenHash  string
	Scope      string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// RunFilter narrows ListRuns.
type RunFilter struct {
	WorkflowID string
	Status     string
	Limit      int
}
