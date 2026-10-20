package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"icaro/internal/config"
	"icaro/internal/container"
	"icaro/internal/container/docker"
	"icaro/internal/creds"
	"icaro/internal/store"
)

func init() { register(newSetupCmd) }

func newSetupCmd(opts *rootOpts) *cobra.Command {
	var yes, skipImages, updateImages bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Create or update ~/.icaro: config, master key, database, profile homes and pinned images",
		Long: "setup is idempotent: it shows a diff of every change and asks before applying it.\n" +
			"Running it again changes nothing unless something is missing or you pass --update-images.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}
			s := &setup{cmd: cmd, cfg: cfg, yes: yes, out: cmd.OutOrStdout(), path: opts.configPath}
			if s.path == "" {
				s.path = cfg.Layout().ConfigFile()
			}
			return s.run(cmd.Context(), skipImages, updateImages)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "apply changes without asking (non-interactive)")
	cmd.Flags().BoolVar(&skipImages, "skip-images", false, "do not pull or pin profile images")
	cmd.Flags().BoolVar(&updateImages, "update-images", false, "pull the latest images and re-pin their digests")
	return cmd
}

type setup struct {
	cmd      *cobra.Command
	cfg      *config.Config
	yes      bool
	out      io.Writer
	path     string
	changed  int
	declined int
}

func (s *setup) consent(question string) (bool, error) {
	if s.yes {
		return true, nil
	}
	return confirm(s.cmd.InOrStdin(), s.cmd.ErrOrStderr(), question)
}

func (s *setup) run(ctx context.Context, skipImages, updateImages bool) error {
	l := s.cfg.Layout()
	if _, err := os.Stat(l.Root); errors.Is(err, os.ErrNotExist) {
		if ok, err := s.consent("Create " + l.Root + "?"); err != nil || !ok {
			return err
		}
		s.changed++
	}
	if err := l.Ensure(); err != nil {
		return err
	}
	if err := s.configFile(); err != nil {
		return err
	}
	// Reload: the file may have changed.
	cfg, err := config.Load(s.path)
	if err != nil {
		return err
	}
	s.cfg = cfg
	if _, info, err := loadMasterKey(s.cmd, cfg); err != nil {
		return err
	} else {
		fmt.Fprintf(s.out, "master key: %s\n", info.Backend)
	}
	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	if n, err := st.PendingMigrations(ctx); err != nil {
		return err
	} else if n > 0 {
		if err := st.Migrate(ctx); err != nil {
			return err
		}
		fmt.Fprintf(s.out, "database: applied %d migration(s)\n", n)
		s.changed++
	}
	homes := &creds.Homes{Layout: l}
	for _, name := range sortedProfiles(cfg) {
		if _, err := os.Stat(homes.SeedDir(name)); errors.Is(err, os.ErrNotExist) {
			if _, err := homes.EnsureSeed(name); err != nil {
				return err
			}
			fmt.Fprintf(s.out, "profile %s: created home %s\n", name, homes.SeedDir(name))
			s.changed++
		}
	}
	if !skipImages {
		if err := s.images(ctx, st, updateImages); err != nil {
			return err
		}
	}
	if s.declined > 0 {
		return fmt.Errorf("setup incomplete: %d change(s) declined", s.declined)
	}
	if s.changed == 0 {
		fmt.Fprintln(s.out, "everything is up to date")
	} else {
		fmt.Fprintln(s.out, "next: `icaro login claude`, then `icaro session create` in a repository")
	}
	return nil
}

