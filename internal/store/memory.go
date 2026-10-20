package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// MemoryRow is one memory (architecture §8). Scope columns are set by the
// memory engine from the caller's identity.
type MemoryRow struct {
	ID          string
	ProjectID   string
	Branch      string
	Worktree    string
	Agent       string
	SessionID   string
	Origin      string
	Kind        string
	Title       string
	Body        string
	Source      string
	SourceLine  int
	Link        string
	ContentHash string
	DocLen      int
	Status      string
	AcceptedBy  string
	AcceptedAt  *time.Time
	Metadata    string // JSON object
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ExpiresAt   *time.Time
}

// MemoryTerm is one inverted-index posting; TF is already weighted by
// field (the memory engine counts title terms double).
type MemoryTerm struct {
	Term string
	TF   int
}

// MemoryEdge links a memory to another memory id or an external ref
// (e.g. "import:fmt", "file:internal/x.go").
type MemoryEdge struct {
	FromID    string
	ToRef     string
	Kind      string
	CreatedAt time.Time
}

// MemoryBundle is a memory with its postings and outgoing edges, written
// atomically.
type MemoryBundle struct {
	Memory *MemoryRow
	Terms  []MemoryTerm
	Edges  []MemoryEdge
}

// MemoryFilter narrows listing and search. ProjectID is mandatory: no
// query ever crosses projects.
type MemoryFilter struct {
	ProjectID string
	Kinds     []string
	// Branch matches memories on that branch and project-wide ones (branch "").
	Branch       string
	Agent        string
	Since        *time.Time
	SourcePrefix string
	Status       string
	Origin       string
	Now          time.Time // expired rows (expires_at <= Now) are excluded
	Limit        int
}

// TermHit is one (memory, term) match with its weighted frequency.
type TermHit struct {
	MemoryID string
	Term     string
	TF       int
	DocLen   int
}

// MemoryKey is the identity of an existing memory used by idempotent ingest.
type MemoryKey struct {
	ID          string
	Kind        string
	Source      string
	ContentHash string
}

// ErrMemoryScope is returned when a filter has no project.
var ErrMemoryScope = errors.New("memory: project scope is required")

const memoryCols = `id, project_id, branch, worktree, agent, session_id, origin, kind, title, body, source, source_line, link, content_hash, doc_len, status, accepted_by, accepted_at, metadata, created_at, updated_at, expires_at`

