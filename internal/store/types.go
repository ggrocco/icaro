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
	RunQueued  = "queued"
	RunRunning = "running"
	// RunWaiting is a run parked on approval or wait steps (ADR 0030): it
	// holds no engine slot until it is due (WakeAt) or decided.
	RunWaiting   = "waiting"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
	RunCancelled = "cancelled"
)

// Step statuses.
const (
	StepPending = "pending"
	StepRunning = "running"
	// StepWaiting is an approval or wait step that has started waiting.
	StepWaiting   = "waiting"
	StepSucceeded = "succeeded"
	StepFailed    = "failed"
	StepSkipped   = "skipped"
	StepCancelled = "cancelled"
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
	ID              string
	WorkflowID      string
	WorkflowName    string
	WorkflowVersion int
	Status          string
	TriggerKind     string
	// TriggerJSON is the normalized ${{ trigger.* }} payload.
	TriggerJSON       string
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
	// IdempotencyKey deduplicates enqueues (webhook deliveries, cron fires).
	IdempotencyKey string
	// Concurrency is the workflow's policy, copied at enqueue time.
	Concurrency string
	SessionID   string
	DeliveryID  string
	Actor       string
	// WakeAt is when the engine next looks at a waiting run; nil waits for
	// a decision only.
	WakeAt *time.Time
}

// StepRun is the persisted state of one step of a run, keyed by step id.
type StepRun struct {
	RunID       string
	StepID      string
	Idx         int // position in the plan's topological order
	Kind        string
	Status      string
	Attempt     int
	SessionID   string
	ContainerID string
	ExitCode    *int
	OutputsJSON string
	LogPath     string
	LogSize     int64
	LogLines    int64
	StartedAt   *time.Time
	FinishedAt  *time.Time
	Error       string
}

// LogChunk indexes a step attempt's log file: after flush Seq the file
// was FileOffset bytes long and held LineCount lines.
type LogChunk struct {
	RunID      string
	StepID     string
	Attempt    int
	Seq        int
	FileOffset int64
	LineCount  int64
	CreatedAt  time.Time
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

// Approval decisions (ADR 0030).
const (
	ApprovalApproved = "approved"
	ApprovalRejected = "rejected"
	ApprovalExpired  = "expired" // the step's deadline passed undecided
)

// Approval is the state of one approval step of a run: the message the
// approver sees and, once made, the decision.
type Approval struct {
	RunID     string
	StepID    string
	Message   string
	Decision  string // "" while pending
	Actor     string
	Comment   string
	CreatedAt time.Time
	DecidedAt *time.Time
}
