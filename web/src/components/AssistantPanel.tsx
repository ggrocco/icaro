import { useEffect, useMemo, useRef, useState } from "react";
import { assistApi, applyEntry, blocker, busy, emptyView, shownText, subscribeChat, type ChatDetail, type ChatView, type ToolCall } from "../assist";
import type { Issue } from "../api";
import { diffLines } from "../diff";
import { href } from "../router";
import { ErrorBox, Issues, Loading, When } from "./common";
import { Icon } from "./icons";

interface Props {
  chatId: string;
  // yaml is the editor's current document; a proposal is shown as a diff
  // against it.
  yaml: string;
  // onAccept replaces the editor's document with the assistant's proposal.
  onAccept: (yaml: string) => void;
  onClose?: () => void;
  // onSettled runs when a turn ends (the chat's title or workflow may
  // have changed).
  onSettled?: () => void;
}

interface Proposal {
  version: number;
  yaml: string;
  issues: Issue[];
}

// AssistantPanel is the chat docked beside the editor (ADR 0035). It
// rebuilds its view from the chat's log, shows the assistant's proposals as
// diffs to accept or discard, and asks the person before any run.
export function AssistantPanel({ chatId, yaml, onAccept, onClose, onSettled }: Props) {
  const [detail, setDetail] = useState<ChatDetail | undefined>();
  const [error, setError] = useState<Error | undefined>();
  const [view, setView] = useState<ChatView>(() => emptyView());
  const [live, setLive] = useState(false);
  const [text, setText] = useState("");
  const [sending, setSending] = useState(false);
  const [actionError, setActionError] = useState<Error | undefined>();
  const [proposal, setProposal] = useState<Proposal | undefined>();
  // The newest draft version the person has accepted or discarded.
  const handled = useRef(0);
  const listRef = useRef<HTMLOListElement>(null);

  useEffect(() => {
    let closed = false;
    setView(emptyView());
    setDetail(undefined);
    assistApi
      .get(chatId)
      .then((d) => {
        if (closed) return;
        setDetail(d);
        // The editor opened on the current draft: nothing to propose yet.
        handled.current = d.draft?.version ?? 0;
        setView((v) => ({ ...emptyView(d), live: v.live, tools: v.tools }));
      })
      .catch(setError);
    const stop = subscribeChat(chatId, (e) => !closed && setView((v) => applyEntry(v, e)), setLive);
    return () => {
      closed = true;
      stop();
    };
  }, [chatId]);

  // A new draft written by the assistant becomes a proposal, unless it is
  // what the editor already holds.
  useEffect(() => {
    if (view.draftSource !== "assistant" || view.draftVersion <= handled.current) return;
    let closed = false;
    assistApi
      .get(chatId)
      .then((d) => {
        if (closed || !d.draft || d.draft.source !== "assistant" || d.draft.version <= handled.current) return;
        if (d.draft.yaml === yaml) {
          handled.current = d.draft.version;
          return;
        }
        setProposal({ version: d.draft.version, yaml: d.draft.yaml, issues: d.draft.issues ?? [] });
      })
      .catch(setActionError);
    return () => {
      closed = true;
    };
  }, [chatId, view.draftVersion, view.draftSource, yaml]);

  useEffect(() => {
    listRef.current?.scrollTo({ top: listRef.current.scrollHeight });
  }, [view.messages, view.live, view.approvals]);

  // When a turn settles, refresh the chat's title and container status.
  const settledCount = view.messages.filter((m) => m.role === "assistant" && m.status !== "queued" && m.status !== "running").length;
  useEffect(() => {
    if (settledCount === 0) return;
    assistApi
      .get(chatId)
      .then((d) => setDetail(d))
      .catch(() => undefined);
    onSettled?.();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [chatId, settledCount]);

  const act = async (fn: () => Promise<unknown>) => {
    setActionError(undefined);
    try {
      await fn();
    } catch (e) {
      setActionError(e as Error);
    }
  };
  const send = async (e?: React.FormEvent) => {
    e?.preventDefault();
    const t = text.trim();
    if (!t || sending) return;
    setSending(true);
    await act(async () => {
      await assistApi.send(chatId, t);
      setText("");
    });
    setSending(false);
  };
  const accept = () => {
    if (!proposal) return;
    handled.current = proposal.version;
    onAccept(proposal.yaml);
    setProposal(undefined);
  };
  const discard = () => {
    if (!proposal) return;
    handled.current = proposal.version;
    setProposal(undefined);
    // The assistant's next get_draft returns the person's document.
    void act(() => assistApi.putDraft(chatId, yaml));
  };

  const working = busy(view);
  const stuck = blocker(view);
  const diff = useMemo(() => (proposal ? diffLines(yaml, proposal.yaml) : []), [proposal, yaml]);

  return (
    <aside className="assistant-panel" aria-label="Workflow assistant">
      <header className="assistant-head">
        <Icon name="assistant" />
        <div className="assistant-title">
          <strong>{detail?.title || "Assistant"}</strong>
          <span className="muted">
            {detail ? `${detail.agent} · profile ${detail.profile}` : "…"}
            {detail?.session_status ? ` · container ${detail.session_status}` : ""}
            {!live && detail ? " · reconnecting" : ""}
          </span>
        </div>
        <a className="ghost icon-btn" href={href("assistant", chatId)} title="Open the chat on its own page" aria-label="Open the chat on its own page">
          <Icon name="external" />
        </a>
        {onClose && (
          <button className="ghost icon-btn" onClick={onClose} aria-label="Close the assistant" title="Close">
            <Icon name="close" />
          </button>
        )}
      </header>
      {error && <ErrorBox error={error} />}
      {!detail && !error && <Loading what="chat" />}
      {detail && (
        <>
          <ol className="chat-log" ref={listRef} aria-live="polite" aria-label="Conversation">
            {view.messages.length === 0 && (
              <li className="chat-empty muted">
                Describe the workflow you want, or ask about the one in the editor. The assistant proposes changes here as a diff; you apply them and save.
              </li>
            )}
            {view.messages.map((m) => (
              <li key={m.id} className={`chat-msg ${m.role}`}>
                <span className="chat-who">{m.role === "user" ? "You" : "Assistant"}</span>
                {(view.tools[m.id] ?? []).length > 0 && <ToolCalls calls={view.tools[m.id]} />}
                <div className="chat-text">{shownText(view, m) || (m.status === "running" ? "…" : "")}</div>
                {view.notes[m.id] && <p className="muted chat-note">{view.notes[m.id]}</p>}
                {m.role === "assistant" && (m.status === "queued" || m.status === "running") && (
                  <span className={`badge ${m.status}`}>{m.status === "queued" ? "waiting" : "working"}</span>
                )}
                {m.role === "assistant" && m.status === "done" && m.usage && (
                  <span className="muted chat-usage">
                    {m.usage.input + (m.usage.cache_read ?? 0)} in · {m.usage.output} out tokens
                    {m.cost_usd !== undefined ? ` · ~$${m.cost_usd.toFixed(4)} (estimate)` : ""}
                  </span>
                )}
              </li>
            ))}
            {view.chatNotes.map((n, i) => (
              <li key={`note-${i}`} className="chat-note muted">
                {n}
              </li>
            ))}
          </ol>

          {view.approvals.map((a) => (
            <div key={a.id} className="notice warn approval-card" role="group" aria-label="Run approval">
              <strong>The assistant wants to run {a.request.workflow ?? "a workflow"}</strong>
              {a.request.version !== undefined && <span className="muted"> v{a.request.version}</span>}
              {a.request.trigger && Object.keys(a.request.trigger).length > 0 && (
                <pre className="code small">{JSON.stringify(a.request.trigger, null, 2)}</pre>
              )}
              <p className="muted">
                Expires <When ts={a.expires_at} />.
              </p>
              <div className="row">
                <button className="primary" onClick={() => act(() => assistApi.decide(chatId, a.id, true))}>
                  <Icon name="play" />
                  Approve run
                </button>
                <button onClick={() => act(() => assistApi.decide(chatId, a.id, false))}>Decline</button>
              </div>
            </div>
          ))}

          {proposal && (
            <div className="proposal" role="group" aria-label="Proposed change">
              <div className="row">
                <strong>Proposed change (draft v{proposal.version})</strong>
                <span className={proposal.issues.length ? "badge failed" : "badge succeeded"}>
                  {proposal.issues.length ? `${proposal.issues.length} issue(s)` : "valid"}
                </span>
              </div>
              <pre className="code diff" aria-label="Diff from the editor to the proposal">
                {diff.map((l, i) => (
                  <div key={i} className={l.kind}>
                    {l.kind === "add" ? "+ " : l.kind === "del" ? "- " : "  "}
                    {l.text}
                  </div>
                ))}
              </pre>
              <Issues issues={proposal.issues} />
              <div className="row">
                <button className="primary" onClick={accept}>
                  <Icon name="check" />
                  Accept into the editor
                </button>
                <button onClick={discard}>Discard</button>
              </div>
              <p className="muted">Accepting only changes the editor. Save with Validate &amp; save.</p>
            </div>
          )}

          {stuck && <Blocker chatId={chatId} profile={detail.profile} status={stuck.status} error={stuck.error} resetAt={stuck.reset_at} messageId={stuck.id} onError={setActionError} />}
          {actionError && <ErrorBox error={actionError} />}

          <form className="composer" onSubmit={send}>
            <label className="sr-only" htmlFor={`chat-${chatId}`}>
              Message the assistant
            </label>
            <textarea
              id={`chat-${chatId}`}
              value={text}
              rows={3}
              placeholder="Ask for a workflow, a change, or why a run failed…"
              onChange={(e) => setText(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) void send();
              }}
            />
            <div className="row">
              {working && (
                <button type="button" onClick={() => act(() => assistApi.cancel(chatId))}>
                  <Icon name="stop" />
                  Cancel
                </button>
              )}
              <button className="primary" type="submit" disabled={!text.trim() || sending}>
                <Icon name="send" />
                {sending ? "Sending…" : "Send"}
              </button>
            </div>
          </form>
        </>
      )}
    </aside>
  );
}

