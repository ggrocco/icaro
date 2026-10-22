package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"icaro/internal/api"
	"icaro/internal/daemon"
	"icaro/internal/mcp"
	"icaro/internal/memory"
)

func init() { register(newMCPCmd) }

// ContainerMCPSocket is where a session's memory socket is mounted inside
// its agent container (session.MemoryAttacher).
const ContainerMCPSocket = "/icaro/mcp.sock"

func newMCPCmd(opts *rootOpts) *cobra.Command {
	var socket, project, agent string
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run the stdio MCP bridge to Ícaro (for Claude Code, Codex and other MCP clients)",
		Long: "Bridges an MCP client speaking stdio to Ícaro's MCP server.\n\n" +
			"On the host (no --socket) it connects to the daemon's /mcp endpoint, scoped to the project of --project\n" +
			"(default: the current directory's git repository). Inside a session container the agent config runs\n" +
			"`icaro mcp --socket " + ContainerMCPSocket + "`, whose scope is fixed by the session.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if socket != "" {
				if _, err := os.Stat(socket); err != nil {
					return fmt.Errorf("mcp socket: %w", err)
				}
				b := &mcp.Bridge{Client: unixClient(socket), URL: "http://icaro/mcp"}
				return runBridge(ctx, b)
			}
			header := http.Header{}
			if project != "" || isRepo(".") {
				pid, err := memory.ProjectIDForPath(orDot(project))
				if err != nil {
					return err
				}
				header.Set(api.HeaderProject, pid)
				header.Set(api.HeaderBranch, memory.GitBranch(orDot(project)))
			}
			if agent != "" {
				header.Set(api.HeaderAgent, agent)
			}
			cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}
			if cfg.Daemon.URL != "" {
				if cfg.Daemon.Token == "" {
					return errors.New("daemon.url is set but no token: set daemon.token or ICARO_DAEMON__TOKEN")
				}
				header.Set("Authorization", "Bearer "+cfg.Daemon.Token)
				return runBridge(ctx, &mcp.Bridge{Client: &http.Client{}, URL: cfg.Daemon.URL + "/mcp", Header: header})
			}
			l := cfg.Layout()
			if err := daemon.EnsureRunning(ctx, l, opts.daemonArgs()); err != nil {
				return fmt.Errorf("reach daemon: %w", err)
			}
			return runBridge(ctx, &mcp.Bridge{Client: daemon.HTTPClient(l, 0), URL: daemon.SocketBaseURL + "/mcp", Header: header})
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", "session MCP socket (inside agent containers: "+ContainerMCPSocket+")")
	cmd.Flags().StringVar(&project, "project", "", "project directory that scopes memory (default: current git repository)")
	cmd.Flags().StringVar(&agent, "agent", os.Getenv("ICARO_AGENT"), "agent name recorded on memories")
	return cmd
}

// runBridge serves stdin/stdout; stdout carries only JSON-RPC.
func runBridge(ctx context.Context, b *mcp.Bridge) error {
	err := b.Run(ctx, os.Stdin, os.Stdout)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func unixClient(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
}

func isRepo(dir string) bool {
	root, err := memory.ProjectRoot(dir)
	if err != nil {
		return false
	}
	// ProjectRoot resolves symlinks (macOS temp dirs live under one), so
	// compare against the resolved path.
	abs, _ := filepath.Abs(dir)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return root != abs || fileExists(filepath.Join(abs, ".git"))
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func orDot(p string) string {
	if p == "" {
		return "."
	}
	return p
}

func absPath(p string) (string, error) { return filepath.Abs(p) }
