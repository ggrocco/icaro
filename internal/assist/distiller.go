package assist

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"icaro/internal/assist/distill"
	"icaro/internal/memory"
	"icaro/internal/store"
)

// Memories is the part of the memory engine the assistant uses.
type Memories interface {
	Put(ctx context.Context, scope memory.Scope, origin string, items []memory.ImportItem) (*memory.ImportReport, error)
	ByOrigin(ctx context.Context, scope memory.Scope, origin string, limit int) ([]*memory.Memory, error)
	Query(ctx context.Context, scope memory.Scope, q memory.Query) ([]*memory.Memory, error)
	Remove(ctx context.Context, scope memory.Scope, ids ...string) error
}

// Model opens the stage 2 model for a batch of jobs. When stage 2 cannot
// run (disabled, no model pulled) it returns a nil completer and why.
type Model interface {
	Open(ctx context.Context) (c distill.Completer, closeFn func(), skip string, err error)
}

// Distiller turns idle chats into memories (ADR 0036): stage 1 always,
// from the chat's log and the store; stage 2 with a local model when one
// is available. It is a component with one goroutine; jobs are claimed
// from the store one at a time.
type Distiller struct {
	s     *Service
	runs  RunsReader
	model Model
	wake  chan struct{}
	log   *slog.Logger
	// Poll is how often the queue is checked without a wake (default
	// 30s); Retry is when a failed job is tried again (default 5m).
	Poll, Retry time.Duration
}

// NewDistiller builds the distiller and has s queue chats on it when
// they go idle. model may be nil (stage 1 only).
func NewDistiller(s *Service, runs RunsReader, model Model) *Distiller {
	d := &Distiller{s: s, runs: runs, model: model, wake: make(chan struct{}, 1), log: s.o.Log, Poll: 30 * time.Second, Retry: 5 * time.Minute}
	s.distiller = d
	return d
}

// Name implements app.Component.
func (d *Distiller) Name() string { return "assistant-memory" }

// Enqueue queues a chat now.
func (d *Distiller) Enqueue(ctx context.Context, chatID string) error {
	if _, err := d.s.o.Store.EnqueueDistill(ctx, chatID, d.s.o.Now()); err != nil {
		return err
	}
	select {
	case d.wake <- struct{}{}:
	default:
	}
	return nil
}

// batch holds the model open across consecutive jobs.
type batch struct {
	c     distill.Completer
	close func()
	skip  string
	open  bool
}

func (b *batch) done() {
	if b.close != nil {
		b.close()
	}
	*b = batch{}
}

// Run processes the queue until ctx ends.
func (d *Distiller) Run(ctx context.Context) error {
	if n, err := d.s.o.Store.ResetDistills(ctx); err != nil {
		d.log.Error("assistant memory: reset jobs", "err", err)
	} else if n > 0 {
		d.log.Info("assistant memory: jobs a restart interrupted are queued again", "jobs", n)
	}
	t := time.NewTicker(d.Poll)
	defer t.Stop()
	var b batch
	defer b.done()
	for {
		job, err := d.s.o.Store.ClaimDistill(ctx, d.s.o.Now())
		switch {
		case errors.Is(err, store.ErrNotFound):
			b.done() // the queue is empty: stop the model server
			select {
			case <-ctx.Done():
				return nil
			case <-d.wake:
			case <-t.C:
			}
			continue
		case err != nil:
			if ctx.Err() != nil {
				return nil
			}
			d.log.Error("assistant memory: claim", "err", err)
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
			}
			continue
		}
		d.process(ctx, job, &b)
	}
}

// process runs one job to a recorded outcome.
func (d *Distiller) process(ctx context.Context, job *store.DistillJob, b *batch) {
	jctx, cancel := context.WithTimeout(ctx, d.s.o.Config.Memory.Distill.Timeout)
	defer cancel()
	n, stage2, err := d.distill(jctx, job.ChatID, b)
	job.Memories, job.Stage2 = n, stage2
	switch {
	case err == nil:
		job.Status, job.Error = store.DistillDone, ""
	case job.Attempts < 2 && !errors.Is(err, ErrNotFound):
		job.Status, job.Error, job.RunAt = store.DistillPending, err.Error(), d.s.o.Now().Add(d.Retry)
	default:
		job.Status, job.Error = store.DistillFailed, err.Error()
	}
	if err != nil {
		d.log.Warn("assistant memory: distill", "chat", job.ChatID, "attempt", job.Attempts, "err", err)
	}
	if ferr := d.s.o.Store.FinishDistill(context.WithoutCancel(ctx), job); ferr != nil {
		d.log.Error("assistant memory: record job", "chat", job.ChatID, "err", ferr)
	}
}

