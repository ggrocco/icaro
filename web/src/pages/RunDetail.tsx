import { useEffect, useMemo, useRef, useState } from "react";
import { api, subscribeRun, terminal, type RunInfo } from "../api";
import { RunGraph } from "../components/lazy";
import { ErrorBox, Loading, Status, When } from "../components/common";
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
      <header className="page-head">
        <h1 id="run-title">
          Run <code>{run.id.slice(-10)}</code> <Status status={run.status} />
        </h1>
        <div className="actions">
          {!terminal(run.status) && (
            <button className="danger" onClick={() => api.cancel(run.id).then((r) => apply(r)).catch(setError)}>
              Cancel
            </button>
          )}
          {terminal(run.status) && (
            <button onClick={() => api.rerun(run).then((r) => go("runs", r.id)).catch(setError)}>Re-run</button>
          )}
        </div>
      </header>
      <p>
        <a href={href("workflows", run.workflow)}>{run.workflow}</a> v{run.version} · trigger {run.trigger_kind} · started <When ts={run.started_at} />
        {run.finished_at && (
          <>
            {" "}
            · finished <When ts={run.finished_at} />
          </>
        )}
        <span className="muted small" aria-live="polite">
          {" "}
          {live ? "· live" : ""}
        </span>
      </p>
      {run.error && <div className="notice error">{run.error}</div>}
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
          {!step && <p className="muted">Select a step to see its outputs and logs.</p>}
          {step && (
            <>
              <h2 className="h3">
                {step.id} <Status status={step.status} />
              </h2>
              {step.error && <div className="notice error">{step.error}</div>}
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
            </>
          )}
        </div>
      </div>
    </section>
  );
}
