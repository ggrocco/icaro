package integration

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"icaro/internal/config"
	"icaro/internal/doctor"
	"icaro/internal/store"
	"icaro/internal/workflow"
)

// DoctorChecks are the Phase 3 checks: pack load errors and missing
// webhook secrets for integrations that active workflows subscribe to.
// They read the packs and the database directly, so they work without
// the daemon.
func DoctorChecks(dir string, st *store.Store) []doctor.Check {
	return []doctor.Check{
		doctor.Func("integration packs", func(context.Context) doctor.Result {
			snap := NewRegistry(dir, nil).Snapshot()
			errs := snap.Errors()
			if len(errs) > 0 {
				var parts []string
				for _, e := range errs {
					parts = append(parts, fmt.Sprintf("%s/%s: %s", e.Pack, e.File, e.Error))
				}
				return doctor.Result{Severity: doctor.Error, Detail: strings.Join(parts, "; ")}
			}
			detail := fmt.Sprintf("%d integration(s) loaded", len(snap.Names()))
			if len(snap.Warnings) > 0 {
				detail += "; " + strings.Join(snap.Warnings, "; ")
			}
			return doctor.Result{Severity: doctor.OK, Detail: detail}
		}),
		doctor.Func("webhook secrets", func(ctx context.Context) doctor.Result {
			missing, err := MissingWebhookSecrets(ctx, NewRegistry(dir, nil).Snapshot(), StoreSpecs{Store: st}, st)
			if err != nil {
				return doctor.Result{Severity: doctor.Error, Detail: err.Error()}
			}
			if len(missing) > 0 {
				var cmds []string
				for _, m := range missing {
					integ, cred, _ := strings.Cut(m, ".")
					cmds = append(cmds, fmt.Sprintf("icaro credential add %s %s", integ, cred))
				}
				return doctor.Result{Severity: doctor.Error, Detail: "missing webhook secret(s) " + strings.Join(missing, ", ") + "; run: " + strings.Join(cmds, "; ")}
			}
			return doctor.Result{Severity: doctor.OK, Detail: "every subscribed integration has its webhook secret"}
		}),
	}
}

// MissingWebhookSecrets lists `<integration>.<credential>` webhook secrets
// that subscribed workflows need but the secret store lacks.
func MissingWebhookSecrets(ctx context.Context, snap *Snapshot, src workflow.SpecSource, st *store.Store) ([]string, error) {
	specs, err := src.ActiveSpecs(ctx)
	if err != nil {
		return nil, err
	}
	need := map[string]bool{}
	for _, s := range specs {
		for _, tr := range s.Triggers {
			if tr.Integration == "" {
				continue
			}
			l, ok := snap.Get(tr.Integration)
			if !ok || l.Manifest.Triggers == nil || l.Manifest.Triggers.Verify.Kind == VerifyURLToken {
				continue
			}
			need[WebhookCredential(l.Manifest)] = true
		}
	}
	var missing []string
	for name := range need {
		if _, err := st.GetSecret(ctx, name); errors.Is(err, store.ErrNotFound) {
			missing = append(missing, name)
		} else if err != nil {
			return nil, err
		}
	}
	sort.Strings(missing)
	return missing, nil
}

// WebhookCredential is the secret name a manifest's webhook verification uses.
func WebhookCredential(m *Manifest) string {
	cred := "webhook_secret"
	if m.Triggers != nil && m.Triggers.Verify.Credential != "" {
		cred = m.Triggers.Verify.Credential
	}
	return m.Name + "." + cred
}

// CredentialPlan says where a new credential may be sent.
type CredentialPlan struct {
	Secret string   `json:"secret"`
	Hosts  []string `json:"hosts"`
	// Webhook is true for a webhook verification secret: it is never sent
	// anywhere, so it gets no host binding.
	Webhook bool `json:"webhook"`
}

// PlanCredential validates that cred is used by the manifest and returns
// the default host binding: the manifest's hosts plus the user-configured
// base_url host.
func PlanCredential(m *Manifest, cred string, settings config.IntegrationConfig) (CredentialPlan, error) {
	if m.Triggers != nil && WebhookCredential(m) == m.Name+"."+cred && m.Triggers.Verify.Kind != VerifyURLToken {
		return CredentialPlan{Secret: m.Name + "." + cred, Hosts: []string{}, Webhook: true}, nil
	}
	var used []string
	for _, a := range m.Auth {
		if a.Credential != "" {
			used = append(used, a.Credential)
		}
		if a.Credential == cred {
			hosts := append([]string{}, m.Hosts...)
			if settings.BaseURL != "" {
				if u, err := url.Parse(settings.BaseURL); err == nil && u.Hostname() != "" && !hostDeclared(hosts, u.Hostname()) {
					hosts = append(hosts, strings.ToLower(u.Hostname()))
				}
			}
			sort.Strings(hosts)
			return CredentialPlan{Secret: m.Name + "." + cred, Hosts: hosts}, nil
		}
	}
	if m.Triggers != nil && m.Triggers.Verify.Kind != VerifyURLToken {
		used = append(used, strings.TrimPrefix(WebhookCredential(m), m.Name+"."))
	}
	sort.Strings(used)
	return CredentialPlan{}, invalid("integration %s uses no credential %q (it uses: %s)", m.Name, cred, orNone(used))
}
