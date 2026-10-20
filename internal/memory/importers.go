package memory

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"icaro/internal/store"
)

// ImportItem is one memory produced by an importer.
type ImportItem struct {
	Key        string // stable key within the source
	Kind       string
	Title      string
	Body       string
	Source     string
	SourceLine int
	Metadata   map[string]any
	CreatedAt  *time.Time
}

// ImportEdge links two imported items by key.
type ImportEdge struct {
	FromKey, ToKey, Kind string
}

// Importer converts another tool's export into memories. Importers are
// adapters: Ícaro never embeds Graphify or ai-memory (architecture §8).
type Importer interface {
	Name() string
	Parse(data []byte) ([]ImportItem, []ImportEdge, error)
}

// Importers are the built-in adapters by format name.
var Importers = map[string]Importer{
	"graphify":  graphifyImporter{},
	"ai-memory": aiMemoryImporter{},
}

// ImportFormats lists the adapter names.
func ImportFormats() []string {
	out := make([]string, 0, len(Importers))
	for k := range Importers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ImportReport summarizes an import.
type ImportReport struct {
	Format    string `json:"format"`
	Imported  int    `json:"imported"`
	Unchanged int    `json:"unchanged"`
	Edges     int    `json:"edges"`
}

// Import loads an export into the caller's project. Re-importing the same
// export is a no-op.
func (e *Engine) Import(ctx context.Context, scope Scope, format string, data []byte) (*ImportReport, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	imp, ok := Importers[format]
	if !ok {
		return nil, fmt.Errorf("%w: unknown import format %q (have %s)", ErrInvalid, format, strings.Join(ImportFormats(), ", "))
	}
	items, edges, err := imp.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrInvalid, format, err)
	}
	origin := "import:" + imp.Name()
	existing, err := e.db.ListMemoryKeys(ctx, scope.ProjectID, origin)
	if err != nil {
		return nil, err
	}
	hashByID := map[string]string{}
	for _, k := range existing {
		hashByID[k.ID] = k.ContentHash
	}
	idOf := func(key string) string { return stableID(scope.ProjectID, origin, key) }
	keys := map[string]bool{}
	for _, it := range items {
		keys[it.Key] = true
	}
	edgesFrom := map[string][]store.MemoryEdge{}
	for _, ed := range edges {
		to := "ref:" + ed.ToKey
		if keys[ed.ToKey] {
			to = idOf(ed.ToKey)
		}
		edgesFrom[ed.FromKey] = append(edgesFrom[ed.FromKey], store.MemoryEdge{ToRef: to, Kind: orDefault(ed.Kind, "related")})
	}
	rep := &ImportReport{Format: format}
	now := e.now()
	var bundles []store.MemoryBundle
	for _, it := range items {
		if !validKind(it.Kind, AllKinds) {
			it.Kind = KindFinding
		}
		raw, _ := json.Marshal([]any{it, edgesFrom[it.Key]})
		sum := sha256.Sum256(raw)
		hash := hex.EncodeToString(sum[:])
		id := idOf(it.Key)
		if hashByID[id] == hash {
			rep.Unchanged++
			continue
		}
		meta, _ := json.Marshal(it.Metadata)
		created := now
		if it.CreatedAt != nil {
			created = it.CreatedAt.UTC()
		}
		title := strings.TrimSpace(it.Title)
		if title == "" {
			title = firstLine(it.Body)
		}
		if title == "" {
			title = it.Key
		}
		row := &store.MemoryRow{
			ID: id, ProjectID: scope.ProjectID, Origin: origin, Kind: it.Kind, Title: truncateRunes(title, maxTitle), Body: snippet([]byte(it.Body), maxBody),
			Source: orDefault(it.Source, origin+":"+it.Key), SourceLine: it.SourceLine, Link: "icaro://memory/" + id,
			ContentHash: hash, Metadata: string(meta), CreatedAt: created, UpdatedAt: now,
		}
		if it.Kind == KindHandoff {
			row.Status = StatusPending
		}
		b := bundle(row)
		b.Edges = edgesFrom[it.Key]
		rep.Edges += len(b.Edges)
		bundles = append(bundles, b)
		rep.Imported++
	}
	for i := 0; i < len(bundles); i += 200 {
		if err := e.db.PutMemories(ctx, bundles[i:min(i+200, len(bundles))]); err != nil {
			return nil, err
		}
	}
	return rep, nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// graphifyImporter reads a knowledge-graph export in node-link form
// (prototype; the assumed format is documented in docs/memory.md):
//
//	{"nodes":[{"id","label","type","source_file","source_location","description"}],
//	 "links":[{"source","target","relation"}]}
//
// "edges" is accepted for "links", and name/kind/file/line/summary as aliases.
type graphifyImporter struct{}

func (graphifyImporter) Name() string { return "graphify" }

