import { useCallback, useEffect, useState } from "react";
import { ApiError } from "../api";
import { Icon, type IconName } from "./icons";

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
    <div className="loading" role="status" aria-live="polite">
      <span className="sr-only">Loading {what}…</span>
      <div className="bar" aria-hidden="true" />
      <div className="bar" aria-hidden="true" />
      <div className="bar" aria-hidden="true" />
    </div>
  );
}

// PageHeader is the title row every page starts with: the h1 (which the
// section is labelled by), an optional one-line description, and actions.
export function PageHeader({
  id,
  title,
  subtitle,
  children,
}: {
  id: string;
  title: React.ReactNode;
  subtitle?: React.ReactNode;
  children?: React.ReactNode;
}) {
  return (
    <header className="page-head">
      <div>
        <h1 id={id}>{title}</h1>
        {subtitle && <p className="subtitle">{subtitle}</p>}
      </div>
      {children && <div className="actions">{children}</div>}
    </header>
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

export function Empty({ title, icon, children }: { title: string; icon?: IconName; children?: React.ReactNode }) {
  return (
    <div className="empty">
      {icon && (
        <div className="empty-icon">
          <Icon name={icon} />
        </div>
      )}
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

// When shows a relative time ("5 min ago") with the full local time on
// hover; dates older than a week show the date itself.
export function When({ ts }: { ts?: string }) {
  if (!ts) return <span className="muted">—</span>;
  const d = new Date(ts);
  return (
    <time dateTime={ts} title={d.toLocaleString()}>
      {relativeTime(d)}
    </time>
  );
}

export function relativeTime(d: Date, now = new Date()): string {
  const s = Math.round((now.getTime() - d.getTime()) / 1000);
  if (Number.isNaN(s)) return "—";
  const abs = Math.abs(s);
  const fmt = (n: number, unit: string) => (s >= 0 ? `${n} ${unit} ago` : `in ${n} ${unit}`);
  if (abs < 45) return s >= 0 ? "just now" : "in a moment";
  if (abs < 3600) return fmt(Math.round(abs / 60), "min");
  if (abs < 86400) return fmt(Math.round(abs / 3600), "h");
  if (abs < 7 * 86400) return fmt(Math.round(abs / 86400), Math.round(abs / 86400) === 1 ? "day" : "days");
  return d.toLocaleDateString(undefined, { year: d.getFullYear() === now.getFullYear() ? undefined : "numeric", month: "short", day: "numeric" });
}
