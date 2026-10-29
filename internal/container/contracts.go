// Package container builds and runs the containers Ícaro creates (agent
// session containers and workflow step containers) under the mandatory
// baseline in blueprint/docs/03-security.md.
package container

import "context"

// Attachment confines a container to a profile's egress allowlist: the
// container joins Network (an internal network with no route out) and
// reaches the allowlisted hosts only through the proxy named in Env.
type Attachment struct {
	Network string
	Env     []string // HTTP_PROXY / HTTPS_PROXY / NO_PROXY
	// Inject is the base URL of the proxy's credential injection relay
	// (http://icaro-egress:3129), set only when the profile injects
	// credentials (ADR 0028).
	Inject string `json:"Inject,omitempty"`
	// Hosts maps the proxy's name to its address on Network (host:ip, for
	// /etc/hosts). Docker's DNS answers through iptables rules in the
	// container's network namespace, which gVisor's network stack does not
	// go through, so containers on a runtime other than Docker's default
	// find the proxy here (ADR 0029).
	Hosts []string `json:"Hosts,omitempty"`
}

// Egress provisions the per-profile egress confinement (ADR 0005).
type Egress interface {
	Ensure(ctx context.Context, profile string, allow []string) (Attachment, error)
}
