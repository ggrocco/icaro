import { api, type HostWorkflow, type TriggerSpec } from "../api";
import { Empty, ErrorBox, Loading, PageHeader, Status, When, useLoad } from "../components/common";
import { Icon } from "../components/icons";
import { href } from "../router";
import { HostRef } from "./Hosts";

// Workflows lists the workflows of this machine and of every host the
// daemon is signed in to; the daemon asks all hosts at once.
export function Workflows() {
  const listing = useLoad(api.hostWorkflows, []);
  const { error, loading, reload } = listing;
  const data = listing.data?.workflows;
  const hosts = listing.data?.hosts ?? [];
  const multi = hosts.length > 1;
  return (
    <section aria-labelledby="wf-title">
      <PageHeader
        id="wf-title"
        title="Workflows"
        subtitle="Pipelines of container, agent and approval steps, started by hand, on a schedule or by a webhook."
      >
        <a className="button" href={href("new")}>
          <Icon name="plus" />
          New workflow
        </a>
      </PageHeader>
      {loading && !data && <Loading what="workflows" />}
      {error && <ErrorBox error={error} retry={reload} />}
      {hosts
        .filter((h) => h.error)
        .map((h) => (
          <div key={h.alias} className="notice warn" role="status">
            <strong>
              {h.label} ({h.alias})
            </strong>{" "}
            could not be listed: {h.error}{" "}
            <button type="button" className="ghost" onClick={reload}>
              Retry
            </button>
          </div>
        ))}
      {data && data.length === 0 && (
        <Empty title="No workflows yet" icon="workflows">
          <p>
            Create one with the form editor, apply a YAML file from a terminal, or let an agent author it through the <code>icaro-workflows</code>{" "}
            skill.
          </p>
          <div className="cmd">icaro workflow apply file.yaml</div>
        </Empty>
      )}
      {data && data.length > 0 && (
        <div className="scroll-x">
          <table>
            <thead>
              <tr>
                <th scope="col">Name</th>
                {multi && <th scope="col">Host</th>}
                <th scope="col">Status</th>
                <th scope="col">Triggers</th>
                <th scope="col">Steps</th>
                <th scope="col">Updated</th>
              </tr>
            </thead>
            <tbody>
              {data.map((w) => (
                <tr key={w.ref}>
                  <td className="workflow-name">
                    <div className="name-cell">
                      <span className="glyph" aria-hidden="true">
                        <Icon name="workflows" />
                      </span>
                      <div>
                        <WorkflowLink w={w} multi={multi} /> <span className="version">v{w.version}</span>
                        {w.description && <div className="muted small">{w.description}</div>}
                      </div>
                    </div>
                  </td>
                  {multi && (
                    <td>
                      <HostRef label={w.host_label} alias={w.host} />
                    </td>
                  )}
                  <td>
                    <Status status={w.status} />
                  </td>
                  <td>
                    <Triggers triggers={w.triggers} />
                  </td>
                  <td>
                    <span className="count">{w.steps?.length ?? 0}</span>
                  </td>
                  <td className="muted">
                    <When ts={w.updated_at} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

// A local workflow opens here; a remote one opens in its host's own UI,
// which has its own sign-in. With several hosts the link shows the ref.
function WorkflowLink({ w, multi }: { w: HostWorkflow; multi: boolean }) {
  const text = multi ? w.ref : w.name;
  if (!w.ui_url) return <a href={href("workflows", w.name)}>{text}</a>;
  return (
    <a href={w.ui_url} target="_blank" rel="noopener noreferrer" title={`Open in ${w.host_label}`}>
      {text} <Icon name="external" />
    </a>
  );
}

function Triggers({ triggers }: { triggers?: TriggerSpec[] }) {
  if (!triggers?.length) return <span className="tag">manual</span>;
  return (
    <div className="trigger-list">
      {triggers.map((t, i) => (
        <span key={i} className="tag" title={t.cron ?? t.event ?? undefined}>
          {t.type === "cron" && t.cron ? `cron ${t.cron}` : t.integration ? `${t.integration}${t.event ? ` · ${t.event}` : ""}` : (t.type ?? "trigger")}
        </span>
      ))}
    </div>
  );
}
