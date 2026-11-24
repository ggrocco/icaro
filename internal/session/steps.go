package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"icaro/internal/container"
	"icaro/internal/creds"
	"icaro/internal/store"
)

// The agent-step half of the manager (ADR 0037). Every attempt of an
// agent step gets a short-lived child session of kind step under its agent
// profile: a fresh home layer over the profile's seed, an injection token
// when the profile injects, and the memory socket. Because it is a running
// session of the profile, `icaro login` refuses to rewrite the seed under
// it. The workflow engine owns the container; FinishAgentStep releases
// everything and deletes the row.

// KindStep backs one attempt of a workflow agent step.
const KindStep = "step"

// AgentStepRequest asks for one agent-step attempt's facets.
type AgentStepRequest struct {
	// RunSessionID is the run's session; its project scopes memory.
	RunSessionID string
	RunID        string
	StepID       string
	Attempt      int
	Profile      string
	// Workspace is the host directory the step mounts at /workspace.
	Workspace string
	Memory    bool
	// Workflow and Repository (the step's rendered repository ref, if
	// any) scope the step's memory when the run has no project.
	Workflow   string
	Repository string
}

// memoryScope is an agent step's memory project, derived server-side
// (ADR 0037): the run's project; else the step's repository, so steps on
// one repository share memory across runs and workflows; else the
// workflow.
func memoryScope(parent *store.SessionRow, req AgentStepRequest) string {
	switch {
	case parent.ProjectID != "":
		return parent.ProjectID
	case req.Repository != "":
		return "repo:" + repoKey(req.Repository)
	}
	return "workflow:" + req.Workflow
}

// scopeFile records a step session's memory scope beside its socket, so a
// re-attach serves the same scope.
const scopeFile = "scope"

// AgentStep is what the engine needs to run one agent-step attempt.
type AgentStep struct {
	SessionID string
	Agent     string
	Home      container.HomeMount
	// Env is the session and provider environment (with inject, a
	// capability token and the relay's base URL, never the key).
	Env    []string
	Egress container.Attachment
	// MCPSocket is the host path of the memory socket ("" without
	// memory), mounted at creds.MCPSocket.
	MCPSocket string
	Runtime   string
}

// AgentSteps is the seam between the engine and the session spine for
// agent steps. Implemented by Manager.
type AgentSteps interface {
	// AgentImage resolves a profile's image to the digest pinned in the
	// images table.
	AgentImage(ctx context.Context, profile string) (string, error)
	StartAgentStep(ctx context.Context, req AgentStepRequest) (*AgentStep, error)
	// ResumeAgentStep returns the facets of a step session after a daemon
	// restart, serving its memory socket again.
	ResumeAgentStep(ctx context.Context, sessionID string) (*AgentStep, error)
	FinishAgentStep(ctx context.Context, sessionID string) error
}

var _ AgentSteps = (*Manager)(nil)

// AgentImage implements AgentSteps.
func (m *Manager) AgentImage(ctx context.Context, profile string) (string, error) {
	prof, ok := m.o.Config.Profiles[profile]
	if !ok {
		return "", invalid("unknown profile %q", profile)
	}
	digest, err := m.o.Images.Resolve(ctx, prof.Image)
	if err != nil {
		return "", fmt.Errorf("image %s: %w", prof.Image, err)
	}
	return digest, nil
}

// StartAgentStep implements AgentSteps.
func (m *Manager) StartAgentStep(ctx context.Context, req AgentStepRequest) (*AgentStep, error) {
	prof, ok := m.o.Config.Profiles[req.Profile]
	if !ok {
		return nil, invalid("unknown profile %q", req.Profile)
	}
	agent, err := creds.LookupAgent(prof.Agent)
	if err != nil {
		return nil, err
	}
	if !agent.CanStep() {
		return nil, invalid("profile %s: %v", req.Profile, creds.ErrNoStep)
	}
	parent, err := m.o.Store.GetSession(ctx, req.RunSessionID)
	if err != nil {
		return nil, fmt.Errorf("run session: %w", err)
	}
	row := &store.SessionRow{
		Kind: KindStep, ProjectID: parent.ProjectID, Profile: req.Profile, RunID: req.RunID,
		WorktreePath: req.Workspace, Status: StatusRunning,
	}
	if err := m.o.Store.CreateSession(ctx, row); err != nil {
		return nil, err
	}
	scope := ""
	if req.Memory {
		scope = memoryScope(parent, req)
	}
	step, err := m.agentStep(ctx, row, agent, true, scope)
	if err != nil {
		_ = m.FinishAgentStep(context.WithoutCancel(ctx), row.ID)
		return nil, err
	}
	return step, nil
}

