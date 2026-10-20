package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"icaro/internal/apiclient"
	"icaro/internal/config"
	"icaro/internal/container/docker"
	"icaro/internal/doctor"
	"icaro/internal/memory"
	"icaro/internal/store"
)

func init() {
	register(newMemoryCmd)
	extraDoctorChecks = append(extraDoctorChecks, memoryDoctorChecks)
}

// memoryDoctorChecks reports memories whose TTL passed but that were not
// purged (the daemon purges them every few minutes while running).
func memoryDoctorChecks(_ context.Context, _ *config.Config, st *store.Store, _ *docker.Engine) []doctor.Check {
	return []doctor.Check{doctor.Func("memory expiry", func(ctx context.Context) doctor.Result {
		if n, err := st.PendingMigrations(ctx); err != nil || n > 0 {
			return doctor.Result{Severity: doctor.Skipped, Detail: "database migrations pending"}
		}
		expired, err := st.ExpiredMemoryIDs(ctx, time.Now(), 10000)
		if err != nil {
			return doctor.Result{Severity: doctor.Error, Detail: err.Error()}
		}
		n := 0
		for _, ids := range expired {
			n += len(ids)
		}
		if n == 0 {
			return doctor.Result{Severity: doctor.OK, Detail: "no expired memories pending purge"}
		}
		return doctor.Result{Severity: doctor.Warning, Detail: fmt.Sprintf("%d expired memories not yet purged", n), Fix: func(ctx context.Context) (string, error) {
			purged, err := memory.New(st, nil, "").Expire(ctx)
			return fmt.Sprintf("purged %d", purged), err
		}}
	})}
}

// projectFlags resolve the memory scope of a CLI command from a path.
type projectFlags struct {
	path string
	id   string
}

func (p *projectFlags) add(cmd *cobra.Command) {
	cmd.PersistentFlags().StringVar(&p.path, "project", ".", "project directory (its git repository defines the project)")
	cmd.PersistentFlags().StringVar(&p.id, "project-id", "", "project id (overrides --project)")
}

func (p *projectFlags) resolve() (string, error) {
	if p.id != "" {
		return p.id, nil
	}
	return memory.ProjectIDForPath(p.path)
}

