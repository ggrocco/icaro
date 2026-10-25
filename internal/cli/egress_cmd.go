package cli

import (
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"icaro/internal/container/egressproxy"
)

func init() { register(newEgressProxyCmd) }

// newEgressProxyCmd is the internal command the egress helper container
// runs; users never call it directly.
func newEgressProxyCmd(*rootOpts) *cobra.Command {
	var listen, allow, injectListen, injectSocket string
	cmd := &cobra.Command{
		Use:    "egress-proxy",
		Short:  "Run the egress allowlist proxy (internal)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var hosts []string
			for _, h := range strings.Split(allow, ",") {
				if h = strings.TrimSpace(h); h != "" {
					hosts = append(hosts, h)
				}
			}
			log := slog.New(slog.NewTextHandler(os.Stderr, nil))
			p := egressproxy.New(hosts, log)
			if injectSocket != "" {
				return egressproxy.ServeWithInjection(cmd.Context(), listen, p, injectListen, egressproxy.NewForwarder(injectSocket, log))
			}
			return egressproxy.ListenAndServe(cmd.Context(), listen, p)
		},
	}
	cmd.Flags().StringVar(&listen, "listen", ":3128", "listen address")
	cmd.Flags().StringVar(&allow, "allow", "", "comma-separated allowlist (host or *.domain)")
	cmd.Flags().StringVar(&injectListen, "inject-listen", ":3129", "listen address of the credential injection relay")
	cmd.Flags().StringVar(&injectSocket, "inject-socket", "", "daemon injection socket; enables the relay (ADR 0028)")
	return cmd
}
