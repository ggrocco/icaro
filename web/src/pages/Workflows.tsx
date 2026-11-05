import { api, type TriggerSpec } from "../api";
import { Empty, ErrorBox, Loading, PageHeader, Status, When, useLoad } from "../components/common";
import { Icon } from "../components/icons";
import { href } from "../router";

export function Workflows() {
  const { data, error, loading, reload } = useLoad(api.workflows, []);
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
                <th scope="col">Status</th>
                <th scope="col">Triggers</th>
                <th scope="col">Steps</th>
                <th scope="col">Updated</th>
              </tr>
            </thead>
            <tbody>
              {data.map((w) => (
                <tr key={w.name}>
                  <td className="workflow-name">
                    <div className="name-cell">
                      <span className="glyph" aria-hidden="true">
                        <Icon name="workflows" />
                      </span>
                      <div>
                        <a href={href("workflows", w.name)}>{w.name}</a> <span className="version">v{w.version}</span>
                        {w.description && <div className="muted small">{w.description}</div>}
                      </div>
                    </div>
                  </td>
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
