import { useEffect, useMemo, useRef, useState } from "react";
import { api, subscribeRun, terminal, type RunInfo, type StepInfo } from "../api";
import { RunGraph } from "../components/lazy";
import { ErrorBox, Loading, PageHeader, Status, When } from "../components/common";
import { Icon } from "../components/icons";
import { go, href } from "../router";
import { parse } from "../yamldoc";

// RunDetail renders the run against its pinned workflow version and
// follows it over SSE: status and logs update without polling.
export function RunDetail({ id }: { id: string }) {
  const [run, setRun] = useState<RunInfo | undefined>();
  const [error, setError] = useState<Error | undefined>();
  const [yaml, setYaml] = useState<string | undefined>();
  const [selected, setSelected] = useState<string | undefined>();
  const [logs, setLogs] = useState<Record<string, string>>({});
  const [live, setLive] = useState(false);
  const logRef = useRef<HTMLPreElement>(null);
  // Once the stream reports the final state, late responses (e.g. to a
  // Cancel click) must not overwrite it with an earlier status.
  const finalRef = useRef(false);
  const apply = (r: RunInfo, final = false) => {
    if (finalRef.current && !final) return;
    if (final || terminal(r.status)) finalRef.current = true;
    setRun(r);
  };

  useEffect(() => {
    let closed = false;
    finalRef.current = false;
    api
      .getRun(id)
      .then((r) => {
        if (closed) return;
        apply(r);
        return api.version(r.workflow, r.version).then((v) => !closed && setYaml(v.yaml));
      })
      .catch(setError);
    const stop = subscribeRun(
      id,
      (e) => {
        if (e.type === "status") {
          apply(e.run, e.final);
          setLive(!e.final);
          if (e.final) return;
        } else if (e.type === "step") {
          if (finalRef.current) return;
          setRun((r) => r && { ...r, steps: r.steps?.map((s) => (s.id === e.step ? { ...s, status: e.status, attempt: e.attempt || s.attempt } : s)) });
          // A step event carries no approval; load it to show the request.
          if (e.status === "waiting") api.getRun(id).then((r) => apply(r)).catch(setError);
        } else {
          setLogs((l) => ({ ...l, [e.step]: (l[e.step] ?? "") + e.line + "\n" }));
        }
        setLive(true);
      },
      () => setLive(false),
    );
    return () => {
      closed = true;
      stop();
    };
  }, [id]);

  useEffect(() => {
    logRef.current?.scrollTo({ top: logRef.current.scrollHeight });
  }, [logs, selected]);

  const spec = useMemo(() => {
    if (!yaml) return undefined;
    const d = parse(yaml);
    return d.errors.length ? undefined : d.toJS();
  }, [yaml]);

  if (error) return <ErrorBox error={error} />;
  if (!run) return <Loading what="run" />;
  const step = run.steps?.find((s) => s.id === selected);

  return (
    <section aria-labelledby="run-title">
      <PageHeader
        id="run-title"
        title={
          <>
            Run <code>{run.id.slice(-10)}</code> <Status status={run.status} />
          </>
        }
      >
        {!terminal(run.status) && (
          <button className="danger" onClick={() => api.cancel(run.id).then((r) => apply(r)).catch(setError)}>
            <Icon name="stop" />
            Cancel
          </button>
        )}
        {terminal(run.status) && (
          <button onClick={() => api.rerun(run).then((r) => go("runs", r.id)).catch(setError)}>
            <Icon name="rerun" />
            Re-run
          </button>
        )}
      </PageHeader>
      <div className="meta">
        <span>
          <Icon name="workflows" />
          <a href={href("workflows", run.workflow)}>{run.workflow}</a> <span className="version">v{run.version}</span>
        </span>
        <span>
          Trigger <strong>{run.trigger_kind}</strong>
        </span>
        {run.rerun_of && (
          <span>
            Re-run of <a href={href("runs", run.rerun_of)}>{run.rerun_of.slice(-10)}</a>
          </span>
        )}
        <span>
          Started <strong>{run.started_at ? <When ts={run.started_at} /> : "not yet"}</strong>
        </span>
        {run.finished_at && (
          <span>
            Finished <strong><When ts={run.finished_at} /></strong>
          </span>
        )}
        <span aria-live="polite" className={live ? "live" : undefined}>
          {live ? "Live" : ""}
        </span>
      </div>
      {run.error && <div className="notice error">{run.error}</div>}
      {(run.steps ?? [])
        .filter((s) => s.status === "waiting" && s.approval && !s.approval.decision)
        .map((s) => (
          <ApprovalRequest key={s.id} runId={run.id} step={s} onDecided={(r) => apply(r)} />
        ))}
      {(run.steps ?? [])
        .filter((s) => s.status === "waiting" && s.await?.state === "waiting")
        .map((s) => (
          <AwaitRequest key={s.id} runId={run.id} step={s} onDelivered={(r) => apply(r)} />
        ))}
      {spec ? <RunGraph spec={spec} steps={run.steps} selected={selected} onSelect={setSelected} /> : <Loading what="graph" />}
      <div className="split">
        <div className="pane">
          <h2 className="h3">Steps</h2>
          <ul className="steps">
            {(run.steps ?? []).map((s) => (
              <li key={s.id}>
                <button className={selected === s.id ? "step active" : "step"} onClick={() => setSelected(s.id)} aria-pressed={selected === s.id}>
                  <Status status={s.status} /> {s.id}
                  {s.attempt > 1 && <span className="muted small"> attempt {s.attempt}</span>}
                </button>
              </li>
            ))}
          </ul>
        </div>
        <div className="pane side">
          {!step && <p className="muted card">Select a step to see its outputs and logs.</p>}
          {step && (
            <div className="step-detail">
              <h2 className="h3">
                {step.id} <Status status={step.status} />
              </h2>
              {step.error && <div className="notice error">{step.error}</div>}
              {step.approval && <ApprovalFacts approval={step.approval} />}
              {step.await && <AwaitFacts await={step.await} />}
              <dl className="kv">
                <dt>Attempts</dt>
                <dd>{step.attempt}</dd>
                {step.exit_code !== undefined && (
                  <>
                    <dt>Exit code</dt>
                    <dd>{step.exit_code}</dd>
                  </>
                )}
              </dl>
              <h3 className="h4">Outputs</h3>
              <pre className="code">{JSON.stringify(step.outputs ?? {}, null, 2)}</pre>
              <h3 className="h4">Logs</h3>
              <pre className="code logs" ref={logRef} aria-live="polite" aria-label={`Logs for ${step.id}`}>
                {logs[step.id] || (step.log_size ? "…" : "(no output)")}
              </pre>
            </div>
          )}
        </div>
      </div>
    </section>
  );
}

