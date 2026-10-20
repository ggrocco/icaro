import { useMemo, useState } from "react";
import { api, type VersionInfo } from "../api";
import { Editor } from "./Editor";
import { RunGraph } from "../components/lazy";
import { Empty, ErrorBox, Loading, Status, When, useLoad } from "../components/common";
import { diffLines } from "../diff";
import { go, href } from "../router";
import { parse } from "../yamldoc";

export function WorkflowDetail({ name, tab = "overview" }: { name: string; tab?: string }) {
  const wf = useLoad(() => api.workflow(name), [name]);
  const runs = useLoad(() => api.runs(name), [name]);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<Error | undefined>();

  const spec = useMemo(() => {
    if (!wf.data?.yaml) return undefined;
    const d = parse(wf.data.yaml);
    return d.errors.length ? undefined : d.toJS();
  }, [wf.data?.yaml]);

  if (wf.loading && !wf.data) return <Loading what="workflow" />;
  if (wf.error) return <ErrorBox error={wf.error} retry={wf.reload} />;
  const w = wf.data!;

  const runNow = async () => {
    setBusy(true);
    setErr(undefined);
    try {
      const fields = triggerFields(spec);
      const trigger: Record<string, unknown> = {};
      for (const f of fields) {
        const v = window.prompt(`trigger.${f}`);
        if (v === null) return;
        trigger[f] = /^-?\d+(\.\d+)?$/.test(v) ? Number(v) : v;
      }
      const run = await api.run(name, trigger);
      go("runs", run.id);
    } catch (e) {
      setErr(e as Error);
    } finally {
      setBusy(false);
    }
  };

  return (
    <section aria-labelledby="wfd-title">
      <header className="page-head">
        <h1 id="wfd-title">
          {w.name} <span className="muted">v{w.version}</span> <Status status={w.status} />
        </h1>
        <div className="actions">
          <button className="primary" onClick={runNow} disabled={busy || w.status !== "active"}>
            Run
          </button>
          <button
            onClick={async () => {
              await api.setEnabled(name, w.status !== "active");
              wf.reload();
            }}
          >
            {w.status === "active" ? "Disable" : "Enable"}
          </button>
        </div>
      </header>
      {w.description && <p>{w.description}</p>}
      {err && <ErrorBox error={err} />}
      <nav className="tabs" aria-label="Workflow sections">
        {["overview", "edit", "versions", "runs"].map((t) => (
          <a key={t} href={href("workflows", name, t)} aria-current={tab === t ? "page" : undefined} className={tab === t ? "tab active" : "tab"}>
            {t}
          </a>
        ))}
      </nav>
      {tab === "overview" && (spec ? <RunGraph spec={spec} /> : <p className="muted">No graph.</p>)}
      {tab === "edit" && <Editor name={name} initial={w.yaml} onSaved={() => wf.reload()} />}
      {tab === "versions" && <Versions name={name} current={w.version} />}
      {tab === "runs" && (
        <>
          {runs.error && <ErrorBox error={runs.error} retry={runs.reload} />}
          <RunTable runs={runs.data ?? []} />
        </>
      )}
    </section>
  );
}

// triggerFields lists trigger.* keys the workflow references, which a
// manual run must supply.
export function triggerFields(spec: unknown): string[] {
  const out = new Set<string>();
  const walk = (v: unknown) => {
    if (typeof v === "string") {
      for (const m of v.matchAll(/trigger\.([A-Za-z_][A-Za-z0-9_-]*)/g)) out.add(m[1]);
    } else if (Array.isArray(v)) v.forEach(walk);
    else if (v && typeof v === "object") {
      for (const [k, val] of Object.entries(v)) if (k !== "triggers") walk(val);
    }
  };
  walk(spec);
  return [...out].sort();
}

function Versions({ name, current }: { name: string; current: number }) {
  const list = useLoad(() => api.versions(name), [name]);
  const [a, setA] = useState(Math.max(1, current - 1));
  const [b, setB] = useState(current);
  const va = useLoad<VersionInfo>(() => api.version(name, a), [name, a]);
  const vb = useLoad<VersionInfo>(() => api.version(name, b), [name, b]);
  const lines = useMemo(() => (va.data?.yaml !== undefined && vb.data?.yaml !== undefined ? diffLines(va.data.yaml, vb.data.yaml) : []), [va.data, vb.data]);
  if (list.loading && !list.data) return <Loading what="versions" />;
  if (list.error) return <ErrorBox error={list.error} />;
  const versions = list.data ?? [];
  return (
    <div>
      <div className="row">
        <label className="field">
          <span className="label">From</span>
          <select value={a} onChange={(e) => setA(Number(e.target.value))}>
            {versions.map((v) => (
              <option key={v.version} value={v.version}>
                v{v.version} · {v.actor}
              </option>
            ))}
          </select>
        </label>
        <label className="field">
          <span className="label">To</span>
          <select value={b} onChange={(e) => setB(Number(e.target.value))}>
            {versions.map((v) => (
              <option key={v.version} value={v.version}>
                v{v.version} · {v.actor}
              </option>
            ))}
          </select>
        </label>
      </div>
      <pre className="code diff" aria-label={`Diff v${a} to v${b}`}>
        {lines.map((l, i) => (
          <div key={i} className={l.kind}>
            {l.kind === "add" ? "+ " : l.kind === "del" ? "- " : "  "}
            {l.text}
          </div>
        ))}
      </pre>
    </div>
  );
}

export function RunTable({ runs }: { runs: { id: string; workflow: string; status: string; trigger: string; created_at: string; version: number }[] }) {
  if (!runs.length) return <Empty title="No runs yet" />;
  return (
    <table>
      <thead>
        <tr>
          <th scope="col">Run</th>
          <th scope="col">Workflow</th>
          <th scope="col">Status</th>
          <th scope="col">Trigger</th>
          <th scope="col">Created</th>
        </tr>
      </thead>
      <tbody>
        {runs.map((r) => (
          <tr key={r.id}>
            <td>
              <a href={href("runs", r.id)}>
                <code>{r.id.slice(-10)}</code>
              </a>
            </td>
            <td>
              {r.workflow} <span className="muted">v{r.version}</span>
            </td>
            <td>
              <Status status={r.status} />
            </td>
            <td>{r.trigger}</td>
            <td>
              <When ts={r.created_at} />
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}
