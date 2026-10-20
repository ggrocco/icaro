import { cloneElement, useId, useMemo } from "react";
import type { ActionInfo, IntegrationInfo } from "../api";
import { getIn, parse, setIn, stepCount } from "../yamldoc";

interface Props {
  yaml: string;
  onChange: (yaml: string) => void;
  integrations: IntegrationInfo[];
}

type KV = Record<string, string>;

// FormEditor edits a workflow through the YAML Document, so unknown
// fields and comments survive every edit (Phase 4: "a round-trip does not
// lose unknown fields").
export function FormEditor({ yaml, onChange, integrations }: Props) {
  const doc = useMemo(() => parse(yaml), [yaml]);
  const actions = useMemo(() => {
    const m = new Map<string, ActionInfo>();
    for (const i of integrations) for (const a of Object.values(i.action_details ?? {})) m.set(a.ref, a);
    return m;
  }, [integrations]);

  if (doc.errors.length) {
    return (
      <div className="notice error" role="alert">
        The YAML does not parse ({doc.errors[0].message}). Fix it in the YAML tab; the form edits a valid document.
      </div>
    );
  }

  const update = (path: (string | number)[], value: unknown) => {
    const d = parse(yaml);
    setIn(d, path, value);
    onChange(d.toString());
  };
  const mutate = (fn: (d: ReturnType<typeof parse>) => void) => {
    const d = parse(yaml);
    fn(d);
    onChange(d.toString());
  };

  const n = stepCount(doc);
  const ids = Array.from({ length: n }, (_, i) => String(getIn(doc, ["steps", i, "id"]) ?? ""));
  const triggers = (getIn<unknown[]>(doc, ["triggers"]) ?? []) as Record<string, string>[];

  return (
    <form className="form" onSubmit={(e) => e.preventDefault()}>
      <fieldset>
        <legend>Workflow</legend>
        <Field label="Name" hint="lowercase letters, digits and dashes">
          <input value={String(getIn(doc, ["name"]) ?? "")} onChange={(e) => update(["name"], e.target.value)} />
        </Field>
        <Field label="Description">
          <input value={String(getIn(doc, ["description"]) ?? "")} onChange={(e) => update(["description"], e.target.value)} />
        </Field>
        <Field label="Concurrency">
          <select value={String(getIn(doc, ["concurrency"]) ?? "")} onChange={(e) => update(["concurrency"], e.target.value)}>
            <option value="">default (allow)</option>
            {["forbid", "queue", "replace", "allow"].map((c) => (
              <option key={c}>{c}</option>
            ))}
          </select>
        </Field>
        <KVEditor label="Environment" value={(getIn<KV>(doc, ["env"]) ?? {}) as KV} onChange={(v) => update(["env"], Object.keys(v).length ? v : undefined)} />
      </fieldset>

      <fieldset>
        <legend>Triggers</legend>
        {triggers.length === 0 && <p className="muted">No triggers: the workflow runs manually.</p>}
        {triggers.map((t, i) => (
          <div className="row" key={i}>
            <select
              aria-label={`Trigger ${i + 1} kind`}
              value={t.integration ? `int:${t.integration}` : t.type ?? "manual"}
              onChange={(e) => {
                const v = e.target.value;
                mutate((d) => {
                  if (v.startsWith("int:")) d.setIn(["triggers", i], { integration: v.slice(4), event: "" });
                  else d.setIn(["triggers", i], v === "cron" ? { type: "cron", cron: "0 * * * *" } : { type: v });
                });
              }}
            >
              <option value="manual">manual</option>
              <option value="cron">cron</option>
              {integrations
                .filter((x) => Object.keys(x.event_details ?? {}).length)
                .map((x) => (
                  <option key={x.name} value={`int:${x.name}`}>
                    {x.name}
                  </option>
                ))}
            </select>
            {t.type === "cron" && (
              <input aria-label="Cron expression" value={t.cron ?? ""} onChange={(e) => update(["triggers", i, "cron"], e.target.value)} />
            )}
            {t.integration && (
              <>
                <select aria-label="Event" value={t.event ?? ""} onChange={(e) => update(["triggers", i, "event"], e.target.value)}>
                  <option value="">choose an event…</option>
                  {Object.values(integrations.find((x) => x.name === t.integration)?.event_details ?? {}).map((ev) => (
                    <option key={ev.event} value={ev.event} title={ev.description}>
                      {ev.event}
                    </option>
                  ))}
                </select>
                <input
                  aria-label="Filter expression"
                  placeholder="filter, e.g. trigger.base_branch == 'main'"
                  value={t.filter ?? ""}
                  onChange={(e) => update(["triggers", i, "filter"], e.target.value)}
                />
              </>
            )}
            <button type="button" className="ghost" onClick={() => mutate((d) => d.deleteIn(["triggers", i]))} aria-label={`Remove trigger ${i + 1}`}>
              Remove
            </button>
          </div>
        ))}
        <button type="button" onClick={() => mutate((d) => (d.hasIn(["triggers"]) ? d.addIn(["triggers"], { type: "manual" }) : d.setIn(["triggers"], [{ type: "manual" }])))}>
          Add trigger
        </button>
      </fieldset>

      {Array.from({ length: n }, (_, i) => {
        const p = (k: string) => ["steps", i, k];
        const uses = String(getIn(doc, p("uses")) ?? "");
        const isUses = doc.hasIn(p("uses"));
        const action = actions.get(uses);
        const needs = (getIn<string[]>(doc, p("needs")) ?? []) as string[];
        return (
          <fieldset key={i}>
            <legend>Step {ids[i] || i + 1}</legend>
            <Field label="Id">
              <input value={ids[i]} onChange={(e) => update(p("id"), e.target.value)} />
            </Field>
            <Field label="Kind">
              <select
                value={isUses ? "uses" : "image"}
                onChange={(e) =>
                  mutate((d) => {
                    if (e.target.value === "uses") {
                      for (const k of ["image", "command", "env", "outputs"]) d.deleteIn(["steps", i, k]);
                      d.setIn(["steps", i, "uses"], [...actions.keys()][0] ?? "webhook.post");
                    } else {
                      d.deleteIn(["steps", i, "uses"]);
                      d.setIn(["steps", i, "image"], "mirror.gcr.io/library/alpine:3.20");
                    }
                  })
                }
              >
                <option value="image">image (container)</option>
                <option value="uses">uses (integration action)</option>
              </select>
            </Field>
            {isUses ? (
              <>
                <Field label="Action">
                  <select value={uses} onChange={(e) => update(p("uses"), e.target.value)}>
                    {!action && <option value={uses}>{uses || "choose…"}</option>}
                    {[...actions.values()].map((a) => (
                      <option key={a.ref} value={a.ref} title={a.description}>
                        {a.ref}
                      </option>
                    ))}
                  </select>
                </Field>
                {action ? (
                  Object.entries(action.inputs).map(([name, info]) => (
                    <Field key={name} label={`${name}${info.required ? " *" : ""}`} hint={info.description}>
                      <input
                        value={toText(getIn(doc, ["steps", i, "input", name]))}
                        placeholder={info.default !== undefined ? String(info.default) : info.type}
                        onChange={(e) => update(["steps", i, "input", name], fromText(e.target.value, info.type))}
                      />
                    </Field>
                  ))
                ) : (
                  <p className="muted">Unknown action: install its integration to get a typed form.</p>
                )}
              </>
            ) : (
              <>
                <Field label="Image">
                  <input value={String(getIn(doc, p("image")) ?? "")} onChange={(e) => update(p("image"), e.target.value)} />
                </Field>
                <Field label="Command">
                  <textarea rows={3} value={String(getIn(doc, p("command")) ?? "")} onChange={(e) => update(p("command"), e.target.value)} />
                </Field>
                <KVEditor label="Environment" value={(getIn<KV>(doc, p("env")) ?? {}) as KV} onChange={(v) => update(p("env"), Object.keys(v).length ? v : undefined)} />
                <KVEditor
                  label="Outputs"
                  hint="name → capture: json:<path>, stdout, stdout:last-line or file:<path>"
                  value={(getIn<KV>(doc, p("outputs")) ?? {}) as KV}
                  onChange={(v) => update(p("outputs"), Object.keys(v).length ? v : undefined)}
                />
              </>
            )}
            <Field label="Needs">
              <div className="chips" role="group" aria-label="Needs">
                {ids
                  .filter((id, j) => j !== i && id)
                  .map((id) => (
                    <label key={id} className="chip">
                      <input
                        type="checkbox"
                        checked={needs.includes(id)}
                        onChange={(e) => update(p("needs"), e.target.checked ? [...needs, id] : needs.filter((x) => x !== id))}
                      />
                      {id}
                    </label>
                  ))}
              </div>
            </Field>
            <Field label="When" hint="e.g. steps.build.outputs.changed == true">
              <input value={String(getIn(doc, p("when")) ?? "")} onChange={(e) => update(p("when"), e.target.value)} />
            </Field>
            <div className="row">
              <Field label="Timeout">
                <input placeholder="10m" value={String(getIn(doc, p("timeout")) ?? "")} onChange={(e) => update(p("timeout"), e.target.value)} />
              </Field>
              <Field label="Retry">
                <input
                  type="number"
                  min={0}
                  max={5}
                  value={String(getIn(doc, p("retry")) ?? "")}
                  onChange={(e) => update(p("retry"), e.target.value === "" ? undefined : Number(e.target.value))}
                />
              </Field>
            </div>
            <button type="button" className="ghost danger" onClick={() => mutate((d) => d.deleteIn(["steps", i]))}>
              Remove step
            </button>
          </fieldset>
        );
      })}
      <button
        type="button"
        onClick={() =>
          mutate((d) => {
            const step = { id: `step-${n + 1}`, image: "mirror.gcr.io/library/alpine:3.20", command: "echo hello" };
            if (d.hasIn(["steps"])) d.addIn(["steps"], step);
            else d.setIn(["steps"], [step]);
          })
        }
      >
        Add step
      </button>
    </form>
  );
}

