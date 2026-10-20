import { useEffect, useState } from "react";

// Hash routes: #/workflows, #/workflows/<name>, #/runs/<id>, ...
export function useRoute(): string[] {
  const read = () => (window.location.hash.replace(/^#\/?/, "") || "workflows").split("/").map(decodeURIComponent);
  const [route, setRoute] = useState(read);
  useEffect(() => {
    const on = () => setRoute(read());
    window.addEventListener("hashchange", on);
    return () => window.removeEventListener("hashchange", on);
  }, []);
  return route;
}

export const href = (...parts: string[]) => "#/" + parts.map(encodeURIComponent).join("/");
export const go = (...parts: string[]) => {
  window.location.hash = href(...parts);
};