// configFile writes the merged config after showing a diff.
func (s *setup) configFile() error {
	existing, err := os.ReadFile(s.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	desired, changed, err := DesiredConfig(existing)
	if err != nil {
		return fmt.Errorf("%s: %w", s.path, err)
	}
	if !changed {
		fmt.Fprintf(s.out, "config: %s is up to date\n", s.path)
		return nil
	}
	fmt.Fprintf(s.out, "config: proposed changes to %s:\n%s", s.path, LineDiff(string(existing), string(desired)))
	question := "Write " + s.path + "?"
	if len(existing) > 0 {
		question = "Overwrite " + s.path + " (a .bak copy is kept)?"
	}
	ok, err := s.consent(question)
	if err != nil || !ok {
		if err == nil {
			fmt.Fprintln(s.out, "config: left unchanged")
			s.declined++
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	if len(existing) > 0 {
		if err := os.WriteFile(s.path+".bak", existing, 0o600); err != nil {
			return err
		}
	}
	s.changed++
	return os.WriteFile(s.path, desired, 0o600)
}

// images pins every profile image by digest.
func (s *setup) images(ctx context.Context, st *store.Store, update bool) error {
	eng, err := docker.New(s.cfg.Docker.Host)
	if err != nil {
		return err
	}
	defer func() { _ = eng.Close() }()
	if err := eng.Ping(ctx); err != nil {
		fmt.Fprintf(s.out, "images: docker is not reachable (%v); skipped — run setup again once it is\n", err)
		return nil
	}
	imgs := &container.Images{Engine: eng, Pins: st}
	seen := map[string]bool{}
	for _, name := range sortedProfiles(s.cfg) {
		ref := s.cfg.Profiles[name].Image
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		pin, err := st.GetPinnedImage(ctx, ref)
		if err == nil && !update {
			continue
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		q := "Pull and pin " + ref + "?"
		if pin != nil {
			q = "Pull " + ref + " and re-pin (currently " + pin.Digest + ")?"
		}
		if ok, err := s.consent(q); err != nil || !ok {
			if err != nil {
				return err
			}
			s.declined++
			continue
		}
		d, err := imgs.Pin(ctx, ref, update)
		if err != nil {
			fmt.Fprintf(s.out, "images: %s: %v (sessions will retry on first use)\n", ref, err)
			continue
		}
		if pin == nil || pin.Digest != d {
			fmt.Fprintf(s.out, "images: pinned %s -> %s\n", ref, d)
			s.changed++
		}
	}
	return nil
}

func sortedProfiles(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Profiles))
	for n := range cfg.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

const configHeader = "# Ícaro configuration (written by `icaro setup`; edit freely).\n# Reference: blueprint/docs/01-architecture.md\n"

// DesiredConfig merges what setup requires into an existing config file:
// the schema version and every built-in profile. It reports changed=false
// (and returns existing untouched) when nothing is missing, so a re-run
// never rewrites a file the user formatted by hand.
func DesiredConfig(existing []byte) ([]byte, bool, error) {
	doc := map[string]any{}
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := yaml.Unmarshal(existing, &doc); err != nil {
			return nil, false, err
		}
		if doc == nil {
			doc = map[string]any{}
		}
	}
	changed := false
	if _, ok := doc["version"]; !ok {
		doc["version"] = config.CurrentVersion
		changed = true
	}
	profiles, _ := doc["profiles"].(map[string]any)
	if profiles == nil {
		profiles = map[string]any{}
	}
	for name, p := range config.DefaultProfiles() {
		if _, ok := profiles[name]; ok {
			continue
		}
		entry := map[string]any{"image": p.Image, "egress": p.Egress}
		if p.Agent != "" {
			entry["agent"] = p.Agent
		}
		profiles[name] = entry
		changed = true
	}
	doc["profiles"] = profiles
	if !changed {
		return existing, false, nil
	}
	var buf bytes.Buffer
	if len(existing) == 0 {
		buf.WriteString(configHeader)
	}
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, false, err
	}
	return buf.Bytes(), true, nil
}

// LineDiff renders a minimal line diff ("+"/"-" prefixed) of a -> b.
func LineDiff(a, b string) string {
	al, bl := splitLines(a), splitLines(b)
	// LCS table; config files are small.
	lcs := make([][]int, len(al)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(bl)+1)
	}
	for i := len(al) - 1; i >= 0; i-- {
		for j := len(bl) - 1; j >= 0; j-- {
			if al[i] == bl[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var sb strings.Builder
	i, j := 0, 0
	for i < len(al) || j < len(bl) {
		switch {
		case i < len(al) && j < len(bl) && al[i] == bl[j]:
			sb.WriteString("  " + al[i] + "\n")
			i, j = i+1, j+1
		case j < len(bl) && (i == len(al) || lcs[i][j+1] >= lcs[i+1][j]):
			sb.WriteString("+ " + bl[j] + "\n")
			j++
		default:
			sb.WriteString("- " + al[i] + "\n")
			i++
		}
	}
	return sb.String()
}

func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
