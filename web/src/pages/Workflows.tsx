import { api } from "../api";
import { Empty, ErrorBox, Loading, Status, When, useLoad } from "../components/common";
import { href } from "../router";

export function Workflows() {
  const { data, error, loading, reload } = useLoad(api.workflows, []);
  return (
    <section aria-labelledby="wf-title">
      <header className="page-head">
        <h1 id="wf-title">Workflows</h1>
        <a className="button" href={href("new")}>
          New workflow
        </a>
      </header>
      {loading && !data && <Loading what="workflows" />}
      {error && <ErrorBox error={error} retry={reload} />}
      {data && data.length === 0 && (
        <Empty title="No workflows yet">
          <p>
            Create one here, or from a terminal with <code>icaro workflow apply file.yaml</code>. Agents can author them through the{" "}
            <code>icaro-workflows</code> skill.
          </p>
        </Empty>
      )}
      {data && data.length > 0 && (
        <table>
          <thead>
            <tr>
              <th scope="col">Name</th>
              <th scope="col">Version</th>
              <th scope="col">Status</th>
              <th scope="col">Steps</th>
              <th scope="col">Updated</th>
            </tr>
          </thead>
          <tbody>
            {data.map((w) => (
              <tr key={w.name}>
                <td>
                  <a href={href("workflows", w.name)}>{w.name}</a>
                  {w.description && <div className="muted small">{w.description}</div>}
                </td>
                <td>v{w.version}</td>
                <td>
                  <Status status={w.status} />
                </td>
                <td>{w.steps?.length ?? 0}</td>
                <td>
                  <When ts={w.updated_at} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}
