package preview

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"icaro/internal/doctor"
	"icaro/internal/store"
)

// NetworkCheck is "Proxy without a dedicated network" (03-security): the
// fix recreates the network and drops the proxy container so the daemon
// recreates it attached to the new network.
func NetworkCheck(e Engine) doctor.Check {
	return doctor.Func("preview proxy network", func(ctx context.Context) doctor.Result {
		ok, err := e.NetworkExists(ctx, Network)
		if err != nil {
			return doctor.Result{Severity: doctor.Error, Detail: "docker: " + err.Error()}
		}
		if ok {
			st, err := e.Inspect(ctx, ProxyName)
			switch {
			case err != nil:
				return doctor.Result{Severity: doctor.Error, Detail: err.Error()}
			case !st.Exists:
				return doctor.Result{Severity: doctor.OK, Detail: Network + " exists; proxy starts with the first preview"}
			case !st.Running:
				return doctor.Result{Severity: doctor.Warning, Detail: ProxyName + " is not running", Fix: func(ctx context.Context) (string, error) {
					return "started " + ProxyName, e.StartContainer(ctx, ProxyName)
				}}
			}
			return doctor.Result{Severity: doctor.OK, Detail: Network + " exists and " + ProxyName + " is running"}
		}
		return doctor.Result{Severity: doctor.Error, Detail: "the dedicated network " + Network + " is missing", Fix: func(ctx context.Context) (string, error) {
			if _, err := e.EnsureNetwork(ctx, Network, map[string]string{"icaro.managed": "preview"}); err != nil {
				return "", err
			}
			if err := e.RemoveContainer(ctx, ProxyName); err != nil {
				return "", err
			}
			return "recreated " + Network + " (the daemon recreates " + ProxyName + " on the next preview)", nil
		}}
	})
}

// ComposeCheck is "Dangerous compose" (03-security): it reports every
// preview blocked by the lint and shows the blocked fields.
func ComposeCheck(st interface {
	ListPreviews(ctx context.Context) ([]*store.PreviewInstance, error)
}) doctor.Check {
	return doctor.Func("preview compose lint", func(ctx context.Context) doctor.Result {
		list, err := st.ListPreviews(ctx)
		if err != nil {
			return doctor.Result{Severity: doctor.Error, Detail: err.Error()}
		}
		var blocked []string
		for _, p := range list {
			if p.Status == store.PreviewBlocked {
				blocked = append(blocked, fmt.Sprintf("%s: %s", p.WorktreePath, p.Error))
			}
		}
		if len(blocked) > 0 {
			return doctor.Result{Severity: doctor.Error, Detail: strings.Join(blocked, "\n       ") +
				"\n       fix the compose file, or allow a finding with `icaro preview allow <path> --finding <id> --yes`"}
		}
		return doctor.Result{Severity: doctor.OK, Detail: fmt.Sprintf("%d preview(s), none blocked", len(list))}
	})
}

// Resolver resolves hostnames (net.DefaultResolver in production).
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// DNSCheck is "Local DNS for .test misconfigured" (spike S7): a warning
// when previews.domain is test and probe.test does not resolve to
// loopback. It never edits the resolver.
func DNSCheck(domain string, r Resolver) doctor.Check {
	return doctor.Func("preview DNS ."+orLocalhost(domain), func(ctx context.Context) doctor.Result {
		if domain == "" || domain == "localhost" {
			return doctor.Result{Severity: doctor.OK, Detail: "*.localhost resolves to loopback without configuration"}
		}
		if r == nil {
			r = net.DefaultResolver
		}
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		host := "icaro-probe." + domain
		addrs, err := r.LookupHost(ctx, host)
		if err != nil || len(addrs) == 0 {
			return doctor.Result{Severity: doctor.Warning, Detail: fmt.Sprintf("%s does not resolve (%v); configure a local resolver for .%s or unset previews.domain to use *.localhost", host, err, domain)}
		}
		for _, a := range addrs {
			if ip := net.ParseIP(a); ip == nil || !ip.IsLoopback() {
				return doctor.Result{Severity: doctor.Warning, Detail: fmt.Sprintf("%s resolves to %s, not loopback; unset previews.domain to use *.localhost", host, a)}
			}
		}
		return doctor.Result{Severity: doctor.OK, Detail: host + " resolves to loopback"}
	})
}

func orLocalhost(d string) string {
	if d == "" {
		return "localhost"
	}
	return d
}
