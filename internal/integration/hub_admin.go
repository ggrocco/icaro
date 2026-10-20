package integration

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"icaro/internal/secrets"
)

// CredentialSetter is the slice of the secret store used to add and list
// integration credentials.
type CredentialSetter interface {
	Set(ctx context.Context, name, value string, hosts []string) error
	List(ctx context.Context) ([]secrets.Info, error)
	Delete(ctx context.Context, name string) error
}

// PlanCredential returns the default binding for `<integration>.<cred>`.
func (h *Hub) PlanCredential(integration, cred string) (CredentialPlan, error) {
	l, ok := h.registry.Snapshot().Get(integration)
	if !ok {
		return CredentialPlan{}, fmt.Errorf("%w: integration %q", ErrNotFound, integration)
	}
	return PlanCredential(l.Manifest, cred, h.executor.Settings(integration))
}

// AddCredential stores an integration credential bound to hosts; nil
// hosts means the planned default binding.
func (h *Hub) AddCredential(ctx context.Context, set CredentialSetter, integration, cred, value string, hosts []string) (CredentialPlan, error) {
	plan, err := h.PlanCredential(integration, cred)
	if err != nil {
		return plan, err
	}
	if hosts != nil && !plan.Webhook {
		plan.Hosts = hosts
	}
	if !plan.Webhook && len(plan.Hosts) == 0 {
		return plan, errors.New("a credential used by actions needs at least one host to be bound to")
	}
	if err := set.Set(ctx, plan.Secret, value, plan.Hosts); err != nil {
		return plan, err
	}
	h.log.Info("integration credential stored", "secret", plan.Secret, "hosts", plan.Hosts)
	return plan, nil
}

// CredentialInfo is a stored integration credential, never its value.
type CredentialInfo struct {
	Name        string   `json:"name"`
	Integration string   `json:"integration"`
	Hosts       []string `json:"hosts"`
	UpdatedAt   string   `json:"updated_at"`
}

// Credentials lists stored secrets that belong to a loaded integration.
func (h *Hub) Credentials(ctx context.Context, set CredentialSetter) ([]CredentialInfo, error) {
	infos, err := set.List(ctx)
	if err != nil {
		return nil, err
	}
	snap := h.registry.Snapshot()
	var out []CredentialInfo
	for _, in := range infos {
		integ, _, ok := strings.Cut(in.Name, ".")
		if !ok {
			continue
		}
		if _, loaded := snap.Get(integ); !loaded {
			continue
		}
		out = append(out, CredentialInfo{Name: in.Name, Integration: integ, Hosts: in.Hosts, UpdatedAt: in.UpdatedAt.UTC().Format(time.RFC3339)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Status is the hub's health: packs, load errors, shadow warnings and
// webhook secrets that subscribed workflows still need.
type Status struct {
	Packs          []PackStatus `json:"packs"`
	Warnings       []string     `json:"warnings,omitempty"`
	Errors         []LoadError  `json:"errors,omitempty"`
	MissingSecrets []string     `json:"missing_webhook_secrets,omitempty"`
	LoadedAt       string       `json:"loaded_at"`
}

// Status reports the current snapshot.
func (h *Hub) Status(ctx context.Context) Status {
	snap := h.registry.Snapshot()
	st := Status{Packs: snap.Packs, Warnings: snap.Warnings, Errors: snap.Errors(), LoadedAt: snap.LoadedAt.UTC().Format(time.RFC3339)}
	if src := h.specSource(); src != nil && h.store != nil {
		st.MissingSecrets, _ = MissingWebhookSecrets(ctx, snap, src, h.store)
	}
	return st
}

// EditManifest validates and commits a manifest to the local pack, then
// reloads so the edit serves immediately.
func (h *Hub) EditManifest(ctx context.Context, name string, src []byte, actor string) (string, error) {
	commit, err := h.packs.EditManifest(ctx, name, src, actor)
	if err != nil {
		return "", err
	}
	return commit, h.registry.Reload(ctx)
}