// ApprovalRequest asks for a decision on a waiting approval step. A
// rejection needs a reason; it becomes the step's error.
function ApprovalRequest({ runId, step, onDecided }: { runId: string; step: StepInfo; onDecided: (r: RunInfo) => void }) {
  const [comment, setComment] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error | undefined>();
  const a = step.approval!;
  const decide = (approve: boolean) => {
    setBusy(true);
    setError(undefined);
    api
      .decide(runId, step.id, approve, comment.trim())
      .then(onDecided)
      .catch(setError)
      .finally(() => setBusy(false));
  };
  const label = `approval-${step.id}`;
  return (
    <div className="notice warn approval" role="region" aria-labelledby={label}>
      <h2 className="h3" id={label}>
        Waiting for approval: <code>{step.id}</code>
      </h2>
      <p className="approval-message">{a.message}</p>
      {a.deadline && (
        <p className="muted small">
          Fails if undecided by <When ts={a.deadline} />
        </p>
      )}
      <label className="field">
        <span className="label">Comment</span>
        <textarea rows={2} maxLength={2000} value={comment} onChange={(e) => setComment(e.target.value)} placeholder="Passed to later steps; required to reject" />
      </label>
      {error && <ErrorBox error={error} />}
      <div className="actions">
        <button className="primary" disabled={busy} onClick={() => decide(true)}>
          Approve
        </button>
        <button className="danger" disabled={busy || !comment.trim()} onClick={() => decide(false)} title={comment.trim() ? undefined : "Say why in the comment"}>
          Reject
        </button>
      </div>
    </div>
  );
}

