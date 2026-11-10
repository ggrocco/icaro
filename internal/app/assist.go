package app

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"icaro/internal/api"
	"icaro/internal/assist"
	"icaro/internal/authoring"
	"icaro/internal/config"
	"icaro/internal/creds"
	"icaro/internal/doctor"
	"icaro/internal/mcp"
	"icaro/internal/memory"
	"icaro/skills"
)

func init() {
	// After authoring (70): the assistant serves the same read-only
	// authoring tools on each assist session's socket, plus its own draft
	// and approval-gated run tools (ADR 0035).
	RegisterWirer(71, "assistant", func(_ context.Context, a *App) error {
		if a.Sessions == nil {
			return nil
		}
		o := assist.Options{
			Store: a.Store, Config: a.Cfg, Sessions: a.Sessions, Workflows: a.Service, Events: a.Workflows.Events,
			Log: a.Log, LogsDir: filepath.Join(a.Layout.Logs(), "assist"),
		}
		if a.Secrets != nil {
			o.Secrets = a.Secrets
		}
		a.Assist = assist.New(o)
		deps := authoring.Deps{Workflows: a.Service}
		if hub := IntegrationHub(a); hub != nil {
			deps.Integrations = hub
		}
		if a.Secrets != nil {
			deps.Secrets = a.Secrets
		}
		srv := a.Assist.Server(buildVersion(), func(s *mcp.Server) {
			authoring.RegisterRead(s, deps)
			if a.Memory != nil {
				// The caller is read-only, so memory_write refuses.
				memory.RegisterTools(s, a.Memory)
			}
		})
		skill, _ := fs.ReadFile(skills.FS, "icaro-workflows/SKILL.md")
		a.Sessions.SetAssist(a.Assist.Attacher(srv), assist.Preamble(string(skill)))
		api.RegisterChats(a.API, a.Assist, a.Workflows.Events)
		a.Add(a.Assist)
		return nil
	})
}

// assistantHosts are the provider hosts an assistant profile's egress
// allowlist must reach when it signs in with a subscription (an inject
// profile reaches the provider through the daemon instead).
var assistantHosts = map[string][]string{
	"claude": {"api.anthropic.com"},
	"codex":  {"chatgpt.com"},
}

// assistantLogins are the files a subscription login leaves in a profile's
// seed home.
var assistantLogins = map[string][]string{
	"claude": {".claude/.credentials.json"},
	"codex":  {".codex/auth.json"},
}

// AssistantCheck reports whether the workflow assistant can run: a profile
// with a chat-capable agent, signed in (or injecting an API key), whose
// allowlist reaches the provider (ADR 0035).
func AssistantCheck(cfg *config.Config) doctor.Check {
	return doctor.Func("workflow assistant", func(context.Context) doctor.Result {
		name := cfg.AssistantProfile()
		prof, ok := cfg.Profiles[name]
		if !ok {
			return doctor.Result{Severity: doctor.Warning, Detail: "no profile runs claude or codex, so the workflow assistant is unavailable; add one or set assistant.profile"}
		}
		agent, err := creds.LookupAgent(prof.Agent)
		if err != nil || !agent.CanChat() {
			return doctor.Result{Severity: doctor.Warning, Detail: fmt.Sprintf("assistant profile %s runs %q, which has no headless chat mode", name, prof.Agent)}
		}
		if len(prof.Inject) > 0 {
			return doctor.Result{Severity: doctor.OK, Detail: fmt.Sprintf("profile %s (%s) with an injected API key; see the credential injection check", name, agent.Name)}
		}
		var probs []string
		seed := (&creds.Homes{Layout: cfg.Layout()}).SeedDir(name)
		signedIn := false
		for _, rel := range assistantLogins[agent.Name] {
			signedIn = signedIn || fileExists(filepath.Join(seed, rel))
		}
		if !signedIn {
			probs = append(probs, fmt.Sprintf("profile %s is not signed in; run `icaro login %s`", name, name))
		}
		for _, host := range assistantHosts[agent.Name] {
			if !slices.Contains(prof.Egress, host) {
				probs = append(probs, fmt.Sprintf("profile %s's egress allowlist does not reach %s", name, host))
			}
		}
		if len(probs) > 0 {
			return doctor.Result{Severity: doctor.Warning, Detail: strings.Join(probs, "; ")}
		}
		return doctor.Result{Severity: doctor.OK, Detail: fmt.Sprintf("profile %s (%s), signed in", name, agent.Name)}
	})
}
