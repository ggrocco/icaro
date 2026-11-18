package assist

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"icaro/internal/assist/stream"
	"icaro/internal/creds"
	"icaro/internal/session"
	"icaro/internal/store"
)

// exitGrace is how long a turn waits for the CLI to exit after its
// stream ended before it signals the process (a variable for tests).
var exitGrace = 15 * time.Second

// runTurn runs one queued turn to a recorded terminal status.
func (s *Service) runTurn(ctx context.Context, a *actor, j job) {
	bg := context.WithoutCancel(ctx)
	m, err := s.o.Store.GetChatMessage(bg, j.messageID)
	if err != nil || m.Status != store.TurnQueued {
		return // cancelled or removed meanwhile
	}
	fail := func(status, msg string) {
		now := s.o.Now()
		m.Status, m.Error, m.FinishedAt = status, msg, &now
		if ok, _ := s.o.Store.UpdateChatMessageIf(bg, m, store.TurnQueued, store.TurnRunning); ok {
			a.append(entry{Type: EntryStatus, MessageID: m.ID, Status: status, Note: msg})
		}
	}
	c, err := s.o.Store.GetChat(bg, a.chatID)
	if err != nil {
		fail(store.TurnError, "chat: "+err.Error())
		return
	}
	agent, err := s.agentFor(c.Profile)
	if err != nil || !agent.CanChat() {
		fail(store.TurnError, fmt.Sprintf("profile %q cannot run the assistant", c.Profile))
		return
	}
	sid, err := s.ensureSession(ctx, a, c, m.ID)
	if err != nil {
		if ctx.Err() != nil {
			fail(store.TurnCancelled, "cancelled")
			return
		}
		fail(store.TurnError, err.Error())
		return
	}
	m.Status = store.TurnRunning
	if ok, err := s.o.Store.UpdateChatMessageIf(bg, m, store.TurnQueued); err != nil || !ok {
		return
	}
	a.append(entry{Type: EntryStatus, MessageID: m.ID, Status: store.TurnRunning})

	turn := creds.ChatTurn{ConversationID: c.ConversationID}
	text := j.text
	if turn.ConversationID == "" {
		turn.NewID = newUUID()
		// A new conversation starts with notes from earlier chats.
		if notes, refs := s.recall(ctx, c, j.text); notes != "" {
			text = notes + "\n\n---\n\n" + j.text
			a.append(entry{Type: EntryMemory, MessageID: m.ID, Memories: refs})
		}
	}
	cmd, _ := agent.ChatCommand(turn)
	r := s.execTurn(ctx, a, m.ID, sid, agent, cmd, text)
	if r.conversation != "" && r.conversation != c.ConversationID {
		if err := s.o.Store.SetChatField(bg, c.ID, store.ChatConversationID, r.conversation); err != nil {
			s.o.Log.Warn("assistant: record conversation", "chat", c.ID, "err", err)
		}
	}

	now := s.o.Now()
	m.Text, m.FinishedAt = r.text, &now
	if u := r.usage; u != nil {
		m.InputTokens, m.OutputTokens, m.CacheRead, m.CacheWrite = u.Input, u.Output, u.CacheRead, u.CacheWrite
		m.CostMicro = int64(u.CostUSD * 1e6)
	}
	m.Status, m.Error = r.status, r.err
	m.ResetAt = r.resetAt
	if ok, err := s.o.Store.UpdateChatMessageIf(bg, m, store.TurnRunning); err != nil || !ok {
		s.o.Log.Warn("assistant: record turn", "chat", c.ID, "message", m.ID, "err", err)
		return
	}
	a.append(entry{Type: EntryStatus, MessageID: m.ID, Status: m.Status, Note: m.Error, Message: ptr(s.message(c, m))})
}

// turnResult is what a turn's stream and process came to.
type turnResult struct {
	status, err  string
	text         string
	conversation string
	usage        *stream.Usage
	resetAt      *time.Time
}

