import { useEffect, useRef } from "react";
import { EditorView, basicSetup } from "codemirror";
import { EditorState, Compartment } from "@codemirror/state";
import { yaml } from "@codemirror/lang-yaml";
import { linter, setDiagnostics, type Diagnostic } from "@codemirror/lint";
import { autocompletion, type CompletionContext } from "@codemirror/autocomplete";
import { HighlightStyle, syntaxHighlighting } from "@codemirror/language";
import { tags as t } from "@lezer/highlight";
import type { Issue } from "../api";
import { parse, rangeFor } from "../yamldoc";
import { pathAtLine, propertiesAt } from "../schema";

// The editor theme reads the app's CSS tokens, so it follows the light or
// dark scheme without reconfiguring CodeMirror.
const editorTheme = EditorView.theme({
  "&": { height: "100%", backgroundColor: "var(--surface)", color: "var(--text)", fontSize: "13px" },
  ".cm-scroller": { fontFamily: "var(--font-mono)", lineHeight: "1.6" },
  ".cm-content": { caretColor: "var(--accent)", padding: "10px 0" },
  ".cm-cursor, .cm-dropCursor": { borderLeftColor: "var(--accent)", borderLeftWidth: "2px" },
  ".cm-gutters": { backgroundColor: "var(--surface-2)", color: "var(--muted)", borderRight: "1px solid var(--border)" },
  ".cm-activeLineGutter": { backgroundColor: "var(--surface-3)", color: "var(--text)" },
  ".cm-activeLine": { backgroundColor: "color-mix(in srgb, var(--surface-3) 60%, transparent)" },
  "&.cm-focused": { outline: "none" },
  "&.cm-focused > .cm-scroller > .cm-selectionLayer .cm-selectionBackground, .cm-selectionBackground, ::selection": {
    backgroundColor: "color-mix(in srgb, var(--accent) 25%, transparent) !important",
  },
  ".cm-selectionMatch": { backgroundColor: "var(--accent-soft)" },
  ".cm-matchingBracket, &.cm-focused .cm-matchingBracket": { backgroundColor: "var(--accent-soft)", outline: "1px solid var(--accent)" },
  ".cm-foldPlaceholder": { backgroundColor: "var(--surface-3)", border: "1px solid var(--border)", color: "var(--muted)" },
  ".cm-tooltip": { backgroundColor: "var(--surface-2)", border: "1px solid var(--border-strong)", borderRadius: "8px", boxShadow: "var(--shadow)" },
  ".cm-tooltip-autocomplete > ul > li[aria-selected]": { backgroundColor: "var(--accent-soft)", color: "var(--text)" },
  ".cm-panels": { backgroundColor: "var(--surface-2)", color: "var(--text)" },
  ".cm-diagnostic-error": { borderLeftColor: "var(--bad)" },
});

const highlight = HighlightStyle.define([
  { tag: [t.propertyName, t.definition(t.propertyName)], color: "var(--syn-key)" },
  { tag: [t.string, t.special(t.string), t.content], color: "var(--syn-string)" },
  { tag: [t.number, t.bool, t.null, t.atom], color: "var(--syn-number)" },
  { tag: [t.comment, t.lineComment, t.blockComment], color: "var(--syn-comment)", fontStyle: "italic" },
  { tag: [t.keyword, t.typeName, t.labelName, t.meta], color: "var(--syn-keyword)" },
  { tag: [t.punctuation, t.separator, t.squareBracket, t.brace], color: "var(--muted)" },
]);

interface Props {
  value: string;
  onChange: (v: string) => void;
  issues: Issue[];
  schema?: object;
  label: string;
}

// YamlEditor is CodeMirror bound to the workflow JSON Schema: key
// completion comes from the schema at the cursor, and the daemon's
// structured issues render inline at their JSON Pointer.
export function YamlEditor({ value, onChange, issues, schema, label }: Props) {
  const host = useRef<HTMLDivElement>(null);
  const view = useRef<EditorView | null>(null);
  const schemaRef = useRef<object | undefined>(schema);
  const onChangeRef = useRef(onChange);
  schemaRef.current = schema;
  onChangeRef.current = onChange;

  useEffect(() => {
    if (!host.current) return;
    const theme = new Compartment();
    const complete = (ctx: CompletionContext) => {
      const word = ctx.matchBefore(/[A-Za-z_-]*/);
      if (!schemaRef.current || !word) return null;
      const line = ctx.state.doc.lineAt(ctx.pos);
      const before = line.text.slice(0, ctx.pos - line.from);
      if (!/^\s*(-\s+)?[A-Za-z_-]*$/.test(before)) return null;
      const path = pathAtLine(ctx.state.doc.toString(), line.number - 1, ctx.pos - line.from);
      if (!path) return null;
      const props = propertiesAt(schemaRef.current as Record<string, unknown>, path);
      if (!props.length) return null;
      return {
        from: word.from,
        options: props.map((p) => ({ label: p.name, type: "property", info: p.description, apply: p.name + ": " })),
      };
    };
    view.current = new EditorView({
      parent: host.current,
      state: EditorState.create({
        doc: value,
        extensions: [
          basicSetup,
          yaml(),
          linter(null),
          autocompletion({ override: [complete] }),
          theme.of([editorTheme, syntaxHighlighting(highlight)]),
          EditorView.contentAttributes.of({ "aria-label": label }),
          EditorView.updateListener.of((u) => {
            if (u.docChanged) onChangeRef.current(u.state.doc.toString());
          }),
        ],
      }),
    });
    return () => view.current?.destroy();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // External value changes (form edits) replace the document.
  useEffect(() => {
    const v = view.current;
    if (v && v.state.doc.toString() !== value) {
      v.dispatch({ changes: { from: 0, to: v.state.doc.length, insert: value } });
    }
  }, [value]);

  useEffect(() => {
    const v = view.current;
    if (!v) return;
    const doc = parse(v.state.doc.toString());
    const len = v.state.doc.length;
    const diags: Diagnostic[] = issues.map((is) => {
      const [from, to] = rangeFor(doc, is.path);
      return {
        from: Math.min(from, len),
        to: Math.min(Math.max(to, from + 1), len),
        severity: "error",
        message: `${is.path}: ${is.message}${is.hint ? ` (${is.hint})` : ""}`,
      };
    });
    v.dispatch(setDiagnostics(v.state, diags));
  }, [issues]);

  return <div className="editor" ref={host} />;
}
