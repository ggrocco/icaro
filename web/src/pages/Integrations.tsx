import { useEffect, useState } from "react";
import { api, ApiError, type Issue } from "../api";
import { YamlEditor } from "../components/lazy";
import { Empty, ErrorBox, Issues, Loading, Status, When, useLoad } from "../components/common";
import { href } from "../router";

export function Integrations() {
  const { data, error, loading, reload } = useLoad(api.integrations, []);
  return (
    <section aria-labelledby="int-title">
      <header className="page-head">
        <h1 id="int-title">Integrations</h1>
      </header>
      <p className="muted">
        Integrations are YAML manifests in git-backed packs. Add a pack with <code>icaro integration add &lt;git-url&gt;</code>.
      </p>
      {loading && !data && <Loading what="integrations" />}
      {error && <ErrorBox error={error} retry={reload} />}
      {data && data.length === 0 && <Empty title="No integrations loaded" />}
      {data && data.length > 0 && (
        <table>
          <thead>
            <tr>
              <th scope="col">Name</th>
              <th scope="col">Pack</th>
              <th scope="col">Events</th>
              <th scope="col">Actions</th>
              <th scope="col">Hosts</th>
            </tr>
          </thead>
          <tbody>
            {data.map((i) => (
              <tr key={i.name}>
                <td>
                  <a href={href("integrations", i.name)}>{i.name}</a>
                  {i.error && (
                    <div className="notice error small" role="alert">
                      {i.error}
                    </div>
                  )}
                  {i.shadowed && <div className="muted small">shadows {i.shadowed}</div>}
                </td>
                <td>{i.pack}</td>
                <td>{(i.events ?? []).map((e) => e.event).join(", ") || "—"}</td>
                <td>{(i.actions ?? []).map((a) => a.ref.split(".")[1]).join(", ") || "—"}</td>
                <td className="small">{(i.hosts ?? []).join(", ")}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

// ManifestEditor validates and commits edits to the integration's pack
// repository; an invalid edit is rejected before anything is written.
export function ManifestEditor({ name }: { name: string }) {
  const m = useLoad(() => api.manifest(name), [name]);
  const [yaml, setYaml] = useState("");
  const [issues, setIssues] = useState<Issue[]>([]);
  const [msg, setMsg] = useState<string | undefined>();
  const [err, setErr] = useState<Error | undefined>();
  useEffect(() => {
    if (m.data) setYaml(m.data.yaml);
  }, [m.data]);
  if (m.loading && !m.data) return <Loading what="manifest" />;
  if (m.error) return <ErrorBox error={m.error} />;
  const save = async () => {
    setErr(undefined);
    setMsg(undefined);
    try {
      const r = await api.saveManifest(name, yaml);
      setIssues([]);
      setMsg(`Committed ${r.commit.slice(0, 8)} to the ${m.data!.writable ? m.data!.pack : "local"} pack.`);
    } catch (e) {
      if (e instanceof ApiError && e.issues.length) setIssues(e.issues);
      setErr(e as Error);
    }
  };
  return (
    <section aria-labelledby="man-title">
      <header className="page-head">
        <h1 id="man-title">
          {name} <span className="muted">({m.data!.pack})</span>
        </h1>
        <div className="actions">
          <button className="primary" onClick={save}>
            Validate & commit
          </button>
          {m.data!.writable && <button onClick={() => api.pushPack(m.data!.pack).then((r) => setMsg(r.output)).catch(setErr)}>Push</button>}
        </div>
      </header>
      {!m.data!.writable && <p className="muted">This pack is read-only; saving copies the manifest into your local pack, which shadows it.</p>}
      {msg && <div className="notice">{msg}</div>}
      {err && <ErrorBox error={err} />}
      <Issues issues={issues} />
      <div className="editor-tall">
        <YamlEditor label={`Manifest ${name}`} value={yaml} onChange={setYaml} issues={issues} />
      </div>
    </section>
  );
}

export function Deliveries() {
  const { data, error, loading, reload } = useLoad(api.deliveries, []);
  const [busy, setBusy] = useState<string | undefined>();
  return (
    <section aria-labelledby="del-title">
      <header className="page-head">
        <h1 id="del-title">Webhook deliveries</h1>
        <button onClick={reload}>Refresh</button>
      </header>
      {loading && !data && <Loading what="deliveries" />}
      {error && <ErrorBox error={error} retry={reload} />}
      {data && data.length === 0 && <Empty title="No deliveries yet">Inbound webhooks appear here with their verification result and the runs they started.</Empty>}
      {data && data.length > 0 && (
        <table>
          <thead>
            <tr>
              <th scope="col">Received</th>
              <th scope="col">Integration</th>
              <th scope="col">Verified</th>
              <th scope="col">Matches</th>
              <th scope="col" aria-label="Actions" />
            </tr>
          </thead>
          <tbody>
            {data.map((d) => (
              <tr key={d.id}>
                <td>
                  <When ts={d.received_at} />
                </td>
                <td>
                  {d.integration}
                  {d.event && <span className="muted"> · {d.event}</span>}
                </td>
                <td>{d.verified ? <Status status="succeeded" /> : <span title={d.verify_error}>{<Status status="failed" />}</span>}</td>
                <td className="small">
                  {(d.matches ?? []).length === 0 && <span className="muted">no workflow matched</span>}
                  {(d.matches ?? []).map((m, i) => (
                    <div key={i}>
                      {m.workflow}:{" "}
                      {m.run_id ? <a href={href("runs", m.run_id)}>run</a> : m.filtered ? "filtered out" : m.deduplicated ? "duplicate" : m.skipped ?? "—"}
                    </div>
                  ))}
                </td>
                <td>
                  <button
                    disabled={busy === d.id}
                    onClick={async () => {
                      setBusy(d.id);
                      try {
                        await api.redeliver(d.id);
                        reload();
                      } finally {
                        setBusy(undefined);
                      }
                    }}
                  >
                    Redeliver
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}
