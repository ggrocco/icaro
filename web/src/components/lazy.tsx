import { lazy, Suspense, type ComponentProps } from "react";
import { Loading } from "./common";

// The editor (CodeMirror) and graph (React Flow) are the heavy parts of
// the bundle; load them on first use.
const YamlEditorImpl = lazy(() => import("./YamlEditor").then((m) => ({ default: m.YamlEditor })));
const RunGraphImpl = lazy(() => import("./RunGraph").then((m) => ({ default: m.RunGraph })));

export function YamlEditor(props: ComponentProps<typeof YamlEditorImpl>) {
  return (
    <Suspense fallback={<Loading what="editor" />}>
      <YamlEditorImpl {...props} />
    </Suspense>
  );
}

export function RunGraph(props: ComponentProps<typeof RunGraphImpl>) {
  return (
    <Suspense fallback={<Loading what="graph" />}>
      <RunGraphImpl {...props} />
    </Suspense>
  );
}