// distill writes a chat's memories and stats; it returns how many
// memories it wrote and what stage 2 did. A stage 2 failure fails the job
// (and so retries it once); stage 1's memories are kept either way.
func (d *Distiller) distill(ctx context.Context, chatID string, b *batch) (int, string, error) {
	s := d.s
	c, err := s.chat(ctx, chatID)
	if err != nil {
		return 0, "", err
	}
	raw, _, err := s.Entries(ctx, chatID, 0)
	if err != nil {
		return 0, "", err
	}
	entries := decodeEntries(raw)
	f := readFacts(c, entries)
	if err := s.o.Store.PutChatStats(ctx, f.stats(chatID)); err != nil {
		return 0, "", err
	}
	if s.o.Memories == nil {
		return 0, "skipped: no memory engine", nil
	}
	items := f.lessons(c.Workflow)
	if dr, err := s.o.Store.GetChatDraft(ctx, chatID); err == nil && strings.TrimSpace(dr.YAML) != "" && strings.TrimSpace(dr.Issues) == "[]" {
		items = append(items, integrationFacts(nameOr(c.Workflow, "the draft"), dr.YAML)...)
	}
	if aps, err := s.o.Store.ListChatApprovals(ctx, chatID, false); err == nil && d.runs != nil {
		items = append(items, outcomes(ctx, d.runs, c, aps, s.redactor(ctx))...)
	} else if d.runs != nil && err != nil {
		return 0, "", err
	}
	stage2 := "skipped: no model"
	if d.model != nil {
		sum, why, err := d.summarize(ctx, c, b)
		switch {
		case err != nil:
			stage2 = "failed: " + err.Error()
		case why != "":
			stage2 = "skipped: " + why
		default:
			stage2 = "done"
			items = append(items, summaryItems(c, sum, s.o.Config.Memory.Distill.TTL)...)
		}
		if err != nil {
			n, perr := d.put(ctx, c, items)
			if perr != nil {
				return n, stage2, perr
			}
			return n, stage2, fmt.Errorf("model stage: %w", err)
		}
	}
	n, err := d.put(ctx, c, items)
	return n, stage2, err
}

// put writes items, leaving out the ones a person removed.
func (d *Distiller) put(ctx context.Context, c *store.Chat, items []memory.ImportItem) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	scope := MemoryScope(c)
	removed, err := d.s.o.Store.DistillRemoved(ctx, c.ID)
	if err != nil {
		return 0, err
	}
	keep := items[:0]
	for _, it := range items {
		if !removed[memory.StableID(scope.ProjectID, Origin(c.ID), it.Key)] {
			if it.Metadata == nil {
				it.Metadata = map[string]any{}
			}
			it.Metadata["chat"] = c.ID
			keep = append(keep, it)
		}
	}
	rep, err := d.s.o.Memories.Put(ctx, scope, Origin(c.ID), keep)
	if err != nil {
		return 0, err
	}
	return rep.Imported + rep.Unchanged, nil
}

// summarize runs stage 2 on the chat's messages (the person's and the
// assistant's words only; never run logs).
func (d *Distiller) summarize(ctx context.Context, c *store.Chat, b *batch) (distill.Summary, string, error) {
	if !b.open {
		comp, closeFn, skip, err := d.model.Open(ctx)
		if err != nil {
			return distill.Summary{}, "", err
		}
		*b = batch{c: comp, close: closeFn, skip: skip, open: true}
	}
	if b.c == nil {
		return distill.Summary{}, b.skip, nil
	}
	msgs, err := d.s.o.Store.ListChatMessages(ctx, c.ID)
	if err != nil {
		return distill.Summary{}, "", err
	}
	var turns []distill.Turn
	for _, m := range msgs {
		if strings.TrimSpace(m.Text) == "" || (m.Role == store.RoleAssistant && m.Status != store.TurnDone) {
			continue
		}
		turns = append(turns, distill.Turn{Role: m.Role, Text: m.Text})
	}
	if len(turns) == 0 {
		return distill.Summary{}, "nothing to summarize", nil
	}
	sum, _, err := distill.Summarize(ctx, b.c, c.Workflow, turns)
	if err != nil {
		b.done() // the server may be broken: open it again next time
	}
	return sum, "", err
}

// summaryItems are stage 2's memories: they expire after ttl.
func summaryItems(c *store.Chat, s distill.Summary, ttl time.Duration) []memory.ImportItem {
	wf := nameOr(c.Workflow, nameOr(c.Title, "this chat's workflow"))
	meta := func(typ string) map[string]any {
		return map[string]any{"type": typ, "workflow": c.Workflow, "confidence": ConfidenceModel}
	}
	item := func(key, kind, title, body, typ string) memory.ImportItem {
		return memory.ImportItem{Key: key, Kind: kind, Title: clip(title, 200), Body: body, Source: SourceSummary, Metadata: meta(typ), TTL: ttl}
	}
	var out []memory.ImportItem
	if s.Intent != "" {
		out = append(out, item("intent", memory.KindFinding, "Intent of "+wf+": "+s.Intent, s.Intent, "intent"))
	}
	if len(s.Constraints) > 0 {
		out = append(out, item("constraints", memory.KindFinding, "Constraints for "+wf, bullets(s.Constraints), "constraints"))
	}
	if len(s.Preferences) > 0 {
		out = append(out, item("preferences", memory.KindFinding, "Preferences for "+wf, bullets(s.Preferences), "preferences"))
	}
	for _, r := range s.Rejected {
		body := r.What
		if r.Why != "" {
			body += "\n\nWhy: " + r.Why
		}
		out = append(out, item("rejected:"+hash(r.What), memory.KindDecision, "Rejected for "+wf+": "+r.What, body, "rejected"))
	}
	for _, o := range s.Open {
		out = append(out, item("open:"+hash(o), memory.KindTask, "Open question about "+wf+": "+o, o, "open"))
	}
	if s.Resume != "" {
		// A finding, not a handoff: a handoff is accepted by agent
		// sessions of the project, which this note is not for.
		out = append(out, item("resume", memory.KindFinding, "Where the chat about "+wf+" left off", s.Resume, "resume"))
	}
	return out
}

func bullets(xs []string) string {
	var b strings.Builder
	for _, x := range xs {
		b.WriteString("- " + x + "\n")
	}
	return strings.TrimSpace(b.String())
}

func nameOr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