// ResumeAgentStep implements AgentSteps.
func (m *Manager) ResumeAgentStep(ctx context.Context, sessionID string) (*AgentStep, error) {
	row, err := m.o.Store.GetSession(ctx, sessionID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if row.Kind != KindStep {
		return nil, invalid("session %s is not an agent-step session", sessionID)
	}
	agent, err := creds.LookupAgent(m.o.Config.Profiles[row.Profile].Agent)
	if err != nil {
		return nil, err
	}
	// The scope file outlives the daemon; its presence says the step was
	// started with memory.
	scope, _ := os.ReadFile(filepath.Join(m.icaroDir(row.ID), scopeFile))
	return m.agentStep(ctx, row, agent, false, string(scope))
}

// agentStep prepares (fresh) or re-derives (resume) a step session's
// facets. The home layer, the token and the socket belong to the row; a
// non-empty scope serves memory in that project.
func (m *Manager) agentStep(ctx context.Context, row *store.SessionRow, agent creds.Agent, fresh bool, scope string) (*AgentStep, error) {
	prof := m.o.Config.Profiles[row.Profile]
	att, err := m.o.Egress.Ensure(ctx, row.Profile, prof.Egress)
	if err != nil {
		return nil, err
	}
	home, err := m.o.Homes.Prepare(ctx, row.Profile, row.ID)
	if err != nil {
		return nil, err
	}
	out := &AgentStep{SessionID: row.ID, Agent: agent.Name, Home: home, Egress: att, Runtime: prof.Runtime,
		Env: []string{"ICARO_SESSION=" + row.ID, "ICARO_PROFILE=" + row.Profile}}
	withMCP := scope != "" && m.o.Memory != nil
	if fresh {
		if err := agent.Configure(m.o.Homes.SessionFiles(row.Profile, row.ID), withMCP, ""); err != nil {
			return nil, fmt.Errorf("configure %s: %w", agent.Name, err)
		}
		m.chownForContainer(filepath.Join(m.layout.Home(row.Profile), "sessions", row.ID))
		providerEnv, err := m.injectEnv(ctx, row, prof.Inject, att)
		if err != nil {
			return nil, err
		}
		out.Env = append(out.Env, providerEnv...)
	}
	// On resume the container already holds its env and token (the token
	// survives a restart in the store), so only the facets are re-derived.
	if withMCP {
		if err := os.MkdirAll(m.icaroDir(row.ID), 0o700); err != nil {
			return nil, err
		}
		m.chownForContainer(m.icaroDir(row.ID))
		if stop := m.mcp.take(row.ID); stop != nil {
			stop()
		}
		if err := os.WriteFile(filepath.Join(m.icaroDir(row.ID), scopeFile), []byte(scope), 0o600); err != nil {
			return nil, err
		}
		sock := MemorySocketPath(m.layout, row.ID)
		_ = os.Remove(sock)
		stop, err := m.o.Memory.ServeSession(ctx, Identity{SessionID: row.ID, ProjectID: scope, Agent: agent.Name}, sock)
		if err != nil {
			return nil, fmt.Errorf("memory socket: %w", err)
		}
		m.chownForContainer(sock)
		m.mcp.put(row.ID, stop)
		out.MCPSocket = sock
	}
	return out, nil
}

// FinishAgentStep implements AgentSteps: it revokes the token, stops the
// memory socket, discards the home layer and deletes the row.
func (m *Manager) FinishAgentStep(ctx context.Context, sessionID string) error {
	row, err := m.o.Store.GetSession(ctx, sessionID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.Kind != KindStep {
		return invalid("session %s is not an agent-step session", sessionID)
	}
	m.revokeInject(ctx, row.ID)
	if stop := m.mcp.take(row.ID); stop != nil {
		stop()
	}
	if err := m.o.Homes.Release(ctx, row.Profile, row.ID); err != nil {
		return err
	}
	_ = os.RemoveAll(m.icaroDir(row.ID))
	return m.o.Store.DeleteSession(ctx, row.ID)
}