function toText(v: unknown): string {
  if (v === undefined || v === null) return "";
  return typeof v === "string" ? v : JSON.stringify(v);
}

function fromText(s: string, type: string): unknown {
  if (s === "") return undefined;
  if (s.includes("${{")) return s;
  if (type === "number" && !isNaN(Number(s))) return Number(s);
  if (type === "boolean" && (s === "true" || s === "false")) return s === "true";
  if (type === "object" || type === "array") {
    try {
      return JSON.parse(s);
    } catch {
      return s;
    }
  }
  return s;
}

// Field labels a control; the hint is linked with aria-describedby so it
// is announced as a description, not as part of the name.
function Field({ label, hint, children }: { label: string; hint?: string; children: React.ReactElement<{ "aria-describedby"?: string }> }) {
  const id = useId();
  const control = hint ? cloneElement(children, { "aria-describedby": id }) : children;
  return (
    <div className="field">
      <label>
        <span className="label">{label}</span>
        {control}
      </label>
      {hint && (
        <span className="hint" id={id}>
          {hint}
        </span>
      )}
    </div>
  );
}

function KVEditor({ label, value, onChange, hint }: { label: string; value: KV; onChange: (v: KV) => void; hint?: string }) {
  const entries = Object.entries(value);
  const set = (rows: [string, string][]) => onChange(Object.fromEntries(rows.filter(([k]) => k !== "")));
  return (
    <div className="field">
      <span className="label">{label}</span>
      {entries.map(([k, v], i) => (
        <div className="row" key={i}>
          <input aria-label={`${label} name ${i + 1}`} value={k} onChange={(e) => set(entries.map((r, j) => (j === i ? [e.target.value, r[1]] : r)))} />
          <input aria-label={`${label} value ${i + 1}`} value={v} onChange={(e) => set(entries.map((r, j) => (j === i ? [r[0], e.target.value] : r)))} />
          <button type="button" className="ghost" aria-label={`Remove ${k}`} onClick={() => set(entries.filter((_, j) => j !== i))}>
            ×
          </button>
        </div>
      ))}
      <button type="button" className="ghost" onClick={() => set([...entries, [`KEY_${entries.length + 1}`, ""]])}>
        Add
      </button>
      {hint && <span className="hint">{hint}</span>}
    </div>
  );
}
