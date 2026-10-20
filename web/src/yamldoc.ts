// Helpers over the `yaml` Document model. Edits go through the Document so
// fields the form does not know about (and comments) survive a round trip.
import { Document, isMap, isSeq, parseDocument, type Node } from "yaml";

export function parse(src: string): Document.Parsed {
  return parseDocument(src, { keepSourceTokens: true });
}

// pointerToPath turns "/steps/0/id" into ["steps", 0, "id"].
export function pointerToPath(ptr: string): (string | number)[] {
  if (ptr === "" || ptr === "/") return [];
  return ptr
    .split("/")
    .slice(1)
    .map((s) => s.replace(/~1/g, "/").replace(/~0/g, "~"))
    .map((s) => (/^\d+$/.test(s) ? Number(s) : s));
}

// rangeFor finds the source range of a JSON Pointer, falling back to the
// closest existing ancestor (e.g. an unknown key reports its parent map's
// key when the key itself is the problem).
export function rangeFor(doc: Document.Parsed, ptr: string): [number, number] {
  const path = pointerToPath(ptr);
  for (let n = path.length; n >= 0; n--) {
    const sub = path.slice(0, n);
    const node = sub.length === 0 ? doc.contents : (doc.getIn(sub, true) as Node | undefined);
    if (node && (node as Node).range) {
      // Prefer the key's range for map entries.
      if (sub.length > 0) {
        const parent = sub.length === 1 ? doc.contents : (doc.getIn(sub.slice(0, -1), true) as Node | undefined);
        if (parent && isMap(parent)) {
          const pair = parent.items.find((p) => String((p.key as { value?: unknown })?.value ?? p.key) === String(sub[sub.length - 1]));
          const key = pair?.key as Node | undefined;
          if (key?.range) return [key.range[0], (node as Node).range![1]];
        }
      }
      const r = (node as Node).range!;
      return [r[0], r[1]];
    }
  }
  return [0, 0];
}

// setIn sets a value, deleting the key when value is undefined or "".
export function setIn(doc: Document.Parsed, path: (string | number)[], value: unknown) {
  if (value === undefined || value === "" || (Array.isArray(value) && value.length === 0)) {
    if (doc.hasIn(path)) doc.deleteIn(path);
    return;
  }
  doc.setIn(path, value);
}

export function getIn<T = unknown>(doc: Document.Parsed, path: (string | number)[]): T | undefined {
  const v = doc.getIn(path);
  if (v && typeof v === "object" && "toJSON" in (v as object)) return (v as { toJSON(): T }).toJSON();
  return v as T | undefined;
}

export function stepCount(doc: Document.Parsed): number {
  const steps = doc.getIn(["steps"], true);
  return isSeq(steps) ? steps.items.length : 0;
}

// canonicalJSON is the JSON the daemon stores, with stable key order.
export function canonicalJSON(doc: Document.Parsed): string {
  return JSON.stringify(sortKeys(doc.toJS()), null, 2);
}

function sortKeys(v: unknown): unknown {
  if (Array.isArray(v)) return v.map(sortKeys);
  if (v && typeof v === "object") {
    const out: Record<string, unknown> = {};
    for (const k of Object.keys(v as object).sort()) out[k] = sortKeys((v as Record<string, unknown>)[k]);
    return out;
  }
  return v;
}