// AwaitRequest shows what a waiting await step waits for and lets a
// person resolve it with a payload, for a daemon its provider cannot
// reach. The daemon checks the fields against the event's declaration.
function AwaitRequest({ runId, step, onDelivered }: { runId: string; step: StepInfo; onDelivered: (r: RunInfo) => void }) {
  const [payload, setPayload] = useState("{}");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error | undefined>();
  const a = step.await!;
  const deliver = () => {
    let body: unknown;
    try {
      body = JSON.parse(payload);
    } catch (e) {
      setError(new Error(`The payload is not JSON: ${(e as Error).message}`));
      return;
    }
    if (!body || typeof body !== "object" || Array.isArray(body)) {
      setError(new Error("The payload must be a JSON object of the event's fields."));
      return;
    }
    setBusy(true);
    setError(undefined);
    api
      .deliver(runId, step.id, body as Record<string, unknown>)
      .then(onDelivered)
      .catch(setError)
      .finally(() => setBusy(false));
  };
  const label = `await-${step.id}`;
  return (
    <div className="notice warn approval" role="region" aria-labelledby={label}>
      <h2 className="h3" id={label}>
        Waiting for <code>{`${a.integration}.${a.event}`}</code>: <code>{step.id}</code>
      </h2>
      <p>
        Key <code>{a.key}</code>
      </p>
      <p className="muted small">
        Fails if no matching event arrives by <When ts={a.deadline} />
      </p>
      <details>
        <summary>Resolve by hand</summary>
        <label className="field">
          <span className="label">Event payload (JSON)</span>
          <textarea rows={4} className="code" spellCheck={false} value={payload} onChange={(e) => setPayload(e.target.value)} />
        </label>
        {error && <ErrorBox error={error} />}
        <div className="actions">
          <button className="primary" disabled={busy} onClick={deliver}>
            Deliver
          </button>
        </div>
      </details>
    </div>
  );
}

function AwaitFacts({ await: a }: { await: NonNullable<StepInfo["await"]> }) {
  return (
    <dl className="kv">
      <dt>Awaits</dt>
      <dd>
        <code>{`${a.integration}.${a.event}`}</code>
      </dd>
      <dt>Key</dt>
      <dd>
        <code>{a.key}</code>
      </dd>
      <dt>State</dt>
      <dd>
        <Status status={a.state} />
      </dd>
      <dt>Deadline</dt>
      <dd>
        <When ts={a.deadline} />
      </dd>
      {a.delivery_id && (
        <>
          <dt>Delivery</dt>
          <dd>
            <code>{a.delivery_id}</code>
          </dd>
        </>
      )}
      {a.actor && (
        <>
          <dt>By</dt>
          <dd>{a.actor}</dd>
        </>
      )}
    </dl>
  );
}

function ApprovalFacts({ approval: a }: { approval: NonNullable<StepInfo["approval"]> }) {
  return (
    <dl className="kv">
      <dt>Request</dt>
      <dd>{a.message}</dd>
      <dt>Decision</dt>
      <dd>{a.decision ? <Status status={a.decision} /> : "pending"}</dd>
      {a.actor && (
        <>
          <dt>By</dt>
          <dd>{a.actor}</dd>
        </>
      )}
      {a.comment && (
        <>
          <dt>Comment</dt>
          <dd>{a.comment}</dd>
        </>
      )}
      {a.decided_at && (
        <>
          <dt>Decided</dt>
          <dd>
            <When ts={a.decided_at} />
          </dd>
        </>
      )}
    </dl>
  );
}