function ToolCalls({ calls }: { calls: ToolCall[] }) {
  return (
    <ul className="tool-calls" aria-label="Tools the assistant used">
      {calls.map((c, i) => (
        <li key={`${c.id}-${i}`}>
          <details>
            <summary>
              <code>{c.tool}</code>
              {!c.done && <span className="muted"> running…</span>}
              {c.isError && <span className="badge error">error</span>}
            </summary>
            {c.args !== undefined && <pre className="code small">{JSON.stringify(c.args, null, 2)}</pre>}
            {c.result && <pre className="code small">{c.result}</pre>}
          </details>
        </li>
      ))}
    </ul>
  );
}

function Blocker({
  chatId,
  profile,
  status,
  error,
  resetAt,
  messageId,
  onError,
}: {
  chatId: string;
  profile: string;
  status: string;
  error?: string;
  resetAt?: string;
  messageId: string;
  onError: (e: Error) => void;
}) {
  const retry = () => assistApi.retry(chatId, messageId).catch(onError);
  if (status === "auth_required")
    return (
      <div className="notice warn" role="status">
        <strong>Sign in required.</strong> The assistant runs your <code>{profile}</code> profile's CLI under its own login. Run{" "}
        <code>icaro login {profile}</code> in a terminal, then retry.
        <button className="ghost" onClick={retry}>
          Retry
        </button>
      </div>
    );
  if (status === "rate_limited")
    return (
      <div className="notice warn" role="status">
        <strong>Usage limit reached.</strong> {resetAt ? <>It resets <When ts={resetAt} />.</> : "Try again later."} Nothing retries on its own.
        <button className="ghost" onClick={retry}>
          Retry
        </button>
      </div>
    );
  const what = status === "interrupted" ? "The reply was interrupted" : status === "cancelled" ? "You cancelled the reply" : "The reply failed";
  return (
    <div className={status === "cancelled" ? "notice" : "notice error"} role="status">
      <strong>{what}.</strong> {error}
      <button className="ghost" onClick={retry}>
        Retry
      </button>
    </div>
  );
}
