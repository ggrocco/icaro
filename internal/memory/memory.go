// Package memory is Ícaro's shared project memory (architecture §8, ADR
// 0004): human/agent memories (decisions, tasks, findings, handoffs) and
// deterministically ingested project facts (files, symbols, ADRs,
// commits), retrieved with full-text search plus filters.
//
// Scope is strict: every read and write is bound to one project id taken
// from the caller's session identity, never from arguments. Every memory
// returned carries its source, a timestamp, its scope and a link.
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"icaro/internal/mcp"
	"icaro/internal/session"
	"icaro/internal/store"
)

// Kinds of memory.
const (
	KindDecision = "decision"
	KindTask     = "task"
	KindFinding  = "finding"
	KindHandoff  = "handoff"
	KindFile     = "file"
	KindSymbol   = "symbol"
	KindADR      = "adr"
	KindCommit   = "commit"
)

// WritableKinds are the kinds agents and humans write; the rest come from
// deterministic ingest.
var WritableKinds = []string{KindDecision, KindTask, KindFinding, KindHandoff}

// AllKinds lists every kind.
var AllKinds = []string{KindDecision, KindTask, KindFinding, KindHandoff, KindFile, KindSymbol, KindADR, KindCommit}

// Handoff statuses.
const (
	StatusPending  = "pending"
	StatusAccepted = "accepted"
)

// Origins.
const (
	OriginAgent  = "agent"
	OriginCLI    = "cli"
	OriginAPI    = "api"
	OriginIngest = "ingest"
)

// Limits.
const (
	maxTitle     = 300
	maxBody      = 64 << 10
	snippetRunes = 1000
	maxQueryTerm = 16
)

// ErrNotFound reports a memory that does not exist in the caller's scope.
var ErrNotFound = errors.New("memory not found")

// ErrInvalid wraps validation failures.
var ErrInvalid = errors.New("invalid memory request")

// ErrConflict reports an operation on a memory in the wrong state.
var ErrConflict = errors.New("memory is not in a state that allows this")

// Scope is who reads or writes. ProjectID is mandatory.
type Scope struct {
	ProjectID string `json:"project_id"`
	Branch    string `json:"branch,omitempty"`
	Worktree  string `json:"worktree,omitempty"`
	Agent     string `json:"agent,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	// Origin records the path a write came through (agent, cli, api, ...).
	Origin string `json:"-"`
}

// ScopeFromIdentity derives a scope from a session identity.
func ScopeFromIdentity(id session.Identity, origin string) Scope {
	return Scope{ProjectID: id.ProjectID, Branch: id.Branch, Agent: id.Agent, SessionID: id.SessionID, Origin: origin}
}

func (s Scope) check() error {
	if s.ProjectID == "" {
		return fmt.Errorf("%w: no project scope", ErrInvalid)
	}
	return nil
}

// Memory is the API view of a memory.
type Memory struct {
	ID         string         `json:"id"`
	Kind       string         `json:"kind"`
	Title      string         `json:"title"`
	Body       string         `json:"body,omitempty"`
	Truncated  bool           `json:"truncated,omitempty"`
	Source     string         `json:"source"`
	SourceLine int            `json:"source_line,omitempty"`
	Link       string         `json:"link"`
	Scope      Scope          `json:"scope"`
	Origin     string         `json:"origin"`
	Status     string         `json:"status,omitempty"`
	AcceptedBy string         `json:"accepted_by,omitempty"`
	AcceptedAt *time.Time     `json:"accepted_at,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	ExpiresAt  *time.Time     `json:"expires_at,omitempty"`
	Score      float64        `json:"score,omitempty"`
}

