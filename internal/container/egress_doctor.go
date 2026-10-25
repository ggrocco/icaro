package container

import (
	"context"
	"fmt"
	"strings"

	"icaro/internal/container/docker"
	"icaro/internal/doctor"
)

// NetworkEngine is what the egress network check needs (docker.Engine).
type NetworkEngine interface {
	Ping(ctx context.Context) error
	ListNetworks(ctx context.Context, prefix string) ([]docker.NetworkInfo, error)
	RemoveNetwork(ctx context.Context, name string) error
}

// EgressNetworkCheck reports every egress network (icaro-egress-*) that is
// not internal: a session container on it would reach the internet
// directly instead of through the egress proxy (ADR 0005). Sessions
// refuse to start on such a network; the fix removes the unused ones so
// the next session recreates them internal.
func EgressNetworkCheck(e NetworkEngine) doctor.Check {
	return doctor.Func("egress networks", func(ctx context.Context) doctor.Result {
		if err := e.Ping(ctx); err != nil {
			return doctor.Result{Severity: doctor.Skipped, Detail: "docker unreachable"}
		}
		nets, err := e.ListNetworks(ctx, NetworkName(""))
		if err != nil {
			return doctor.Result{Severity: doctor.Error, Detail: err.Error()}
		}
		var bad, inUse, unused []string
		for _, n := range nets {
			if n.Internal {
				continue
			}
			bad = append(bad, n.Name)
			if n.Containers > 0 {
				inUse = append(inUse, fmt.Sprintf("%s (%d container(s))", n.Name, n.Containers))
			} else {
				unused = append(unused, n.Name)
			}
		}
		if len(bad) == 0 {
			return doctor.Result{Severity: doctor.OK, Detail: fmt.Sprintf("%d egress network(s), all internal", len(nets))}
		}
		res := doctor.Result{Severity: doctor.Error, Detail: "not internal, so containers bypass the egress proxy: " + strings.Join(bad, ", ")}
		if len(inUse) > 0 {
			res.Detail += "; stop the sessions on " + strings.Join(inUse, ", ") + " and run `docker network rm` on them"
		}
		if len(unused) > 0 {
			res.Fix = func(ctx context.Context) (string, error) {
				for _, n := range unused {
					if err := e.RemoveNetwork(ctx, n); err != nil {
						return "", err
					}
				}
				return "removed " + strings.Join(unused, ", ") + "; the next session recreates them internal", nil
			}
		}
		return res
	})
}
