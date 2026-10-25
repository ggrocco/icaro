package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"icaro/internal/config"
	"icaro/internal/container"
	"icaro/internal/doctor"
	"icaro/internal/inject"
	"icaro/internal/secrets"
	"icaro/internal/store"
)

func init() {
	// After sessions (20): the injector authorises tokens against the
	// session spine and configures the egress proxies it built.
	RegisterWirer(25, "inject", func(_ context.Context, a *App) error {
		profiles := InjectProfiles(a.Cfg)
		if len(profiles) == 0 {
			return nil // ADR 0005 behaviour for every profile
		}
		uid, gid := a.Sessions.ContainerIDs()
		run := a.Layout.RunDir()
		a.Egress.InjectDir = func(profile string) string {
			if _, ok := profiles[profile]; ok {
				return container.InjectSocketDir(run, profile)
			}
			return ""
		}
		a.Egress.InjectUser = fmt.Sprintf("%d:%d", uid, gid)
		a.Add(&inject.Server{
			Profiles:   profiles,
			SocketPath: func(profile string) string { return InjectSocketPath(a.Cfg, profile) },
			Auth:       a.Sessions,
			Keys:       a.Secrets,
			Audit:      &inject.FileAudit{Dir: filepath.Join(a.Layout.Logs(), "inject"), Log: a.Log},
			Log:        a.Log,
			UID:        uid, GID: gid,
		})
		return nil
	})
}

// InjectProfiles returns the profiles that inject credentials (ADR 0028),
// each with its provider -> secret map.
func InjectProfiles(cfg *config.Config) map[string]map[string]string {
	out := map[string]map[string]string{}
	for name, p := range cfg.Profiles {
		if len(p.Inject) > 0 {
			out[name] = p.Inject
		}
	}
	return out
}

// InjectSocketPath is the host path of a profile's injection socket.
func InjectSocketPath(cfg *config.Config, profile string) string {
	return filepath.Join(container.InjectSocketDir(cfg.Layout().RunDir(), profile), container.InjectSocketName)
}

// InjectCheck warns about profiles whose credential injection cannot work:
// a secret that does not exist, a secret not bound to its provider's API
// host (the injector refuses to send it anywhere else), or a socket path
// over the Unix limit.
func InjectCheck(cfg *config.Config, st *store.Store) doctor.Check {
	return doctor.Func("credential injection", func(ctx context.Context) doctor.Result {
		profiles := InjectProfiles(cfg)
		if len(profiles) == 0 {
			return doctor.Result{Severity: doctor.OK, Detail: "no profile injects credentials"}
		}
		names := make([]string, 0, len(profiles))
		for n := range profiles {
			names = append(names, n)
		}
		sort.Strings(names)
		var probs []string
		for _, name := range names {
			if p := InjectSocketPath(cfg, name); len(p) >= inject.MaxSocketPath {
				probs = append(probs, fmt.Sprintf("profile %s: socket %s is over the %d-byte Unix limit; use a shorter ICARO_HOME", name, p, inject.MaxSocketPath-1))
			}
			provs := make([]string, 0, len(profiles[name]))
			for prov := range profiles[name] {
				provs = append(provs, prov)
			}
			sort.Strings(provs)
			for _, prov := range provs {
				secret := profiles[name][prov]
				host := providerHost(prov)
				row, err := st.GetSecret(ctx, secret)
				switch {
				case errors.Is(err, store.ErrNotFound):
					probs = append(probs, fmt.Sprintf("profile %s injects %s from secret %s, which does not exist; run `icaro secret set %s --host %s`", name, prov, secret, secret, host))
				case err != nil:
					return doctor.Result{Severity: doctor.Error, Detail: err.Error()}
				case !secrets.HostAllowed(row.Hosts, host):
					probs = append(probs, fmt.Sprintf("profile %s: secret %s is not bound to %s, so it is never sent; run `icaro secret set %s --host %s`", name, secret, host, secret, host))
				}
			}
		}
		if len(probs) > 0 {
			return doctor.Result{Severity: doctor.Warning, Detail: strings.Join(probs, "; ")}
		}
		return doctor.Result{Severity: doctor.OK, Detail: "profiles " + strings.Join(names, ", ") + " inject credentials; every secret exists and is host-bound"}
	})
}

func providerHost(provider string) string {
	// The upstreams are constants; config validation refuses unknown providers.
	u, _ := url.Parse(inject.Providers[provider].Upstream)
	return u.Hostname()
}