// WriteRequest creates a memory.
type WriteRequest struct {
	Kind       string         `json:"kind"`
	Title      string         `json:"title"`
	Body       string         `json:"body,omitempty"`
	Source     string         `json:"source,omitempty"`
	SourceLine int            `json:"source_line,omitempty"`
	Link       string         `json:"link,omitempty"`
	TTL        time.Duration  `json:"-"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	// Supersedes links this memory to an earlier one it replaces.
	Supersedes string `json:"supersedes,omitempty"`
}

// Query filters retrieval. Text is matched with full-text search; empty
// Text lists the newest matching memories.
type Query struct {
	Text         string     `json:"text,omitempty"`
	Kinds        []string   `json:"kinds,omitempty"`
	Branch       string     `json:"branch,omitempty"`
	Agent        string     `json:"agent,omitempty"`
	Since        *time.Time `json:"since,omitempty"`
	SourcePrefix string     `json:"source_prefix,omitempty"`
	Status       string     `json:"status,omitempty"`
	Limit        int        `json:"limit,omitempty"`
}

// Edge is an outgoing link of a memory.
type Edge struct {
	To   string `json:"to"`
	Kind string `json:"kind"`
}

// Provenance is the full record of one memory.
type Provenance struct {
	Memory
	ContentHash string `json:"content_hash,omitempty"`
	Edges       []Edge `json:"edges"`
}

// Engine implements memory on top of the store.
type Engine struct {
	db  *store.Store
	log *slog.Logger
	now func() time.Time
	// version is reported by the MCP servers.
	version string
	// sessionServer serves per-session sockets; built before serving.
	sessionServer *mcp.Server
}

// New returns an engine.
func New(db *store.Store, log *slog.Logger, version string) *Engine {
	if log == nil {
		log = slog.Default()
	}
	e := &Engine{db: db, log: log, now: func() time.Time { return time.Now().UTC() }, version: version}
	e.sessionServer = e.NewMCPServer(DefaultInstructions)
	return e
}

// DefaultInstructions is sent to agents on initialize when the daemon
// does not configure a preamble.
const DefaultInstructions = "Shared project memory for this Ícaro session. Before you start, call memory_query with kinds=[\"handoff\"] and status=\"pending\" and accept a relevant handoff with memory_provenance(accept=true). Record decisions and findings with memory_write. When you finish or stop, write a handoff with the state, decisions and next steps."

// SetClock overrides the clock (tests).
func (e *Engine) SetClock(now func() time.Time) { e.now = now }

func validKind(k string, kinds []string) bool {
	for _, x := range kinds {
		if x == k {
			return true
		}
	}
	return false
}

// Write stores a decision, task, finding or handoff stamped with scope.
func (e *Engine) Write(ctx context.Context, scope Scope, req WriteRequest) (*Memory, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if !validKind(req.Kind, WritableKinds) {
		return nil, fmt.Errorf("%w: kind must be one of %s", ErrInvalid, strings.Join(WritableKinds, ", "))
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		req.Title = firstLine(req.Body)
	}
	if req.Title == "" {
		return nil, fmt.Errorf("%w: title or body is required", ErrInvalid)
	}
	if utf8.RuneCountInString(req.Title) > maxTitle {
		return nil, fmt.Errorf("%w: title exceeds %d characters", ErrInvalid, maxTitle)
	}
	if len(req.Body) > maxBody {
		return nil, fmt.Errorf("%w: body exceeds %d bytes", ErrInvalid, maxBody)
	}
	if req.TTL < 0 {
		return nil, fmt.Errorf("%w: ttl must be positive", ErrInvalid)
	}
	if req.Supersedes != "" {
		if _, err := e.db.GetMemory(ctx, scope.ProjectID, req.Supersedes); err != nil {
			return nil, fmt.Errorf("%w: supersedes %s: not found in this project", ErrInvalid, req.Supersedes)
		}
	}
	now := e.now()
	row := &store.MemoryRow{
		ID: store.NewID(), ProjectID: scope.ProjectID, Branch: scope.Branch, Worktree: scope.Worktree, Agent: scope.Agent,
		SessionID: scope.SessionID, Origin: orDefault(scope.Origin, OriginAPI), Kind: req.Kind, Title: req.Title, Body: req.Body,
		Source: req.Source, SourceLine: req.SourceLine, Link: req.Link, CreatedAt: now, UpdatedAt: now,
	}
	if row.Source == "" {
		row.Source = defaultSource(scope)
	}
	if row.Link == "" {
		row.Link = "icaro://memory/" + row.ID
	}
	if req.Kind == KindHandoff {
		row.Status = StatusPending
	}
	if req.TTL > 0 {
		exp := now.Add(req.TTL)
		row.ExpiresAt = &exp
	}
	meta, err := encodeMeta(req.Metadata)
	if err != nil {
		return nil, err
	}
	row.Metadata = meta
	b := bundle(row)
	if req.Supersedes != "" {
		b.Edges = append(b.Edges, store.MemoryEdge{ToRef: req.Supersedes, Kind: "supersedes"})
	}
	if err := e.db.PutMemories(ctx, []store.MemoryBundle{b}); err != nil {
		return nil, err
	}
	return toMemory(row, false), nil
}

// bundle computes postings and document length for a row.
func bundle(row *store.MemoryRow) store.MemoryBundle {
	title, body := Tokenize(row.Title+" "+sourceTerms(row.Source)), Tokenize(row.Body)
	row.DocLen = len(title) + len(body)
	weighted := termCounts(body)
	for t, n := range termCounts(title) {
		weighted[t] += titleWeight * n
	}
	terms := make([]store.MemoryTerm, 0, len(weighted))
	for t, n := range weighted {
		terms = append(terms, store.MemoryTerm{Term: t, TF: n})
	}
	return store.MemoryBundle{Memory: row, Terms: terms}
}

// sourceTerms makes file paths searchable by their segments.
func sourceTerms(src string) string {
	if strings.Contains(src, "://") || strings.HasPrefix(src, "agent:") || strings.HasPrefix(src, "session:") {
		return ""
	}
	return strings.NewReplacer("/", " ", ".", " ", "-", " ").Replace(src)
}

func defaultSource(s Scope) string {
	switch {
	case s.SessionID != "":
		return "session:" + s.SessionID
	case s.Agent != "":
		return "agent:" + s.Agent
	}
	return orDefault(s.Origin, OriginAPI)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if utf8.RuneCountInString(s) > 120 {
		s = string([]rune(s)[:120])
	}
	return strings.TrimSpace(s)
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func encodeMeta(m map[string]any) (string, error) {
	if len(m) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("%w: metadata: %v", ErrInvalid, err)
	}
	if len(b) > 16<<10 {
		return "", fmt.Errorf("%w: metadata exceeds 16 KiB", ErrInvalid)
	}
	return string(b), nil
}

func toMemory(r *store.MemoryRow, snippet bool) *Memory {
	m := &Memory{
		ID: r.ID, Kind: r.Kind, Title: r.Title, Body: r.Body, Source: r.Source, SourceLine: r.SourceLine, Link: r.Link,
		Scope:  Scope{ProjectID: r.ProjectID, Branch: r.Branch, Worktree: r.Worktree, Agent: r.Agent, SessionID: r.SessionID},
		Origin: r.Origin, Status: r.Status, AcceptedBy: r.AcceptedBy, AcceptedAt: r.AcceptedAt,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, ExpiresAt: r.ExpiresAt,
	}
	if r.Metadata != "" && r.Metadata != "{}" {
		_ = json.Unmarshal([]byte(r.Metadata), &m.Metadata)
	}
	if snippet && utf8.RuneCountInString(m.Body) > snippetRunes {
		m.Body = string([]rune(m.Body)[:snippetRunes]) + "…"
		m.Truncated = true
	}
	return m
}

func (e *Engine) filter(scope Scope, q Query) (store.MemoryFilter, error) {
	for _, k := range q.Kinds {
		if !validKind(k, AllKinds) {
			return store.MemoryFilter{}, fmt.Errorf("%w: unknown kind %q", ErrInvalid, k)
		}
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	return store.MemoryFilter{
		ProjectID: scope.ProjectID, Kinds: q.Kinds, Branch: q.Branch, Agent: q.Agent, Since: q.Since,
		SourcePrefix: q.SourcePrefix, Status: q.Status, Now: e.now(), Limit: limit,
	}, nil
}

// Ranking parameters (ADR 0020).
const (
	bm25K1      = 1.2
	bm25B       = 0.75
	titleWeight = 2
	// postingCap bounds the postings read per query term: the highest-tf
	// postings of a very common term are the only ones that can rank.
	postingCap = 2000
)

// Query retrieves memories in scope. Results are ranked by BM25 when Text
// is set, newest first otherwise. Bodies are trimmed to a snippet.
func (e *Engine) Query(ctx context.Context, scope Scope, q Query) ([]*Memory, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	f, err := e.filter(scope, q)
	if err != nil {
		return nil, err
	}
	terms := uniqueTerms(q.Text, maxQueryTerm)
	if len(terms) == 0 {
		if strings.TrimSpace(q.Text) != "" {
			return []*Memory{}, nil
		}
		rows, err := e.db.ListMemories(ctx, f)
		if err != nil {
			return nil, err
		}
		out := make([]*Memory, 0, len(rows))
		for _, r := range rows {
			out = append(out, toMemory(r, true))
		}
		return out, nil
	}
	// Statistics are project-wide, so idf does not depend on filters.
	n, avgdl, err := e.db.MemoryCorpusStats(ctx, store.MemoryFilter{ProjectID: scope.ProjectID, Now: f.Now})
	if err != nil {
		return nil, err
	}
	var hits []store.TermHit
	df := map[string]int{}
	capped := false
	for _, t := range terms {
		posts, d, err := e.db.TermPostings(ctx, scope.ProjectID, t, postingCap)
		if err != nil {
			return nil, err
		}
		df[t] = d
		capped = capped || d > len(posts)
		hits = append(hits, posts...)
	}
	if len(hits) == 0 {
		return []*Memory{}, nil
	}
	out, err := e.topFiltered(ctx, f, rank(hits, df, n, avgdl))
	if err != nil {
		return nil, err
	}
	if len(out) < f.Limit && capped {
		// Filters removed the capped candidates: rank every matching
		// posting exactly (slower, rare).
		all, err := e.db.SearchMemoryTerms(ctx, f, terms)
		if err != nil {
			return nil, err
		}
		return e.topFiltered(ctx, f, rank(all, df, n, avgdl))
	}
	return out, nil
}

// topFiltered returns the best-scoring candidates that pass the filter,
// fetching them in chunks so filtered-out candidates are skipped cheaply.
func (e *Engine) topFiltered(ctx context.Context, f store.MemoryFilter, scores map[string]float64) ([]*Memory, error) {
	ids := make([]string, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if scores[ids[i]] != scores[ids[j]] {
			return scores[ids[i]] > scores[ids[j]]
		}
		return ids[i] > ids[j]
	})
	chunk := max(f.Limit*4, 40)
	out := make([]*Memory, 0, f.Limit)
	for start := 0; start < len(ids) && len(out) < f.Limit; start += chunk {
		part := ids[start:min(start+chunk, len(ids))]
		rows, err := e.db.FilterMemoryIDs(ctx, f, part)
		if err != nil {
			return nil, err
		}
		byID := make(map[string]*store.MemoryRow, len(rows))
		for _, r := range rows {
			byID[r.ID] = r
		}
		for _, id := range part {
			r, ok := byID[id]
			if !ok {
				continue
			}
			m := toMemory(r, true)
			m.Score = math.Round(scores[id]*1000) / 1000
			out = append(out, m)
			if len(out) == f.Limit {
				break
			}
		}
	}
	return out, nil
}

// rank scores postings with Okapi BM25 over the filtered corpus.
func rank(hits []store.TermHit, df map[string]int, n int, avgdl float64) map[string]float64 {
	if avgdl <= 0 {
		avgdl = 1
	}
	scores := map[string]float64{}
	for _, h := range hits {
		d := float64(df[h.Term])
		idf := max(math.Log(1+(float64(n)-d+0.5)/(d+0.5)), 1e-4)
		tf := float64(h.TF)
		norm := tf * (bm25K1 + 1) / (tf + bm25K1*(1-bm25B+bm25B*float64(h.DocLen)/avgdl))
		scores[h.MemoryID] += idf * norm
	}
	return scores
}

// Provenance returns a memory in full, with its edges.
func (e *Engine) Provenance(ctx context.Context, scope Scope, id string) (*Provenance, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	r, err := e.db.GetMemory(ctx, scope.ProjectID, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	if r.ExpiresAt != nil && !r.ExpiresAt.After(e.now()) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	edges, err := e.db.ListMemoryEdges(ctx, scope.ProjectID, id)
	if err != nil {
		return nil, err
	}
	p := &Provenance{Memory: *toMemory(r, false), ContentHash: r.ContentHash, Edges: []Edge{}}
	for _, ed := range edges {
		p.Edges = append(p.Edges, Edge{To: ed.ToRef, Kind: ed.Kind})
	}
	return p, nil
}

// PendingHandoffs lists pending handoffs in scope, for a branch (and
// project-wide ones) when branch is set.
func (e *Engine) PendingHandoffs(ctx context.Context, scope Scope, branch string) ([]*Memory, error) {
	return e.Query(ctx, scope, Query{Kinds: []string{KindHandoff}, Status: StatusPending, Branch: branch, Limit: 100})
}

// AcceptHandoff marks a pending handoff accepted by the caller.
func (e *Engine) AcceptHandoff(ctx context.Context, scope Scope, id string) (*Memory, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	by := scope.Agent
	if scope.SessionID != "" {
		by = strings.TrimPrefix(by+"@session:"+scope.SessionID, "@")
	}
	if by == "" {
		by = orDefault(scope.Origin, OriginAPI)
	}
	r, err := e.db.AcceptHandoff(ctx, scope.ProjectID, id, by, e.now())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	case errors.Is(err, store.ErrConflict):
		return nil, fmt.Errorf("%w: %s is a %s with status %q", ErrConflict, id, r.Kind, r.Status)
	case err != nil:
		return nil, err
	}
	return toMemory(r, false), nil
}

// Expire deletes memories whose TTL has passed and returns how many.
func (e *Engine) Expire(ctx context.Context) (int, error) {
	total := 0
	for {
		batch, err := e.db.ExpiredMemoryIDs(ctx, e.now(), 500)
		if err != nil {
			return total, err
		}
		if len(batch) == 0 {
			return total, nil
		}
		for project, ids := range batch {
			if err := e.db.DeleteMemories(ctx, project, ids); err != nil {
				return total, err
			}
			total += len(ids)
		}
	}
}

// Name implements the daemon component interface.
func (e *Engine) Name() string { return "memory-expiry" }

// Run purges expired memories periodically until ctx ends.
func (e *Engine) Run(ctx context.Context) error {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		if n, err := e.Expire(ctx); err != nil && ctx.Err() == nil {
			e.log.Warn("memory expiry", "err", err)
		} else if n > 0 {
			e.log.Info("expired memories", "count", n)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
