import { useCallback, useEffect, useState } from "react";
import { ApiError } from "../api";

export function useLoad<T>(fn: () => Promise<T>, deps: unknown[]) {
  const [data, setData] = useState<T | undefined>();
  const [error, setError] = useState<Error | undefined>();
  const [loading, setLoading] = useState(true);
  const reload = useCallback(() => {
    setLoading(true);
    fn()
      .then((d) => {
        setData(d);
        setError(undefined);
      })
      .catch(setError)
      .finally(() => setLoading(false));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
  useEffect(reload, [reload]);
  return { data, error, loading, reload, setData };
}

export function Loading({ what }: { what: string }) {
  return (
    <p className="muted" role="status" aria-live="polite">
      Loading {what}…
    </p>
  );
}

export function ErrorBox({ error, retry }: { error: Error; retry?: () => void }) {
  const issues = error instanceof ApiError ? error.issues : [];
  return (
    <div className="notice error" role="alert">
      <strong>{error instanceof ApiError && error.status === 404 ? "Not found" : "Something went wrong"}:</strong> {error.message}
      {issues.length > 0 && <Issues issues={issues} />}
      {retry && (
        <button type="button" className="ghost" onClick={retry}>
          Retry
        </button>
      )}
    </div>
  );
}

export function Empty({ title, children }: { title: string; children?: React.ReactNode }) {
  return (
    <div className="empty">
      <h3>{title}</h3>
      {children}
    </div>
  );
}

export function Issues({ issues }: { issues: { path: string; message: string; hint?: string }[] }) {
  if (!issues.length) return null;
  return (
    <ul className="issues" aria-label="Validation issues">
      {issues.map((is, i) => (
        <li key={i}>
          <code>{is.path}</code> {is.message}
          {is.hint && <span className="hint"> — {is.hint}</span>}
        </li>
      ))}
    </ul>
  );
}

export function Status({ status }: { status: string }) {
  return <span className={`badge ${status}`}>{status}</span>;
}

export function When({ ts }: { ts?: string }) {
  if (!ts) return <span className="muted">—</span>;
  const d = new Date(ts);
  return <time dateTime={ts}>{d.toLocaleString()}</time>;
}
