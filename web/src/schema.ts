// Minimal JSON Schema navigation for editor completion and the form editor.
import { isMap, isSeq, parseDocument, type Node } from "yaml";

type S = Record<string, unknown>;

function resolve(root: S, s: S | undefined): S | undefined {
  let cur = s;
  for (let i = 0; cur && typeof cur.$ref === "string" && i < 16; i++) {
    const ref = cur.$ref as string;
    if (!ref.startsWith("#/")) return cur;
    cur = ref
      .slice(2)
      .split("/")
      .reduce<unknown>((acc, k) => (acc as S | undefined)?.[k], root) as S | undefined;
  }
  return cur;
}

// branches flattens oneOf/anyOf/allOf into the schemas a value may match.
function branches(root: S, s: S | undefined): S[] {
  const r = resolve(root, s);
  if (!r) return [];
  const out: S[] = [r];
  for (const k of ["oneOf", "anyOf", "allOf"]) {
    const arr = r[k];
    if (Array.isArray(arr)) for (const b of arr) out.push(...branches(root, b as S));
  }
  return out;
}

export function schemaAt(root: S, path: (string | number)[]): S[] {
  let cur: S[] = [root];
  for (const seg of path) {
    const next: S[] = [];
    for (const s of cur.flatMap((c) => branches(root, c))) {
      if (typeof seg === "number") {
        if (s.items) next.push(s.items as S);
      } else {
        const props = s.properties as S | undefined;
        if (props && props[seg]) next.push(props[seg] as S);
        else if (s.additionalProperties && typeof s.additionalProperties === "object") next.push(s.additionalProperties as S);
      }
    }
    cur = next;
    if (!cur.length) return [];
  }
  return cur.flatMap((c) => branches(root, c));
}

export interface PropInfo {
  name: string;
  description?: string;
  enum?: unknown[];
}

export function propertiesAt(root: S, path: (string | number)[]): PropInfo[] {
  const seen = new Map<string, PropInfo>();
  for (const s of schemaAt(root, path)) {
    const props = s.properties as Record<string, S> | undefined;
    if (!props) continue;
    for (const [name, p] of Object.entries(props)) {
      if (seen.has(name)) continue;
      const r = resolve(root, p) ?? p;
      seen.set(name, { name, description: r.description as string | undefined, enum: r.enum as unknown[] | undefined });
    }
  }
  return [...seen.values()];
}

const MARK = "__icaro_cursor__";

// pathAtLine finds the mapping path a new key typed on `line` (0-based)
// would belong to, by inserting a marker key there and parsing.
export function pathAtLine(src: string, line: number, column: number): (string | number)[] | null {
  const lines = src.split("\n");
  const cur = lines[line] ?? "";
  const indent = cur.slice(0, column).match(/^(\s*(?:-\s+)?)/)?.[1] ?? "";
  lines[line] = indent + MARK + ": 0";
  const doc = parseDocument(lines.join("\n"));
  if (doc.errors.length) return null;
  const found = find(doc.contents as Node | null, []);
  return found;
}

function find(n: Node | null, path: (string | number)[]): (string | number)[] | null {
  if (!n) return null;
  if (isMap(n)) {
    for (const p of n.items) {
      const k = (p.key as { value?: unknown })?.value ?? p.key;
      if (k === MARK) return path;
      const r = find(p.value as Node, [...path, String(k)]);
      if (r) return r;
    }
  } else if (isSeq(n)) {
    for (let i = 0; i < n.items.length; i++) {
      const r = find(n.items[i] as Node, [...path, i]);
      if (r) return r;
    }
  }
  return null;
}
