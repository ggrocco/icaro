package cli

import (
	"fmt"
	"net/http"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"
)

func init() { register(newUICmd) }

func newUICmd(opts *rootOpts) *cobra.Command {
	var printOnly bool
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Open the web UI with a one-time sign-in link (loopback only)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			var out struct {
				URL     string `json:"url"`
				Expires int    `json:"expires_in_seconds"`
			}
			if err := c.Do(cmd.Context(), http.MethodPost, "/api/v1/ui/link", nil, &out); err != nil {
				return fmt.Errorf("%w (icaro ui talks to the local daemon socket)", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n(single use, valid for %ds)\n", out.URL, out.Expires)
			if !printOnly {
				_ = openBrowser(out.URL)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&printOnly, "print", false, "print the link instead of opening a browser")
	return cmd
}

func openBrowser(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	if _, err := exec.LookPath(name); err != nil {
		return err
	}
	return exec.Command(name, url).Start() //nolint:gosec // opens our own loopback URL
}
