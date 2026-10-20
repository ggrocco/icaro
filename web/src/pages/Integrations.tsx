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
        Integrations are YAML manifests in git-backed packs. Add a pack with <code>icaro integration add &lt;git-url&gt;</code>; edits made here
        commit to your <code>local</code> pack.
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
                  {i.shadows_builtin && <div className="muted small">shadows the builtin</div>}
                  {i.description && <div className="muted small">{i.description}</div>}
                </td>
                <td>{i.pack}</td>
                <td className="small">{i.events.join(", ") || "—"}</td>
                <td className="small">{i.actions.join(", ") || "—"}</td>
                <td className="small">{i.hosts.join(", ")}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

// ManifestEditor validates and commits edits to the local pack; an invalid
// edit is rejected before anything is written, and push stays manual.
export function ManifestEditor({ name }: { name: string }) {
  const m = useLoad(() => api.integration(name), [name]);
  const [yaml, setYaml] = useState("");
  const [issues, setIssues] = useState<Issue[]>([]);
  const [msg, setMsg] = useState<string | undefined>();
  const [err, setErr] = useState<Error | undefined>();
  const [saved, setSaved] = useState(false);
  useEffect(() => {
    if (m.data?.manifest !== undefined) setYaml(m.data.manifest);
  }, [m.data?.manifest]);
  if (m.loading && !m.data) return <Loading what="manifest" />;
  if (m.error) return <ErrorBox error={m.error} />;
  const info = m.data!;
  const save = async () => {
    setErr(undefined);
    setMsg(undefined);
    try {
      const r = await api.saveManifest(name, yaml);
      setIssues([]);
      setSaved(true);
      setMsg(r.changed ? `Committed ${r.commit.slice(0, 8)} to the ${r.pack} pack.` : "No changes.");
      m.reload();
    } catch (e) {
      if (e instanceof ApiError && e.issues.length) setIssues(e.issues);
      setErr(e as Error);
    }
  };
  return (
    <section aria-labelledby="man-title">
      <header className="page-head">
        <h1 id="man-title">
          {name} <span className="muted">({info.pack})</span>
        </h1>
        <div className="actions">
          <button className="primary" onClick={save}>
            Validate & commit
          </button>
          {(info.pack === "local" || saved) && (
            <button
              onClick={() =>
                api
                  .pushPack("local")
                  .then(() => setMsg("Pushed the local pack."))
                  .catch(setErr)
              }
            >
              Push
            </button>
          )}
        </div>
      </header>
      {info.pack !== "local" && (
        <p className="muted">
          This manifest lives in the read-only <code>{info.pack}</code> pack; saving writes a copy to your local pack, which shadows it.
        </p>
      )}
      {msg && (
        <div className="notice" role="status">
          {msg}
        </div>
      )}
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
  const [err, setErr] = useState<Error | undefined>();
  return (
    <section aria-labelledby="del-title">
      <header className="page-head">
        <h1 id="del-title">Webhook deliveries</h1>
        <button onClick={reload}>Refresh</button>
      </header>
      {loading && !data && <Loading what="deliveries" />}
      {error && <ErrorBox error={error} retry={reload} />}
      {err && <ErrorBox error={err} />}
      {data && data.length === 0 && (
        <Empty title="No deliveries yet">Inbound webhooks appear here with their verification result and the runs they started.</Empty>
      )}
      {data && data.length > 0 && (
        <table>
          <thead>
            <tr>
              <th scope="col">Received</th>
              <th scope="col">Integration</th>
              <th scope="col">Status</th>
              <th scope="col">Workflows</th>
              <th scope="col">
                <span className="sr-only">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {data.map((d) => (
              <tr key={d.id}>
                <td>
                  <When ts={d.received_at} />
                  {d.redelivery_of && <div className="muted small">redelivery</div>}
                </td>
                <td>
                  {d.integration}
                  {d.events.length > 0 && <span className="muted"> · {d.events.map((e) => e.event).join(", ")}</span>}
                </td>
                <td>
                  <Status status={d.status} />
                  {(d.verify_error || d.error) && <div className="small muted">{d.verify_error || d.error}</div>}
                </td>
                <td className="small">
                  {d.results.length === 0 && <span className="muted">no workflow matched</span>}
                  {d.results.map((m, i) => (
                    <div key={i}>
                      {m.workflow}:{" "}
                      {m.run_id ? (
                        <a href={href("runs", m.run_id)}>{m.deduplicated ? "duplicate of run" : "run"}</a>
                      ) : m.filtered ? (
                        "filtered out"
                      ) : (
                        m.skipped ?? m.error ?? "—"
                      )}
                    </div>
                  ))}
                </td>
                <td>
                  <button
                    disabled={busy === d.id || !d.verified}
                    title={d.verified ? "Run the pipeline again from this record" : "Only verified deliveries can be redelivered"}
                    onClick={async () => {
                      setBusy(d.id);
                      setErr(undefined);
                      try {
                        await api.redeliver(d.id);
                        reload();
                      } catch (e) {
                        setErr(e as Error);
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