// execTurn runs the CLI and turns its stream into log entries and a
// result. Cancelling ctx (the person's Cancel) stops the process.
func (s *Service) execTurn(ctx context.Context, a *actor, msgID, sid string, agent creds.Agent, cmd []string, text string) turnResult {
	tctx, cancel := context.WithTimeout(ctx, s.o.Config.Assistant.TurnTimeout)
	defer cancel()
	pr, pw := io.Pipe()
	type execResult struct {
		res session.TurnResult
		err error
	}
	done := make(chan execResult, 1)
	go func() {
		res, err := s.o.Sessions.RunTurn(tctx, sid, session.TurnRequest{Cmd: cmd, Env: agent.ChatEnv(), Stdin: []byte(text)}, pw)
		_ = pw.CloseWithError(err)
		done <- execResult{res, err}
	}()

	var out turnResult
	var b strings.Builder
	rx := s.redactor(ctx)
	p, _ := stream.New(agent.Name)
	sum, _ := stream.Read(pr, p, func(ev stream.Event) {
		ev.Text, ev.Result = rx.RedactString(ev.Text), rx.RedactString(ev.Result)
		if len(ev.Args) > 0 {
			ev.Args = rx.Redact(ev.Args)
		}
		switch ev.Kind {
		case stream.KindText:
			if b.Len() < store.MaxChatText {
				b.WriteString(ev.Text)
			}
		case stream.KindSession:
			out.conversation = ev.ConversationID
		case stream.KindUsage:
			out.usage = ev.Usage
		}
		a.append(entry{Type: EntryEvent, MessageID: msgID, Event: &ev})
	})
	go func() { _, _ = io.Copy(io.Discard, pr) }() // let the CLI finish writing

	var res execResult
	select {
	case res = <-done:
	case <-time.After(exitGrace):
		cancel()
		res = <-done
	}
	if ctx.Err() != nil || tctx.Err() != nil || res.err != nil {
		// The exec stream is gone but the process may live on: signal it.
		if err := s.o.Sessions.CancelTurn(context.WithoutCancel(ctx), sid); err != nil {
			s.o.Log.Warn("assistant: stop turn process", "session", sid, "err", err)
		}
	}
	out.text = b.String()
	switch {
	case ctx.Err() != nil:
		out.status, out.err = store.TurnCancelled, "cancelled"
	case sum.Terminal != nil:
		t := sum.Terminal
		switch t.Kind {
		case stream.KindDone:
			out.status = store.TurnDone
		case stream.KindRateLimited:
			out.status, out.err, out.resetAt = store.TurnRateLimited, t.Text, t.ResetAt
		case stream.KindAuthRequired:
			out.status, out.err = store.TurnAuthRequired, t.Text
		default:
			out.status, out.err = store.TurnError, t.Text
		}
	case tctx.Err() != nil:
		out.status, out.err = store.TurnError, fmt.Sprintf("the turn did not finish within %s", s.o.Config.Assistant.TurnTimeout)
	case res.err != nil:
		out.status, out.err = store.TurnError, "run the agent: "+res.err.Error()
	default:
		msg := fmt.Sprintf("the agent exited (code %d) without finishing the turn", res.res.ExitCode)
		if res.res.Stderr != "" {
			msg += ": " + lastLines(rx.RedactString(res.res.Stderr), 5)
		}
		out.status, out.err = store.TurnError, msg
	}
	return out
}

// ensureSession returns the chat's running assist session, creating or
// starting it, and waiting while it is queued behind assistant.max_active.
func (s *Service) ensureSession(ctx context.Context, a *actor, c *store.Chat, msgID string) (string, error) {
	sid := c.SessionID
	var sess *session.Session
	if sid != "" {
		got, err := s.o.Sessions.Get(ctx, sid)
		switch {
		case errors.Is(err, session.ErrNotFound):
			sid = ""
		case err != nil:
			return "", err
		default:
			sess = got
		}
	}
	if sid == "" {
		created, err := s.o.Sessions.CreateAssist(ctx, session.AssistRequest{ChatID: c.ID, Profile: c.Profile, ProjectID: c.ProjectID})
		if err != nil {
			return "", fmt.Errorf("start the assistant: %w", err)
		}
		sid, sess = created.ID, created
		if err := s.o.Store.SetChatField(context.WithoutCancel(ctx), c.ID, store.ChatSessionID, sid); err != nil {
			return "", err
		}
	}
	switch sess.Status {
	case session.StatusRunning, session.StatusQueued, session.StatusCreating:
	default:
		started, err := s.o.Sessions.Start(ctx, sid)
		if err != nil {
			return "", fmt.Errorf("start the assistant: %w", err)
		}
		sess = started
	}
	noted := false
	for sess.Status != session.StatusRunning {
		switch sess.Status {
		case session.StatusQueued, session.StatusCreating:
			if !noted && sess.Status == session.StatusQueued {
				noted = true
				a.append(entry{Type: EntryStatus, MessageID: msgID, Status: store.TurnQueued, Note: fmt.Sprintf("Waiting for a free assistant slot (assistant.max_active is %d).", s.o.Config.Assistant.MaxActive)})
			}
		default:
			reason := sess.Error
			if reason == "" {
				reason = "it is " + sess.Status
			}
			return "", fmt.Errorf("the assistant's container did not start: %s", reason)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(s.o.Poll):
		}
		got, err := s.o.Sessions.Get(ctx, sid)
		if err != nil {
			return "", err
		}
		sess = got
	}
	return sid, nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}

// newUUID returns a random (version 4) UUID, the id Claude Code accepts
// for a new conversation.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
