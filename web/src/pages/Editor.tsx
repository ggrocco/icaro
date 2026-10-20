import { useEffect, useMemo, useRef, useState } from "react";
import { api, ApiError, type Issue } from "../api";
import { FormEditor } from "../components/FormEditor";
import { YamlEditor } from "../components/lazy";
import { RunGraph } from "../components/lazy";
import { ErrorBox, Issues, useLoad } from "../components/common";
import { canonicalJSON, parse } from "../yamldoc";
import { go } from "../router";

const TEMPLATE = `version: 1
name: my-workflow
description: Describe what this workflow does
triggers:
  - type: manual
steps:
  - id: hello
    image: mirror.gcr.io/library/alpine:3.20
    command: echo "hello" && printf '{"greeting":"hi"}' > /icaro/output.json
    outputs:
      greeting: json:greeting
`;

interface Props {
  name?: string;
  initial?: string;
  onSaved?: (name: string) => void;
}

// Editor shows the form and the YAML side by side over one source text,
// validates continuously, and saves only a document the daemon accepts.
export function Editor({ name, initial, onSaved }: Props) {
  const [yaml, setYaml] = useState(initial ?? TEMPLATE);
  const [tab, setTab] = useState<"form" | "yaml" | "json">("form");
  const [issues, setIssues] = useState<Issue[]>([]);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<Error | undefined>();
  const [saved, setSaved] = useState(false);
  const schema = useLoad(api.schema, []);
  const integrations = useLoad(api.integrationDetails, []);
  const timer = useRef<number | undefined>(undefined);

  useEffect(() => {
    if (initial !== undefined) setYaml(initial);
  }, [initial]);

  // Validate as the user types (debounced): the same structured issues as
  // the CLI, API and MCP validate_workflow.
  useEffect(() => {
    window.clearTimeout(timer.current);
    timer.current = window.setTimeout(() => {
      api
        .validate(yaml)
        .then((r) => setIssues(r.issues ?? []))
        .catch((e) => setIssues(e instanceof ApiError ? e.issues : [{ path: "/", message: String(e.message) }]));
    }, 350);
    return () => window.clearTimeout(timer.current);
  }, [yaml]);

  const doc = useMemo(() => parse(yaml), [yaml]);
  const spec = useMemo(() => (doc.errors.length ? undefined : doc.toJS()), [doc]);

  const save = async () => {
    setSaving(true);
    setSaveError(undefined);
    setSaved(false);
    try {
      const v = await api.validate(yaml);
      if (!v.valid) {
        setIssues(v.issues);
        setTab("yaml");
        return;
      }
      const info = await api.apply(yaml);
      setSaved(true);
      onSaved?.(info.name);
      if (!name) go("workflows", info.name);
    } catch (e) {
      setSaveError(e as Error);
      if (e instanceof ApiError && e.issues.length) setIssues(e.issues);
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="editor-page">
      <div className="toolbar" role="toolbar" aria-label="Editor">
        <div role="tablist" aria-label="Editor mode">
          {(["form", "yaml", "json"] as const).map((t) => (
            <button key={t} role="tab" aria-selected={tab === t} className={tab === t ? "tab active" : "tab"} onClick={() => setTab(t)}>
              {t === "json" ? "Canonical JSON" : t.toUpperCase()}
            </button>
          ))}
        </div>
        <span className={issues.length ? "badge failed" : "badge succeeded"} aria-live="polite">
          {issues.length ? `${issues.length} issue(s)` : "valid"}
        </span>
        <button className="primary" onClick={save} disabled={saving || issues.length > 0}>
          {saving ? "Saving…" : "Validate & save"}
        </button>
        {saved && <span className="muted">Saved.</span>}
      </div>
      {saveError && <ErrorBox error={saveError} />}
      <div className="split">
        <div className="pane">
          {tab === "form" && <FormEditor yaml={yaml} onChange={setYaml} integrations={integrations.data ?? []} />}
          {tab === "yaml" && <YamlEditor label="Workflow YAML" value={yaml} onChange={setYaml} issues={issues} schema={schema.data} />}
          {tab === "json" && <pre className="code">{doc.errors.length ? "(YAML does not parse)" : canonicalJSON(doc)}</pre>}
        </div>
        <div className="pane side">
          <h2 className="h3">Graph</h2>
          {spec ? <RunGraph spec={spec} /> : <p className="muted">Fix the YAML to see the graph.</p>}
          <h2 className="h3">Issues</h2>
          {issues.length ? <Issues issues={issues} /> : <p className="muted">No issues.</p>}
        </div>
      </div>
    </div>
  );
}