func (graphifyImporter) Parse(data []byte) ([]ImportItem, []ImportEdge, error) {
	var doc struct {
		Nodes []map[string]any `json:"nodes"`
		Links []map[string]any `json:"links"`
		Edges []map[string]any `json:"edges"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, nil, err
	}
	if doc.Nodes == nil {
		return nil, nil, fmt.Errorf("no nodes array")
	}
	var items []ImportItem
	for i, n := range doc.Nodes {
		id := str(n, "id")
		if id == "" {
			return nil, nil, fmt.Errorf("node %d has no id", i)
		}
		typ := strings.ToLower(str(n, "type", "kind", "file_type"))
		kind := KindFinding
		switch typ {
		case "function", "method", "class", "module", "symbol", "struct", "interface", "type", "code":
			kind = KindSymbol
		case "file", "document", "paper":
			kind = KindFile
		case "decision", "concept":
			kind = KindDecision
		case "task", "todo":
			kind = KindTask
		case "adr":
			kind = KindADR
		}
		meta := map[string]any{"graph_type": typ}
		for k, v := range n {
			switch k {
			case "id", "label", "name", "type", "kind", "file_type", "source_file", "file", "source_location", "line", "description", "summary":
			default:
				meta[k] = v
			}
		}
		items = append(items, ImportItem{
			Key: id, Kind: kind, Title: orDefault(str(n, "label", "name"), id), Body: str(n, "description", "summary"),
			Source: str(n, "source_file", "file"), SourceLine: lineOf(n["source_location"], n["line"]), Metadata: meta,
		})
	}
	var edges []ImportEdge
	for _, l := range append(doc.Links, doc.Edges...) {
		from, to := str(l, "source", "from"), str(l, "target", "to")
		if from == "" || to == "" {
			continue
		}
		edges = append(edges, ImportEdge{FromKey: from, ToKey: to, Kind: str(l, "relation", "kind", "type")})
	}
	return items, edges, nil
}

// aiMemoryImporter reads an ai-memory style export (prototype): a JSON
// object {"memories":[...]}, a JSON array, or JSON lines, each memory with
// id, title, content, tags, namespace, tier, created_at and source.
type aiMemoryImporter struct{}

func (aiMemoryImporter) Name() string { return "ai-memory" }

func (aiMemoryImporter) Parse(data []byte) ([]ImportItem, []ImportEdge, error) {
	var recs []map[string]any
	trimmed := bytes.TrimSpace(data)
	switch {
	case len(trimmed) == 0:
		return nil, nil, fmt.Errorf("empty export")
	case trimmed[0] == '[':
		if err := json.Unmarshal(trimmed, &recs); err != nil {
			return nil, nil, err
		}
	case bytes.HasPrefix(trimmed, []byte(`{"memories"`)) || bytes.Contains(trimmed[:min(len(trimmed), 200)], []byte(`"memories"`)):
		var doc struct {
			Memories []map[string]any `json:"memories"`
		}
		if err := json.Unmarshal(trimmed, &doc); err != nil {
			return nil, nil, err
		}
		recs = doc.Memories
	default:
		sc := bufio.NewScanner(bytes.NewReader(trimmed))
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			var r map[string]any
			if err := json.Unmarshal(line, &r); err != nil {
				return nil, nil, err
			}
			recs = append(recs, r)
		}
	}
	var items []ImportItem
	var edges []ImportEdge
	for i, r := range recs {
		id := str(r, "id")
		if id == "" {
			id = "idx-" + strconv.Itoa(i)
		}
		tags := strs(r["tags"])
		kind := KindFinding
		for _, cand := range append([]string{strings.ToLower(str(r, "kind", "type"))}, tags...) {
			switch strings.ToLower(cand) {
			case KindDecision, KindTask, KindHandoff, KindFinding:
				kind = strings.ToLower(cand)
			}
		}
		meta := map[string]any{"tags": tags}
		for _, k := range []string{"namespace", "tier", "priority", "confidence"} {
			if v, ok := r[k]; ok {
				meta[k] = v
			}
		}
		var created *time.Time
		if s := str(r, "created_at"); s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				created = &t
			}
		}
		items = append(items, ImportItem{Key: id, Kind: kind, Title: str(r, "title"), Body: str(r, "content", "body", "text"), Source: str(r, "source"), Metadata: meta, CreatedAt: created})
		for _, rel := range strs(r["related"]) {
			edges = append(edges, ImportEdge{FromKey: id, ToKey: rel, Kind: "related"})
		}
	}
	return items, edges, nil
}

func str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
	}
	return ""
}

func strs(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// lineOf extracts a line number from "L12", "12", 12 or "file.go:12".
func lineOf(vals ...any) int {
	for _, v := range vals {
		switch x := v.(type) {
		case float64:
			return int(x)
		case string:
			s := x
			if i := strings.LastIndex(s, ":"); i >= 0 {
				s = s[i+1:]
			}
			s = strings.TrimPrefix(strings.TrimPrefix(s, "L"), "l")
			if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
				return n
			}
		}
	}
	return 0
}
