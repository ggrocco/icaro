package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"icaro/internal/app"
	"icaro/internal/config"
	"icaro/internal/daemon"
)

func init() { register(newServeCmd) }

func newServeCmd(opts *rootOpts) *cobra.Command {
	var logLevel, listen string
	var background bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the daemon (one per user; Unix socket, optional TCP with --listen)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}
			if listen != "" {
				cfg.Daemon.Listen = listen
			}
			if background {
				if err := daemon.StartBackground(cmd.Context(), cfg.Layout(), opts.daemonArgs(), 15*time.Second); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "daemon started; socket %s, log %s\n", cfg.Layout().Socket(), cfg.Layout().DaemonLog())
				return nil
			}
			var lvl slog.Level
			if err := lvl.UnmarshalText([]byte(logLevel)); err != nil {
				return fmt.Errorf("invalid --log-level %q", logLevel)
			}
			log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
			return runDaemon(cmd.Context(), cfg, log)
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "", "also listen on TCP (e.g. 127.0.0.1:8787); every TCP request needs an API token")
	cmd.Flags().BoolVar(&background, "background", false, "start the daemon detached and return once it answers")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "debug | info | warn | error")
	return cmd
}

// runDaemon holds the single-daemon lock for the lifetime of the process.
func runDaemon(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	inst, err := daemon.Acquire(cfg.Layout())
	if err != nil {
		return err
	}
	defer func() { _ = inst.Close() }()

	a, err := app.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 4)

	unixLn, err := inst.ListenUnix()
	if err != nil {
		return err
	}
	handler := a.Handler()
	servers := []*http.Server{newHTTPServer(handler, runCtx)}
	go serve(servers[0], unixLn, errCh)
	log.Info("daemon listening", "socket", cfg.Layout().Socket(), "pid", os.Getpid())

	if cfg.Daemon.Listen != "" {
		tcpLn, err := net.Listen("tcp", cfg.Daemon.Listen)
		if err != nil {
			return fmt.Errorf("listen %s: %w", cfg.Daemon.Listen, err)
		}
		srv := newHTTPServer(handler, runCtx)
		servers = append(servers, srv)
		go serve(srv, tcpLn, errCh)
		log.Info("daemon listening on TCP (token required)", "addr", tcpLn.Addr().String())
	}

	go func() { errCh <- a.Run(runCtx) }()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errCh:
		if err != nil {
			cancel()
			shutdown(servers)
			return err
		}
	}
	cancel()
	shutdown(servers)
	return nil
}

func newHTTPServer(h http.Handler, base context.Context) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return base },
		ConnContext:       app.ConnContext,
	}
}

func serve(srv *http.Server, ln net.Listener, errCh chan<- error) {
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		errCh <- err
	}
}

func shutdown(servers []*http.Server) {
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	for _, s := range servers {
		_ = s.Shutdown(sctx)
	}
}
