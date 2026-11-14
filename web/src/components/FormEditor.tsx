import { cloneElement, useId, useMemo } from "react";
import type { ActionInfo, AgentProfileInfo, IntegrationInfo } from "../api";
import { getIn, parse, setIn, stepCount } from "../yamldoc";

interface Props {
  yaml: string;
  onChange: (yaml: string) => void;
  integrations: IntegrationInfo[];
  // agentProfiles are the operator's workflows.agent_profiles (ADR 0037).
  agentProfiles?: AgentProfileInfo[];
}

type KV = Record<string, string>;

// FormEditor edits a workflow through the YAML Document, so unknown
// fields and comments survive every edit (Phase 4: "a round-trip does not
// lose unknown fields").
export function FormEditor({ yaml, onChange, integrations, agentProfiles = [] }: Props) {
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
        const kind = isUses ? "uses" : doc.hasIn(p("approval")) ? "approval" : doc.hasIn(p("wait")) ? "wait" : doc.hasIn(p("agent")) ? "agent" : "image";
        const pauses = kind === "approval" || kind === "wait";
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
                value={kind}
                onChange={(e) =>
                  mutate((d) => {
                    // Each form has its own fields; switching drops the old ones.
                    for (const k of ["image", "command", "env", "outputs", "uses", "input", "approval", "wait", "agent"]) d.deleteIn(["steps", i, k]);
                    if (e.target.value === "approval" || e.target.value === "wait") d.deleteIn(["steps", i, "retry"]);
                    if (e.target.value === "wait") d.deleteIn(["steps", i, "timeout"]);
                    if (e.target.value === "uses") d.setIn(["steps", i, "uses"], [...actions.keys()][0] ?? "webhook.post");
                    else if (e.target.value === "approval") d.setIn(["steps", i, "approval"], { message: "Continue?" });
                    else if (e.target.value === "wait") d.setIn(["steps", i, "wait"], "10m");
                    else if (e.target.value === "agent") d.setIn(["steps", i, "agent"], { prompt: "Describe the task for the agent." });
                    else d.setIn(["steps", i, "image"], "mirror.gcr.io/library/alpine:3.24");
                  })
                }
              >
                <option value="image">image (container)</option>
                <option value="agent">agent (an agent CLI does the task)</option>
                <option value="uses">uses (integration action)</option>
                <option value="approval">approval (a person decides)</option>
                <option value="wait">wait (pause for a time)</option>
              </select>
            </Field>
            {kind === "approval" ? (
              <Field label="Message" hint="Shown to the approver; may use ${{ trigger.* }} and ${{ steps.*.outputs.* }}">
                <textarea
                  rows={2}
                  value={String(getIn(doc, ["steps", i, "approval", "message"]) ?? "")}
                  onChange={(e) => update(["steps", i, "approval", "message"], e.target.value || undefined)}
                />
              </Field>
            ) : kind === "wait" ? (
              <Field label="Duration" hint="e.g. 30m or 2h (at most 720h)">
                <input value={String(getIn(doc, p("wait")) ?? "")} onChange={(e) => update(p("wait"), e.target.value)} />
              </Field>
            ) : kind === "agent" ? (
              <AgentFields
                profiles={agentProfiles}
                profile={String(getIn(doc, ["steps", i, "agent", "profile"]) ?? "")}
                prompt={String(getIn(doc, ["steps", i, "agent", "prompt"]) ?? "")}
                params={(getIn<Record<string, unknown>>(doc, ["steps", i, "agent", "params"]) ?? {}) as Record<string, unknown>}
                memory={getIn(doc, ["steps", i, "agent", "memory"]) !== false}
                env={(getIn<KV>(doc, p("env")) ?? {}) as KV}
                outputs={(getIn<KV>(doc, p("outputs")) ?? {}) as KV}
                update={(k, v) => update(["steps", i, "agent", ...k], v)}
                updateStep={(k, v) => update(p(k), v)}
              />
            ) : isUses ? (
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
              {kind !== "wait" && (
                <Field label={kind === "approval" ? "Deadline" : "Timeout"} hint={kind === "approval" ? "undecided after this, it fails" : undefined}>
                  <input placeholder={kind === "approval" ? "24h" : "10m"} value={String(getIn(doc, p("timeout")) ?? "")} onChange={(e) => update(p("timeout"), e.target.value)} />
                </Field>
              )}
              {!pauses && (
                <Field label="Retry">
                  <input
                    type="number"
                    min={0}
                    max={5}
                    value={String(getIn(doc, p("retry")) ?? "")}
                    onChange={(e) => update(p("retry"), e.target.value === "" ? undefined : Number(e.target.value))}
                  />
                </Field>
              )}
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
            const step = { id: `step-${n + 1}`, image: "mirror.gcr.io/library/alpine:3.24", command: "echo hello" };
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

// AgentFields edits an agent step (ADR 0037): the profile among the
// operator's workflows.agent_profiles, the prompt, the harness's params,
// memory, env and outputs. The agent's final answer is outputs.result.
function AgentFields(props: {
  profiles: AgentProfileInfo[];
  profile: string;
  prompt: string;
  params: Record<string, unknown>;
  memory: boolean;
  env: KV;
  outputs: KV;
  update: (path: string[], v: unknown) => void;
  updateStep: (key: string, v: unknown) => void;
}) {
  const { profiles, profile, prompt, params, memory, env, outputs, update, updateStep } = props;
  const chosen = profiles.find((x) => x.name === profile) ?? (profile ? undefined : profiles[0]);
  if (profiles.length === 0) {
    return (
      <p className="notice warn" role="status">
        Agent steps are off: no profile is listed in <code>workflows.agent_profiles</code>. An operator enables them in the config.
      </p>
    );
  }
  return (
    <>
      <Field label="Profile" hint="its login, egress allowlist and credentials are what the agent gets">
        <select value={profile} onChange={(e) => update(["profile"], e.target.value || undefined)}>
          <option value="">{`default (${profiles[0].name})`}</option>
          {profile && !profiles.some((x) => x.name === profile) && <option value={profile}>{profile} (not listed)</option>}
          {profiles.map((x) => (
            <option key={x.name} value={x.name}>
              {x.name} ({x.agent})
            </option>
          ))}
        </select>
      </Field>
      <Field label="Prompt" hint="fed on stdin; may use ${{ steps.*.outputs.* }} and ${{ trigger.* }}, never secrets">
        <textarea rows={5} value={prompt} onChange={(e) => update(["prompt"], e.target.value)} />
      </Field>
      {(chosen?.params ?? []).map((pp) => {
        const v = params[pp.name];
        const set = (val: unknown) => update(["params", pp.name], val === "" ? undefined : val);
        return (
          <Field key={pp.name} label={pp.name} hint={pp.description}>
            {pp.type === "bool" ? (
              <input type="checkbox" checked={v === true} onChange={(e) => set(e.target.checked || undefined)} />
            ) : pp.type === "enum" ? (
              <select value={String(v ?? "")} onChange={(e) => set(e.target.value)}>
                <option value="">(CLI default)</option>
                {(pp.enum ?? []).map((o) => (
                  <option key={o}>{o}</option>
                ))}
              </select>
            ) : pp.type === "int" ? (
              <input type="number" min={pp.min} value={v === undefined ? "" : String(v)} onChange={(e) => set(e.target.value === "" ? "" : Number(e.target.value))} />
            ) : (
              <input value={String(v ?? "")} placeholder="(CLI default)" onChange={(e) => set(e.target.value)} />
            )}
          </Field>
        );
      })}
      <Field label="Memory" hint="the project memory tools, scoped by the daemon">
        <input type="checkbox" checked={memory} onChange={(e) => update(["memory"], e.target.checked ? undefined : false)} />
      </Field>
      <KVEditor label="Environment" hint="pass secrets the agent needs here, not in the prompt" value={env} onChange={(v) => updateStep("env", Object.keys(v).length ? v : undefined)} />
      <KVEditor
        label="Outputs"
        hint="besides the built-in result: name → json:<path> from /icaro/output.json, or file:<path>"
        value={outputs}
        onChange={(v) => updateStep("outputs", Object.keys(v).length ? v : undefined)}
      />
    </>
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