func scanMemory(sc interface{ Scan(...any) error }) (*MemoryRow, error) {
	var m MemoryRow
	var acceptedAt, expiresAt sql.NullString
	var created, updated string
	err := sc.Scan(&m.ID, &m.ProjectID, &m.Branch, &m.Worktree, &m.Agent, &m.SessionID, &m.Origin, &m.Kind, &m.Title, &m.Body,
		&m.Source, &m.SourceLine, &m.Link, &m.ContentHash, &m.DocLen, &m.Status, &m.AcceptedBy, &acceptedAt, &m.Metadata,
		&created, &updated, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.AcceptedAt, m.ExpiresAt = parseTSPtr(acceptedAt), parseTSPtr(expiresAt)
	m.CreatedAt, m.UpdatedAt = parseTS(created), parseTS(updated)
	return &m, nil
}

// PutMemories writes bundles in one transaction. A bundle whose memory id
// already exists replaces that memory, its postings and its edges.
func (s *Store) PutMemories(ctx context.Context, bundles []MemoryBundle) error {
	if len(bundles) == 0 {
		return nil
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		del := s.rebind(`DELETE FROM memories WHERE id = ?`)
		delTerms := s.rebind(`DELETE FROM memory_terms WHERE memory_id = ?`)
		delEdges := s.rebind(`DELETE FROM memory_edges WHERE from_id = ?`)
		ins := s.rebind(`INSERT INTO memories (` + memoryCols + `) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		insEdge := s.rebind(`INSERT INTO memory_edges (project_id, from_id, to_ref, kind, created_at) VALUES (?,?,?,?,?)
			ON CONFLICT (from_id, to_ref, kind) DO NOTHING`)
		for _, b := range bundles {
			m := b.Memory
			if m.ProjectID == "" {
				return ErrMemoryScope
			}
			if m.ID == "" {
				m.ID = NewID()
			}
			now := Now()
			if m.CreatedAt.IsZero() {
				m.CreatedAt = now
			}
			if m.UpdatedAt.IsZero() {
				m.UpdatedAt = now
			}
			if m.Metadata == "" {
				m.Metadata = "{}"
			}
			// Explicit deletes (not only the cascade) keep Postgres and
			// SQLite behavior identical regardless of FK enforcement.
			for _, q := range []string{delTerms, delEdges, del} {
				if _, err := tx.ExecContext(ctx, q, m.ID); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, ins, m.ID, m.ProjectID, m.Branch, m.Worktree, m.Agent, m.SessionID, m.Origin, m.Kind,
				m.Title, m.Body, m.Source, m.SourceLine, m.Link, m.ContentHash, m.DocLen, m.Status, m.AcceptedBy, tsPtr(m.AcceptedAt),
				m.Metadata, ts(m.CreatedAt), ts(m.UpdatedAt), tsPtr(m.ExpiresAt)); err != nil {
				return err
			}
			for i := 0; i < len(b.Terms); i += termBatch {
				chunk := b.Terms[i:min(i+termBatch, len(b.Terms))]
				args := make([]any, 0, len(chunk)*5)
				for _, t := range chunk {
					args = append(args, t.Term, m.ID, m.ProjectID, t.TF, m.DocLen)
				}
				q := `INSERT INTO memory_terms (term, memory_id, project_id, tf, doc_len) VALUES ` +
					strings.TrimSuffix(strings.Repeat("(?,?,?,?,?),", len(chunk)), ",")
				if _, err := tx.ExecContext(ctx, s.rebind(q), args...); err != nil {
					return err
				}
			}
			for _, e := range b.Edges {
				at := e.CreatedAt
				if at.IsZero() {
					at = now
				}
				if _, err := tx.ExecContext(ctx, insEdge, m.ProjectID, m.ID, e.ToRef, e.Kind, ts(at)); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// termBatch bounds the rows of one multi-row posting insert.
const termBatch = 200

// GetMemory fetches a memory inside a project. A memory of another project
// is reported as not found.
func (s *Store) GetMemory(ctx context.Context, projectID, id string) (*MemoryRow, error) {
	if projectID == "" {
		return nil, ErrMemoryScope
	}
	return scanMemory(s.queryRow(ctx, `SELECT `+memoryCols+` FROM memories WHERE project_id = ? AND id = ?`, projectID, id))
}

// GetMemoriesByIDs fetches memories of one project by id (order unspecified).
func (s *Store) GetMemoriesByIDs(ctx context.Context, projectID string, ids []string) ([]*MemoryRow, error) {
	if projectID == "" {
		return nil, ErrMemoryScope
	}
	return s.FilterMemoryIDs(ctx, MemoryFilter{ProjectID: projectID, Now: time.Unix(0, 0)}, ids)
}

func collectMemories(rows *sql.Rows) ([]*MemoryRow, error) {
	defer func() { _ = rows.Close() }()
	var out []*MemoryRow
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// memoryWhere renders the filter as a WHERE clause over alias m.
func memoryWhere(f MemoryFilter) (string, []any, error) {
	if f.ProjectID == "" {
		return "", nil, ErrMemoryScope
	}
	where := []string{"m.project_id = ?"}
	args := []any{f.ProjectID}
	if len(f.Kinds) > 0 {
		where = append(where, "m.kind IN ("+placeholders(len(f.Kinds))+")")
		for _, k := range f.Kinds {
			args = append(args, k)
		}
	}
	if f.Branch != "" {
		where = append(where, "(m.branch = ? OR m.branch = '')")
		args = append(args, f.Branch)
	}
	if f.Agent != "" {
		where = append(where, "m.agent = ?")
		args = append(args, f.Agent)
	}
	if f.Since != nil {
		where = append(where, "m.created_at >= ?")
		args = append(args, ts(*f.Since))
	}
	if f.SourcePrefix != "" {
		where = append(where, `m.source LIKE ? ESCAPE '\'`)
		args = append(args, likeEscape(f.SourcePrefix)+"%")
	}
	if f.Status != "" {
		where = append(where, "m.status = ?")
		args = append(args, f.Status)
	}
	if f.Origin != "" {
		where = append(where, "m.origin = ?")
		args = append(args, f.Origin)
	}
	now := f.Now
	if now.IsZero() {
		now = Now()
	}
	where = append(where, "(m.expires_at IS NULL OR m.expires_at > ?)")
	args = append(args, ts(now))
	return strings.Join(where, " AND "), args, nil
}

func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func limitOf(f MemoryFilter, def, max int) int {
	if f.Limit <= 0 {
		return def
	}
	if f.Limit > max {
		return max
	}
	return f.Limit
}

// ListMemories returns matching memories, newest first.
func (s *Store) ListMemories(ctx context.Context, f MemoryFilter) ([]*MemoryRow, error) {
	where, args, err := memoryWhere(f)
	if err != nil {
		return nil, err
	}
	args = append(args, limitOf(f, 50, 1000))
	rows, err := s.query(ctx, `SELECT `+prefixCols("m.", memoryCols)+` FROM memories m WHERE `+where+` ORDER BY m.created_at DESC, m.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	return collectMemories(rows)
}

func prefixCols(prefix, cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = prefix + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// TermPostings returns the document frequency of term in a project and
// its cap highest-tf postings, read from the covering index alone.
func (s *Store) TermPostings(ctx context.Context, projectID, term string, limit int) ([]TermHit, int, error) {
	if projectID == "" {
		return nil, 0, ErrMemoryScope
	}
	var df int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM memory_terms WHERE project_id = ? AND term = ?`, projectID, term).Scan(&df); err != nil {
		return nil, 0, err
	}
	if df == 0 {
		return nil, 0, nil
	}
	rows, err := s.query(ctx, `SELECT memory_id, tf, doc_len FROM memory_terms WHERE project_id = ? AND term = ?
		ORDER BY tf DESC LIMIT ?`, projectID, term, limit)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	var out []TermHit
	for rows.Next() {
		h := TermHit{Term: term}
		if err := rows.Scan(&h.MemoryID, &h.TF, &h.DocLen); err != nil {
			return nil, 0, err
		}
		out = append(out, h)
	}
	return out, df, rows.Err()
}

// SearchMemoryTerms returns every posting for terms among memories
// matching f (exact but slower than TermPostings: it joins memories).
func (s *Store) SearchMemoryTerms(ctx context.Context, f MemoryFilter, terms []string) ([]TermHit, error) {
	if len(terms) == 0 {
		return nil, nil
	}
	where, wargs, err := memoryWhere(f)
	if err != nil {
		return nil, err
	}
	args := []any{f.ProjectID}
	for _, t := range terms {
		args = append(args, t)
	}
	args = append(args, wargs...)
	rows, err := s.query(ctx, `SELECT t.memory_id, t.term, t.tf, t.doc_len
		FROM memory_terms t JOIN memories m ON m.id = t.memory_id
		WHERE t.project_id = ? AND t.term IN (`+placeholders(len(terms))+`) AND `+where, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TermHit
	for rows.Next() {
		var h TermHit
		if err := rows.Scan(&h.MemoryID, &h.Term, &h.TF, &h.DocLen); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// FilterMemoryIDs fetches the memories among ids that match f. Rows are
// read by primary key and filtered with Match, which keeps the plan a
// handful of point lookups on both backends.
func (s *Store) FilterMemoryIDs(ctx context.Context, f MemoryFilter, ids []string) ([]*MemoryRow, error) {
	if f.ProjectID == "" {
		return nil, ErrMemoryScope
	}
	rows, err := s.memoriesByID(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	for _, m := range rows {
		if f.Match(m) {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *Store) memoriesByID(ctx context.Context, ids []string) ([]*MemoryRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.query(ctx, `SELECT `+memoryCols+` FROM memories WHERE id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, err
	}
	return collectMemories(rows)
}

// Match reports whether m passes the filter; it mirrors memoryWhere.
func (f MemoryFilter) Match(m *MemoryRow) bool {
	if m.ProjectID != f.ProjectID || f.ProjectID == "" {
		return false
	}
	if len(f.Kinds) > 0 {
		ok := false
		for _, k := range f.Kinds {
			ok = ok || m.Kind == k
		}
		if !ok {
			return false
		}
	}
	if f.Branch != "" && m.Branch != f.Branch && m.Branch != "" {
		return false
	}
	if (f.Agent != "" && m.Agent != f.Agent) || (f.Status != "" && m.Status != f.Status) || (f.Origin != "" && m.Origin != f.Origin) {
		return false
	}
	if f.Since != nil && m.CreatedAt.Before(f.Since.UTC().Truncate(time.Microsecond)) {
		return false
	}
	if f.SourcePrefix != "" && !strings.HasPrefix(m.Source, f.SourcePrefix) {
		return false
	}
	now := f.Now
	if now.IsZero() {
		now = Now()
	}
	return m.ExpiresAt == nil || m.ExpiresAt.After(now)
}

// MemoryCorpusStats returns the number of matching memories and their
// average document length (BM25 normalization).
func (s *Store) MemoryCorpusStats(ctx context.Context, f MemoryFilter) (int, float64, error) {
	where, args, err := memoryWhere(f)
	if err != nil {
		return 0, 0, err
	}
	var n int
	var total sql.NullInt64
	if err := s.queryRow(ctx, `SELECT COUNT(*), SUM(m.doc_len) FROM memories m WHERE `+where, args...).Scan(&n, &total); err != nil {
		return 0, 0, err
	}
	if n == 0 {
		return 0, 0, nil
	}
	return n, float64(total.Int64) / float64(n), nil
}

// ListMemoryKeys returns the id/kind/source/hash of a project's memories
// with the given origin (idempotent ingest and import).
func (s *Store) ListMemoryKeys(ctx context.Context, projectID, origin string) ([]MemoryKey, error) {
	if projectID == "" {
		return nil, ErrMemoryScope
	}
	rows, err := s.query(ctx, `SELECT id, kind, source, content_hash FROM memories WHERE project_id = ? AND origin = ?`, projectID, origin)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []MemoryKey
	for rows.Next() {
		var k MemoryKey
		if err := rows.Scan(&k.ID, &k.Kind, &k.Source, &k.ContentHash); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// DeleteMemories removes memories of a project by id.
func (s *Store) DeleteMemories(ctx context.Context, projectID string, ids []string) error {
	if projectID == "" {
		return ErrMemoryScope
	}
	if len(ids) == 0 {
		return nil
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			for _, q := range []string{
				`DELETE FROM memory_terms WHERE memory_id = ? AND project_id = ?`,
				`DELETE FROM memory_edges WHERE from_id = ? AND project_id = ?`,
				`DELETE FROM memories WHERE id = ? AND project_id = ?`,
			} {
				if _, err := tx.ExecContext(ctx, s.rebind(q), id, projectID); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// AcceptHandoff marks a pending handoff accepted. It returns ErrNotFound
// for an unknown id (or another project's) and ErrConflict when the
// handoff is not pending.
func (s *Store) AcceptHandoff(ctx context.Context, projectID, id, by string, at time.Time) (*MemoryRow, error) {
	if projectID == "" {
		return nil, ErrMemoryScope
	}
	res, err := s.exec(ctx, `UPDATE memories SET status = 'accepted', accepted_by = ?, accepted_at = ?, updated_at = ?
		WHERE project_id = ? AND id = ? AND kind = 'handoff' AND status = 'pending'`, by, ts(at), ts(at), projectID, id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		m, err := s.GetMemory(ctx, projectID, id)
		if err != nil {
			return nil, err
		}
		if m.Kind != "handoff" || m.Status != "pending" {
			return m, ErrConflict
		}
	}
	return s.GetMemory(ctx, projectID, id)
}

// ListMemoryEdges returns the outgoing edges of a memory.
func (s *Store) ListMemoryEdges(ctx context.Context, projectID, fromID string) ([]MemoryEdge, error) {
	rows, err := s.query(ctx, `SELECT from_id, to_ref, kind, created_at FROM memory_edges WHERE project_id = ? AND from_id = ? ORDER BY kind, to_ref`, projectID, fromID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []MemoryEdge
	for rows.Next() {
		var e MemoryEdge
		var at string
		if err := rows.Scan(&e.FromID, &e.ToRef, &e.Kind, &at); err != nil {
			return nil, err
		}
		e.CreatedAt = parseTS(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ExpiredMemoryIDs lists memories whose TTL has passed, per project.
func (s *Store) ExpiredMemoryIDs(ctx context.Context, now time.Time, limit int) (map[string][]string, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.query(ctx, `SELECT project_id, id FROM memories WHERE expires_at IS NOT NULL AND expires_at <= ? ORDER BY expires_at LIMIT ?`, ts(now), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]string{}
	for rows.Next() {
		var p, id string
		if err := rows.Scan(&p, &id); err != nil {
			return nil, err
		}
		out[p] = append(out[p], id)
	}
	return out, rows.Err()
}
