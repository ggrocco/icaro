// Package session owns the core primitive (ADR 0002): a unit of agent work
// with a worktree, a container, an optional preview and a memory scope.
// It is the only component that creates git worktrees.
//
// This file fixes the contract other components code against; the manager
// implementation lives beside it.
package session

import (
	"context"
	"time"
)

// Status values (ADR 0006).
const (
	StatusCreating = "creating"
	StatusRunning  = "running"
	StatusQueued   = "queued"
	StatusStopped  = "stopped"
	StatusOrphaned = "orphaned"
	StatusError    = "error"
)

// Kinds of session.
const (
	// KindAgent is an interactive agent session (icaro session create / icaro claude).
	KindAgent = "agent"
	// KindRun backs one workflow run (ADR 0010): its steps share the workspace.
	KindRun = "run"
)

// Session is the persisted parent row (ADR 0006).
type Session struct {
	ID                string     `json:"id"`
	Kind              string     `json:"kind"`
	ProjectID         string     `json:"project_id"`
	Profile           string     `json:"profile"`
	Branch            string     `json:"branch,omitempty"`
	WorktreePath      string     `json:"worktree_path"`
	ContainerID       string     `json:"container_id,omitempty"`
	PreviewInstanceID string     `json:"preview_instance_id,omitempty"`
	RunID             string     `json:"run_id,omitempty"`
	Status            string     `json:"status"`
	Error             string     `json:"error,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	LastHeartbeat     *time.Time `json:"last_heartbeat,omitempty"`
}

// Identity is what a session presents to memory and MCP: scope is derived
// server-side from it, never from what an agent asks for (ADR 0004).
type Identity struct {
	SessionID string
	ProjectID string
	Branch    string
	Agent     string
}

// RunWorkspace is what the workflow engine needs from a run session.
type RunWorkspace struct {
	SessionID string
	// Dir is the host directory mounted at /workspace for steps without a
	// repository.
	Dir string
}

// RunSessions is the seam between the workflow engine and the session
// spine (ADR 0010). Implemented by Manager.
type RunSessions interface {
	// StartRun creates (or, after a restart, returns) the run session.
	StartRun(ctx context.Context, runID, profile string) (RunWorkspace, error)
	// RepoWorktree resolves repository ref@revision once per run and
	// returns a worktree directory shared by the run's steps.
	RepoWorktree(ctx context.Context, sessionID, ref, revision string) (string, error)
	// SetContainer records the step container currently running.
	SetContainer(ctx context.Context, sessionID, containerID string) error
	// FinishRun stops the session and cleans its worktrees unless keep.
	FinishRun(ctx context.Context, sessionID string, keep bool) error
}

// PreviewHook lets the preview manager follow worktrees created and
// removed by sessions (the container↔preview seam, architecture §7).
type PreviewHook interface {
	WorktreeAdded(ctx context.Context, sessionID, projectID, worktreePath string) (previewInstanceID string, err error)
	WorktreeRemoved(ctx context.Context, sessionID, worktreePath string) error
}

// MemoryAttacher serves the memory MCP server for one session on a Unix
// socket that the session bind-mounts into its agent container at
// /icaro/mcp.sock. Scope comes from id, never from the agent (ADR 0004).
// Inside the container the agent's MCP config runs
// `icaro mcp --socket /icaro/mcp.sock`, a stdio bridge (spike S3 fallback).
type MemoryAttacher interface {
	ServeSession(ctx context.Context, id Identity, socketPath string) (stop func(), err error)
}
