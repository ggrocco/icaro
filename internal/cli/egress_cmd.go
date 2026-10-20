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
	var listen, allow string
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
			return egressproxy.ListenAndServe(cmd.Context(), listen, egressproxy.New(hosts, log))
		},
	}
	cmd.Flags().StringVar(&listen, "listen", ":3128", "listen address")
	cmd.Flags().StringVar(&allow, "allow", "", "comma-separated allowlist (host or *.domain)")
	return cmd
}
