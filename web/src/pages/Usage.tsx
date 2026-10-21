import { api } from "../api";
import { Empty, ErrorBox, Loading, useLoad } from "../components/common";
import { go } from "../router";
import { fmtCost, fmtTokens, summarizeUsage } from "../usage";

const windows = [
  ["1d", "Today and yesterday"],
  ["7d", "Last 7 days"],
  ["30d", "Last 30 days"],
  ["90d", "Last 90 days"],
];

// Usage is the web view of `icaro usage`: token counts from the Claude
// Code transcripts in the profile homes, with source and confidence on
// every row and costs shown only as estimates (ADR 0022).
export function Usage({ since }: { since?: string }) {
  const win = since && windows.some(([k]) => k === since) ? since : "7d";
  const { data, error, loading, reload } = useLoad(() => api.usage(win), [win]);
  const sum = data && summarizeUsage(data.records);
  return (
    <section aria-labelledby="usage-title">
      <header className="page-head">
        <h1 id="usage-title">Usage</h1>
        <div className="toolbar">
          <label className="row">
            <span className="muted small">Window</span>
            <select value={win} onChange={(e) => go("usage", e.target.value)}>
              {windows.map(([k, label]) => (
                <option key={k} value={k}>
                  {label}
                </option>
              ))}
            </select>
          </label>
          <button onClick={reload} disabled={loading}>
            Refresh
          </button>
        </div>
      </header>
      <p className="muted">
        Token counts parsed from the Claude Code transcripts in your profile homes. Costs are list-price <strong>estimates</strong>, never
        billed amounts; models without a price show none.
        {data && (
          <>
            {" "}
            Days follow <code>{data.timezone}</code>.
          </>
        )}
      </p>
      {loading && !data && <Loading what="usage" />}
      {error && <ErrorBox error={error} retry={reload} />}
      {data && sum && (
        <>
          {data.alerts.map((a) => (
            <div key={a.day} className="notice error" role="alert">
              <strong>{a.day}:</strong> estimated spend {fmtCost(a.spent_usd_estimate, a.partial)} reached the daily limit $
              {a.limit_usd.toFixed(2)}
              {a.partial && " (some usage that day is unpriced)"}.
            </div>
          ))}
          {sum.unavailable.map((r) => (
            <div key={`${r.provider}/${r.source}`} className="notice warn">
              <strong>
                {r.provider} {r.source !== "*" && <code>{r.source}</code>}
              </strong>{" "}
              unavailable: {r.error}
            </div>
          ))}
          {sum.days.length === 0 ? (
            <Empty title="No usage in this window">
              <p>
                Usage appears once an agent session writes transcripts to its profile home. To include your own <code>~/.claude</code>, run{" "}
                <code>icaro usage --host-claude</code> in a terminal.
              </p>
            </Empty>
          ) : (
            <>
              <dl className="tiles" aria-label="Totals">
                <Tile label="Messages" value={fmtTokens(sum.total.messages)} />
                <Tile label="Input tokens" value={fmtTokens(sum.total.input)} />
                <Tile label="Output tokens" value={fmtTokens(sum.total.output)} />
                <Tile label="Cache read / write" value={`${fmtTokens(sum.total.cacheRead)} / ${fmtTokens(sum.total.cacheWrite)}`} />
                <Tile
                  label="Estimated cost"
                  value={fmtCost(sum.total.cost, sum.total.unpriced)}
                  note={data.daily_limit_usd ? `daily limit $${data.daily_limit_usd.toFixed(2)}` : undefined}
                />
              </dl>
              <h2 className="h3">By day</h2>
              <div className="scroll-x">
                <table aria-label="Usage by day">
                  <thead>
                    <tr>
                      <th scope="col">Day</th>
                      <th scope="col">Messages</th>
                      <th scope="col">Input</th>
                      <th scope="col">Output</th>
                      <th scope="col">Cache R/W</th>
                      <th scope="col">Cost (est.)</th>
                    </tr>
                  </thead>
                  <tbody>
                    {sum.days.map((d) => (
                      <tr key={d.day} className={data.alerts.some((a) => a.day === d.day) ? "over" : undefined}>
                        <td className="num">{d.day}</td>
                        <td className="num">{fmtTokens(d.messages)}</td>
                        <td className="num">{fmtTokens(d.input)}</td>
                        <td className="num">{fmtTokens(d.output)}</td>
                        <td className="num">
                          {fmtTokens(d.cacheRead)} / {fmtTokens(d.cacheWrite)}
                        </td>
                        <td className="num">{fmtCost(d.cost, d.unpriced)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              <h2 className="h3">By source and model</h2>
              <div className="scroll-x">
                <table aria-label="Usage by source and model">
                  <thead>
                    <tr>
                      <th scope="col">Day</th>
                      <th scope="col">Source</th>
                      <th scope="col">Model</th>
                      <th scope="col">Messages</th>
                      <th scope="col">Input</th>
                      <th scope="col">Output</th>
                      <th scope="col">Cache R/W</th>
                      <th scope="col">Cost (est.)</th>
                      <th scope="col">Confidence</th>
                    </tr>
                  </thead>
                  <tbody>
                    {data.records
                      .filter((r) => !r.error && r.day)
                      .slice()
                      .reverse()
                      .map((r) => (
                        <tr key={`${r.day}/${r.provider}/${r.source}/${r.model}`}>
                          <td className="num">{r.day}</td>
                          <td>
                            <code>{r.source}</code>
                            <div className="muted small">{r.provider}</div>
                          </td>
                          <td className="small num">{r.model}</td>
                          <td className="num">{fmtTokens(r.messages)}</td>
                          <td className="num">{fmtTokens(r.input_tokens)}</td>
                          <td className="num">{fmtTokens(r.output_tokens)}</td>
                          <td className="num">
                            {fmtTokens(r.cache_read_tokens)} / {fmtTokens(r.cache_write_tokens)}
                          </td>
                          <td className="num">{fmtCost(r.cost_usd_estimate)}</td>
                          <td>{r.confidence}</td>
                        </tr>
                      ))}
                  </tbody>
                </table>
              </div>
            </>
          )}
        </>
      )}
    </section>
  );
}

function Tile({ label, value, note }: { label: string; value: string; note?: string }) {
  return (
    <div className="tile">
      <dt className="muted small">{label}</dt>
      <dd>
        {value}
        {note && <div className="muted small">{note}</div>}
      </dd>
    </div>
  );
}
