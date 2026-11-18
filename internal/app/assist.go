package app

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"icaro/internal/api"
	"icaro/internal/assist"
	"icaro/internal/assist/distill"
	"icaro/internal/authoring"
	"icaro/internal/config"
	"icaro/internal/container"
	"icaro/internal/creds"
	"icaro/internal/doctor"
	"icaro/internal/mcp"
	"icaro/internal/memory"
	"icaro/internal/session"
	"icaro/internal/store"
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
		if a.Memory != nil {
			o.Memories = a.Memory
		}
		a.Assist = assist.New(o)
		// The memory distiller (ADR 0036): exact memories always, a small
		// local model when one is pulled or an endpoint is set.
		dcfg := a.Cfg.Memory.Distill
		models := distill.Store{Dir: a.Layout.Models()}
		ns := container.Namespace(a.Cfg.Home)
		src := distill.Source{
			Enabled: dcfg.Enabled, Endpoint: dcfg.Endpoint, EndpointModel: dcfg.EndpointModel,
			Model: dcfg.Model, Tier: distill.AutoTier(int64(min(session.TotalRAM(), 1<<62))), Store: models,
			Managed: distill.Managed{
				Docker: a.Docker, Models: models, SocketDir: filepath.Join(a.Layout.RunDir(), "distill"),
				User: a.Sessions.User(), CPUs: dcfg.CPUs, Name: "icaro-distill-" + ns, Key: ns,
			},
		}
		a.Add(assist.NewDistiller(a.Assist, a.Service, src))
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

// DistillCheck reports whether the memory distiller's model stage can run
// (ADR 0036): off, an endpoint that answers, or a pulled model whose
// SHA-256 matches its pin; and whether the last job failed.
func DistillCheck(cfg *config.Config, st *store.Store) doctor.Check {
	return doctor.Func("memory distiller", func(ctx context.Context) doctor.Result {
		d := cfg.Memory.Distill
		res := doctor.Result{Severity: doctor.OK}
		switch {
		case !d.Enabled:
			res.Detail = "the model stage is off; chats leave exact memories only"
		case d.Endpoint != "":
			c := &distill.Client{BaseURL: d.Endpoint, Model: d.EndpointModel}
			rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := c.Ready(rctx)
			cancel()
			if err != nil {
				return doctor.Result{Severity: doctor.Warning, Detail: fmt.Sprintf("memory.distill.endpoint %s does not answer: %v", d.Endpoint, err)}
			}
			res.Detail = "model server at " + d.Endpoint
		default:
			s := distill.Store{Dir: cfg.Layout().Models()}
			m, ok, err := s.Pick(d.Model, distill.AutoTier(int64(min(session.TotalRAM(), 1<<62))))
			switch {
			case err != nil:
				return doctor.Result{Severity: doctor.Error, Detail: "memory.distill.model: " + err.Error()}
			case !ok:
				res.Detail = "no model is pulled; chats leave exact memories only (icaro memory model pull adds the summaries)"
			default:
				if err := s.Verify(m); err != nil {
					return doctor.Result{Severity: doctor.Error, Detail: fmt.Sprintf("model %s: %v; run icaro memory model rm %s --yes, then pull it again", m.Name, err, m.Name)}
				}
				res.Detail = fmt.Sprintf("model %s (%s), pinned SHA-256 verified", m.Name, m.Tier)
			}
		}
		if st != nil {
			if j, err := st.LastDistill(ctx); err == nil && j.Status == store.DistillFailed {
				return doctor.Result{Severity: doctor.Warning, Detail: res.Detail + "; the last distill job failed: " + j.Error}
			}
		}
		return res
	})
}
