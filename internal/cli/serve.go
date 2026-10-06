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

	"icaro/internal/api"
	"icaro/internal/crypto"
	"icaro/internal/runner"
	"icaro/internal/runner/docker"
	"icaro/internal/service"
	"icaro/internal/store"
	"icaro/internal/workflow"
)

func schemaJSON() []byte { return workflow.SchemaJSON() }

func newServeCmd(opts *rootOpts) *cobra.Command {
	var role string
	var logLevel string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the API server and/or the runner",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}
			var lvl slog.Level
			if err := lvl.UnmarshalText([]byte(logLevel)); err != nil {
				return fmt.Errorf("invalid --log-level %q", logLevel)
			}
			log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

			withServer := role == "all" || role == "server"
			withRunner := role == "all" || role == "runner"
			if !withServer && !withRunner {
				return fmt.Errorf("--role must be all, server or runner")
			}

			ctx := cmd.Context()
			key, err := crypto.LoadMasterKey(cfg.DataDir, false)
			if err != nil {
				return err
			}
			st, err := openStore(ctx, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			if err := st.Migrate(ctx); err != nil {
				return err
			}

			var rn *runner.Runner
			var rc service.RunnerControl
			if withRunner {
				engine, err := docker.New(cfg.Runner.DockerHost)
				if err != nil {
					return err
				}
				defer func() { _ = engine.Close() }()
				runnerID, err := runnerID(ctx, st)
				if err != nil {
					return err
				}
				svcForRunner := service.New(service.Options{Store: st, Key: key, LogsDir: cfg.LogsDir(), MaxTimeout: cfg.Runner.Defaults.Timeout, Log: log})
				rn = runner.New(runner.Config{
					RunnerID:      runnerID,
					Concurrency:   cfg.Runner.Concurrency,
					PollInterval:  cfg.Runner.PollInterval,
					StepTimeout:   cfg.Runner.Defaults.Timeout,
					MemoryBytes:   cfg.Runner.Defaults.MemoryMB << 20,
					NanoCPUs:      int64(cfg.Runner.Defaults.CPUs * 1e9),
					Pids:          cfg.Runner.Defaults.PidsLimit,
					NoFile:        cfg.Runner.Defaults.NoFile,
					StrictRuntime: cfg.Runner.StrictRuntime,
					LogsDir:       cfg.LogsDir(),
				}, st, engine, svcForRunner, nil, log)
				rc = rn
			}
			svc := service.New(service.Options{Store: st, Key: key, Runner: rc, LogsDir: cfg.LogsDir(), MaxTimeout: cfg.Runner.Defaults.Timeout, Log: log})

			errCh := make(chan error, 2)
			runCtx, cancel := context.WithCancel(ctx)
			defer cancel()

			if withRunner {
				go func() {
					log.Info("runner started", "concurrency", cfg.Runner.Concurrency)
					errCh <- rn.Run(runCtx)
				}()
			}
			var httpSrv *http.Server
			if withServer {
				httpSrv = &http.Server{
					Addr:              cfg.Server.Listen,
					Handler:           api.New(svc, log).Handler(),
					ReadHeaderTimeout: 10 * time.Second,
					BaseContext:       func(net.Listener) context.Context { return runCtx },
				}
				go func() {
					log.Info("api listening", "addr", cfg.Server.Listen)
					if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
						errCh <- err
						return
					}
					errCh <- nil
				}()
			}

			select {
			case <-ctx.Done():
				log.Info("shutting down")
			case err := <-errCh:
				if err != nil {
					cancel()
					return err
				}
			}
			cancel()
			if httpSrv != nil {
				sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer scancel()
				_ = httpSrv.Shutdown(sctx)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&role, "role", "all", "all | server | runner")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "debug | info | warn | error")
	return cmd
}

// runnerID returns the persistent id for this runner, creating it once.
func runnerID(ctx context.Context, st *store.Store) (string, error) {
	if v := os.Getenv("ICARO_RUNNER_ID"); v != "" {
		return v, nil
	}
	id, err := st.GetMeta(ctx, "runner_id")
	if errors.Is(err, store.ErrNotFound) {
		host, _ := os.Hostname()
		id = host + "-" + store.NewID()[:8]
		return id, st.SetMeta(ctx, "runner_id", id)
	}
	return id, err
}
