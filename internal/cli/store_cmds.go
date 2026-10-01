package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/ggrocco/icaro/internal/config"
	"github.com/ggrocco/icaro/internal/crypto"
	"github.com/ggrocco/icaro/internal/store"
)

// openStore opens the configured database, creating the data directory.
func openStore(ctx context.Context, cfg *config.Config) (*store.Store, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	dsn := cfg.Database.DSN
	if cfg.Database.Driver == "sqlite" {
		dsn = cfg.SQLitePath()
	}
	return store.Open(ctx, store.Dialect(cfg.Database.Driver), dsn)
}

func newInitCmd(opts *rootOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create the data directory, master key and database schema",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}
			if _, err := crypto.LoadMasterKey(cfg.DataDir, true); err != nil {
				return err
			}
			s, err := openStore(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()
			if err := s.Migrate(cmd.Context()); err != nil {
				return err
			}
			if err := os.MkdirAll(cfg.LogsDir(), 0o700); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "initialized %s (%s)\n", cfg.DataDir, cfg.Database.Driver)
			fmt.Fprintln(cmd.OutOrStdout(), "next: icaro token create --name dev --scope admin")
			return nil
		},
	}
}

func newMigrateCmd(opts *rootOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply pending database migrations",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}
			s, err := openStore(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()
			return s.Migrate(cmd.Context())
		},
	}
}

func newTokenCmd(opts *rootOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage API tokens (direct database access)",
	}
	var name, scope string
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a token; the secret is printed once",
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch scope {
			case store.ScopeRead, store.ScopeWrite, store.ScopeAdmin:
			default:
				return fmt.Errorf("scope must be read, write or admin")
			}
			cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}
			s, err := openStore(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()
			plain, hash, err := crypto.NewToken()
			if err != nil {
				return err
			}
			if _, err := s.CreateToken(cmd.Context(), name, hash, scope); err != nil {
				if errors.Is(err, store.ErrConflict) {
					return fmt.Errorf("token %q already exists", name)
				}
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), plain)
			return nil
		},
	}
	create.Flags().StringVar(&name, "name", "", "token name (required)")
	create.Flags().StringVar(&scope, "scope", store.ScopeWrite, "read | write | admin")
	_ = create.MarkFlagRequired("name")

	list := &cobra.Command{
		Use:   "list",
		Short: "List tokens",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}
			s, err := openStore(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()
			toks, err := s.ListTokens(cmd.Context())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSCOPE\tCREATED\tLAST USED\tREVOKED")
			for _, t := range toks {
				last, revoked := "-", "-"
				if t.LastUsedAt != nil {
					last = t.LastUsedAt.Local().Format("2006-01-02 15:04")
				}
				if t.RevokedAt != nil {
					revoked = t.RevokedAt.Local().Format("2006-01-02 15:04")
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.Name, t.Scope, t.CreatedAt.Local().Format("2006-01-02 15:04"), last, revoked)
			}
			return tw.Flush()
		},
	}

	revoke := &cobra.Command{
		Use:   "revoke NAME",
		Short: "Revoke a token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}
			s, err := openStore(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()
			if err := s.RevokeToken(cmd.Context(), args[0]); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("no active token named %q", args[0])
				}
				return err
			}
			return nil
		},
	}
	cmd.AddCommand(create, list, revoke)
	return cmd
}
