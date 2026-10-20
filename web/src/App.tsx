import { useEffect, useState } from "react";
import { api, ApiError } from "./api";
import { ErrorBox, Loading, useLoad } from "./components/common";
import { Editor } from "./pages/Editor";
import { Deliveries, Integrations, ManifestEditor } from "./pages/Integrations";
import { RunDetail } from "./pages/RunDetail";
import { RunTable, WorkflowDetail } from "./pages/WorkflowDetail";
import { Workflows } from "./pages/Workflows";
import { href, useRoute } from "./router";

export function App() {
  const [me, setMe] = useState<{ name: string } | null | undefined>();
  useEffect(() => {
    api
      .me()
      .then(setMe)
      .catch((e) => (e instanceof ApiError && e.status === 401 ? setMe(null) : setMe(null)));
  }, []);
  if (me === undefined) return <Loading what="session" />;
  if (me === null) return <Login onLogin={setMe} />;
  return <Shell me={me} />;
}

function Shell({ me }: { me: { name: string } }) {
  const route = useRoute();
  const [section, a, b] = route;
  const nav = [
    ["workflows", "Workflows"],
    ["runs", "Runs"],
    ["deliveries", "Deliveries"],
    ["integrations", "Integrations"],
  ];
  return (
    <>
      <a className="skip" href="#main">
        Skip to content
      </a>
      <header className="top">
        <a className="brand" href={href("workflows")}>
          <span aria-hidden="true">☀︎</span> Ícaro
        </a>
        <nav aria-label="Main">
          {nav.map(([k, label]) => (
            <a key={k} href={href(k)} aria-current={section === k || (k === "workflows" && section === "new") ? "page" : undefined}>
              {label}
            </a>
          ))}
        </nav>
        <span className="muted small">
          {me.name}{" "}
          <button className="ghost" onClick={() => api.logout().then(() => window.location.reload())}>
            Sign out
          </button>
        </span>
      </header>
      <main id="main" tabIndex={-1}>
        {section === "workflows" && !a && <Workflows />}
        {section === "workflows" && a && <WorkflowDetail name={a} tab={b} />}
        {section === "new" && (
          <section aria-labelledby="new-title">
            <h1 id="new-title">New workflow</h1>
            <Editor />
          </section>
        )}
        {section === "runs" && !a && <AllRuns />}
        {section === "runs" && a && <RunDetail id={a} />}
        {section === "deliveries" && <Deliveries />}
        {section === "integrations" && !a && <Integrations />}
        {section === "integrations" && a && <ManifestEditor name={a} />}
        {!["workflows", "new", "runs", "deliveries", "integrations"].includes(section) && <p>Page not found.</p>}
      </main>
    </>
  );
}

function AllRuns() {
  const runs = useLoad(() => api.runs(), []);
  return (
    <section aria-labelledby="runs-title">
      <header className="page-head">
        <h1 id="runs-title">Runs</h1>
        <button onClick={runs.reload}>Refresh</button>
      </header>
      {runs.loading && !runs.data && <Loading what="runs" />}
      {runs.error && <ErrorBox error={runs.error} retry={runs.reload} />}
      {runs.data && <RunTable runs={runs.data} />}
    </section>
  );
}

function Login({ onLogin }: { onLogin: (me: { name: string }) => void }) {
  const [token, setToken] = useState(() => new URLSearchParams(window.location.search).get("token") ?? "");
  const [err, setErr] = useState<Error | undefined>();
  const submit = async (e?: React.FormEvent) => {
    e?.preventDefault();
    setErr(undefined);
    try {
      const me = await api.login(token.trim());
      window.history.replaceState(null, "", window.location.pathname + window.location.hash);
      onLogin(me);
    } catch (e) {
      setErr(e as Error);
    }
  };
  useEffect(() => {
    // `icaro ui` opens a one-time login link.
    if (token) void submit();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  return (
    <main className="login" id="main">
      <h1>
        <span aria-hidden="true">☀︎</span> Ícaro
      </h1>
      <form onSubmit={submit}>
        <label className="field">
          <span className="label">API token</span>
          <input type="password" autoComplete="off" value={token} onChange={(e) => setToken(e.target.value)} autoFocus />
          <span className="hint">
            Run <code>icaro ui</code> for a one-time sign-in link, or paste a token from <code>icaro token create</code>.
          </span>
        </label>
        <button className="primary" type="submit" disabled={!token}>
          Sign in
        </button>
      </form>
      {err && <ErrorBox error={err} />}
    </main>
  );
}