func newMemoryCmd(opts *rootOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory",
		Short: "Read and write shared project memory (fallback for agents without MCP)",
		Long: "Shared memory holds decisions, tasks, findings and handoffs plus indexed files, symbols, ADRs and commits.\n" +
			"MCP-capable agents use it through `icaro mcp`; these commands are the HTTP/CLI fallback. See docs/memory.md.",
	}
	var pf projectFlags
	var asJSON bool
	pf.add(cmd)
	cmd.PersistentFlags().BoolVar(&asJSON, "json", false, "print JSON")

	project := &cobra.Command{
		Use:   "project [PATH]",
		Short: "Print the project id for a directory",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := pf.path
			if len(args) == 1 {
				p = args[0]
			}
			root, err := memory.ProjectRoot(p)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), map[string]string{"project": memory.ProjectIDForRoot(root), "root": root})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", memory.ProjectIDForRoot(root), root)
			return nil
		},
	}

	var kind, title, body, bodyFile, source, agent, supersedes string
	var ttl time.Duration
	write := &cobra.Command{
		Use:   "write --kind KIND --title TITLE",
		Short: "Record a decision, task, finding or handoff",
		RunE: func(cmd *cobra.Command, _ []string) error {
			pid, err := pf.resolve()
			if err != nil {
				return err
			}
			if bodyFile != "" {
				b, err := readInputArg(bodyFile)
				if err != nil {
					return err
				}
				body = string(b)
			}
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			m, err := c.WriteMemory(cmd.Context(), memory.RESTWrite{
				Project: pid, Branch: memory.GitBranch(pf.path), Worktree: memory.GitTopLevel(pf.path), Agent: agent, Origin: memory.OriginCLI,
				Kind: kind, Title: title, Body: body, Source: source, TTLSeconds: int64(ttl / time.Second), Supersedes: supersedes,
			})
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), m)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s %q\n", m.ID, m.Kind, m.Title)
			return nil
		},
	}
	write.Flags().StringVar(&kind, "kind", "", "decision | task | finding | handoff")
	write.Flags().StringVar(&title, "title", "", "short title")
	write.Flags().StringVar(&body, "body", "", "details")
	write.Flags().StringVar(&bodyFile, "body-file", "", "read the body from a file (- for stdin)")
	write.Flags().StringVar(&source, "source", "", "path, URL or commit this comes from")
	write.Flags().StringVar(&agent, "agent", os.Getenv("ICARO_AGENT"), "agent name to record")
	write.Flags().StringVar(&supersedes, "supersedes", "", "id of a memory this replaces")
	write.Flags().DurationVar(&ttl, "ttl", 0, "expire after this duration (e.g. 72h)")
	_ = write.MarkFlagRequired("kind")

	var kinds []string
	var branch, qAgent, since, sourcePrefix, status string
	var limit int
	query := &cobra.Command{
		Use:   "query [TEXT...]",
		Short: "Search memory (newest first when TEXT is empty)",
		RunE: func(cmd *cobra.Command, args []string) error {
			pid, err := pf.resolve()
			if err != nil {
				return err
			}
			q := apiclient.MemoryQuery{Project: pid, Text: strings.Join(args, " "), Kinds: kinds, Branch: branch, Agent: qAgent, SourcePrefix: sourcePrefix, Status: status, Limit: limit}
			if since != "" {
				t, err := parseSince(since)
				if err != nil {
					return err
				}
				q.Since = &t
			}
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			res, err := c.QueryMemory(cmd.Context(), q)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), res)
			}
			return printMemories(cmd.OutOrStdout(), res.Results)
		},
	}
	query.Flags().StringSliceVar(&kinds, "kind", nil, "filter by kind (repeatable)")
	query.Flags().StringVar(&branch, "branch", "", "this branch plus project-wide memories")
	query.Flags().StringVar(&qAgent, "agent", "", "filter by agent")
	query.Flags().StringVar(&since, "since", "", "only newer than a duration (24h) or RFC 3339 time")
	query.Flags().StringVar(&sourcePrefix, "source", "", "source prefix (e.g. internal/api/)")
	query.Flags().StringVar(&status, "status", "", "handoff status: pending | accepted")
	query.Flags().IntVar(&limit, "limit", 10, "maximum results")

	show := &cobra.Command{
		Use:   "show ID",
		Short: "Show a memory with its provenance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pid, err := pf.resolve()
			if err != nil {
				return err
			}
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			p, err := c.MemoryProvenance(cmd.Context(), pid, args[0])
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), p)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "%s  %s  %s\n%s\n\n", p.ID, p.Kind, p.Title, p.Body)
			fmt.Fprintf(w, "source:  %s\nlink:    %s\norigin:  %s\ncreated: %s\n", sourceWithLine(p.Source, p.SourceLine), p.Link, p.Origin, p.CreatedAt.Local().Format(time.RFC3339))
			if p.Scope.Branch != "" || p.Scope.Agent != "" || p.Scope.SessionID != "" {
				fmt.Fprintf(w, "scope:   branch=%s agent=%s session=%s\n", p.Scope.Branch, p.Scope.Agent, p.Scope.SessionID)
			}
			if p.Status != "" {
				fmt.Fprintf(w, "status:  %s %s\n", p.Status, p.AcceptedBy)
			}
			for _, e := range p.Edges {
				fmt.Fprintf(w, "edge:    %s -> %s\n", e.Kind, e.To)
			}
			return nil
		},
	}

	handoff := &cobra.Command{Use: "handoff", Short: "List and accept handoffs between sessions"}
	var hBranch string
	hList := &cobra.Command{
		Use:   "list",
		Short: "List pending handoffs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			pid, err := pf.resolve()
			if err != nil {
				return err
			}
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			res, err := c.PendingHandoffs(cmd.Context(), pid, hBranch)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), res)
			}
			return printMemories(cmd.OutOrStdout(), res.Results)
		},
	}
	hList.Flags().StringVar(&hBranch, "branch", "", "only this branch plus project-wide handoffs")
	var hAgent string
	hAccept := &cobra.Command{
		Use:   "accept ID",
		Short: "Accept a pending handoff",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pid, err := pf.resolve()
			if err != nil {
				return err
			}
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			m, err := c.AcceptHandoff(cmd.Context(), memory.RESTAccept{Project: pid, Agent: hAgent}, args[0])
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), m)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "accepted %s: %s\n\n%s\n", m.ID, m.Title, m.Body)
			return nil
		},
	}
	hAccept.Flags().StringVar(&hAgent, "agent", os.Getenv("ICARO_AGENT"), "who accepts")
	handoff.AddCommand(hList, hAccept)

	var commits int
	ingest := &cobra.Command{
		Use:   "ingest [PATH]",
		Short: "Index files, symbols, imports, ADRs and recent commits (deterministic, idempotent)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := pf.path
			if len(args) == 1 {
				dir = args[0]
			}
			abs, err := absPath(dir)
			if err != nil {
				return err
			}
			req := memory.RESTIngest{Path: abs, Commits: commits, Project: pf.id}
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			res, err := c.IngestMemory(cmd.Context(), req)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), res)
			}
			r := res.Report
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %d files, %d symbols, %d ADRs, %d commits indexed; %d unchanged, %d removed, %d skipped\n",
				res.Project, r.Files, r.Symbols, r.ADRs, r.Commits, r.Unchanged, r.Removed, r.Skipped)
			return nil
		},
	}
	ingest.Flags().IntVar(&commits, "commits", 0, "recent commits to index (default 200, -1 for none)")

	var format string
	imp := &cobra.Command{
		Use:   "import --format FORMAT FILE",
		Short: "Import another tool's export (" + strings.Join(memory.ImportFormats(), ", ") + "; prototype adapters)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pid, err := pf.resolve()
			if err != nil {
				return err
			}
			data, err := readInputArg(args[0])
			if err != nil {
				return err
			}
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			rep, err := c.ImportMemory(cmd.Context(), memory.RESTImport{Project: pid, Format: format, Data: string(data)})
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), rep)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "imported %d, unchanged %d, edges %d\n", rep.Imported, rep.Unchanged, rep.Edges)
			return nil
		},
	}
	imp.Flags().StringVar(&format, "format", "", "export format: "+strings.Join(memory.ImportFormats(), " | "))
	_ = imp.MarkFlagRequired("format")

	cmd.AddCommand(project, write, query, show, handoff, ingest, imp)
	return cmd
}

func parseSince(s string) (time.Time, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("--since must be a duration (24h) or RFC 3339 time")
	}
	return t, nil
}

func sourceWithLine(src string, line int) string {
	if line > 0 {
		return fmt.Sprintf("%s:%d", src, line)
	}
	return src
}

func printMemories(w io.Writer, ms []*memory.Memory) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tKIND\tTITLE\tSOURCE\tCREATED")
	for _, m := range ms {
		title := m.Title
		if len([]rune(title)) > 60 {
			title = string([]rune(title)[:57]) + "..."
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", m.ID, m.Kind, title, sourceWithLine(m.Source, m.SourceLine), m.CreatedAt.Local().Format("2006-01-02 15:04"))
	}
	return tw.Flush()
}
