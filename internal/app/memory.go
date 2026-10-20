package app

import (
	"context"
	"runtime/debug"

	"icaro/internal/api"
	"icaro/internal/mcp"
	"icaro/internal/memory"
)

// HostInstructions are sent to host CLIs that connect to /mcp.
const HostInstructions = "Ícaro daemon tools. Workflow authoring: fetch get_workflow_schema and list_integrations first, check list_secrets, then validate_workflow until clean before create_workflow/update_workflow, run_workflow with a small trigger, and debug with get_run. Memory tools are scoped to the project the bridge was started in (icaro mcp --project <path>)."

func init() {
	RegisterWirer(40, "memory", func(_ context.Context, a *App) error {
		version := buildVersion()
		a.Memory = memory.New(a.Store, a.Log, version)
		a.Memory.SetSessionInstructions(a.Cfg.Memory.Preamble)
		// One host MCP server: memory tools now, authoring tools (Phase 4)
		// register into a.MCP from a later wirer.
		a.MCP = mcp.NewServer("icaro", version, HostInstructions)
		memory.RegisterTools(a.MCP, a.Memory)
		api.RegisterMemory(a.API, a.Memory, a.MCP)
		a.Add(a.Memory)
		if a.Sessions != nil {
			// Each agent session gets its own memory MCP socket, scoped to
			// the session's identity (ADR 0004, ADR 0019).
			a.Sessions.SetMemoryAttacher(a.Memory)
		}
		return nil
	})
}

func buildVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}
