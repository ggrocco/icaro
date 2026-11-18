import { useEffect, useState } from "react";
import { api, ApiError, type Me } from "./api";
import { ErrorBox, Loading, PageHeader, useLoad } from "./components/common";
import { Icon, Logo, type IconName } from "./components/icons";
import { Assistant, AssistantChat } from "./pages/Assistant";
import { Editor } from "./pages/Editor";
import { Hosts } from "./pages/Hosts";
import { Deliveries, Integrations, ManifestEditor } from "./pages/Integrations";
import { RunDetail } from "./pages/RunDetail";
import { Usage } from "./pages/Usage";
import { RunTable, WorkflowDetail } from "./pages/WorkflowDetail";
import { Workflows } from "./pages/Workflows";
import { href, useRoute } from "./router";

export function App() {
  const [me, setMe] = useState<Me | null | undefined>();
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

function Shell({ me }: { me: Me }) {
  const route = useRoute();
  const [section, a, b] = route;
  const nav: [string, string, IconName][] = [
    ["workflows", "Workflows", "workflows"],
    ["assistant", "Assistant", "assistant"],
    ["runs", "Runs", "runs"],
    ["deliveries", "Deliveries", "deliveries"],
    ["integrations", "Integrations", "integrations"],
    ["hosts", "Hosts", "hosts"],
    ["usage", "Usage", "usage"],
  ];
  return (
    <div className="shell">
      <a className="skip" href="#main">
        Skip to content
      </a>
      <aside className="sidebar">
        <a className="brand" href={href("workflows")}>
          <Logo />
          <span className="wordmark">Ícaro</span>
        </a>
        <nav aria-label="Main">
          <span className="nav-label" aria-hidden="true">
            Automation
          </span>
          {nav.map(([k, label, icon]) => (
            <a key={k} href={href(k)} aria-current={section === k || (k === "workflows" && section === "new") ? "page" : undefined}>
              <Icon name={icon} />
              <span>{label}</span>
            </a>
          ))}
        </nav>
        <div className="sidebar-foot">
          <span className="avatar" aria-hidden="true">
            {(me.local ? "This machine" : me.name).replace(/^[^A-Za-z0-9]+/, "").charAt(0) || "?"}
          </span>
          <span className="who" title={me.local ? "This machine: no sign-in needed (daemon.ui_trust_loopback)" : me.name}>
            {me.local ? "This machine" : me.name}
          </span>
          {/* The trusted loopback UI has no session to sign out of. */}
          {!me.local && (
            <button
              className="ghost icon-btn"
              aria-label="Sign out"
              title="Sign out"
              onClick={() => api.logout().then(() => window.location.reload())}
            >
              <Icon name="logout" />
            </button>
          )}
        </div>
      </aside>
      <main id="main" tabIndex={-1}>
        {section === "workflows" && !a && <Workflows />}
        {section === "workflows" && a && <WorkflowDetail name={a} tab={b} />}
        {section === "new" && (
          <section aria-labelledby="new-title">
            <PageHeader
              id="new-title"
              title="New workflow"
              subtitle="Edit with the form or YAML; it is validated as you type and saved only when the daemon accepts it."
            />
            <Editor />
          </section>
        )}
        {section === "assistant" && !a && <Assistant />}
        {section === "assistant" && a && <AssistantChat id={a} />}
        {section === "runs" && !a && <AllRuns />}
        {section === "runs" && a && <RunDetail id={a} />}
        {section === "deliveries" && <Deliveries />}
        {section === "integrations" && !a && <Integrations />}
        {section === "integrations" && a && <ManifestEditor name={a} />}
        {section === "hosts" && <Hosts />}
        {section === "usage" && <Usage since={a} />}
        {!["workflows", "new", "assistant", "runs", "deliveries", "integrations", "hosts", "usage"].includes(section) && <p>Page not found.</p>}
      </main>
    </div>
  );
}

function AllRuns() {
  const runs = useLoad(() => api.runs(), []);
  return (
    <section aria-labelledby="runs-title">
      <PageHeader id="runs-title" title="Runs" subtitle="Every workflow run, newest first. Open one to follow its steps and logs live.">
        <button onClick={runs.reload}>
          <Icon name="refresh" />
          Refresh
        </button>
      </PageHeader>
      {runs.loading && !runs.data && <Loading what="runs" />}
      {runs.error && <ErrorBox error={runs.error} retry={runs.reload} />}
      {runs.data && <RunTable runs={runs.data} />}
    </section>
  );
}

function Login({ onLogin }: { onLogin: (me: Me) => void }) {
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
      <div className="login-card">
        <Logo />
        <h1>Sign in to Ícaro</h1>
        <p className="subtitle">Workflows, runs and integrations for your agents.</p>
        <form onSubmit={submit}>
          <label className="field">
            <span className="label">API token</span>
            <input type="password" autoComplete="off" value={token} onChange={(e) => setToken(e.target.value)} autoFocus placeholder="icaro_…" />
            <span className="hint">
              Run <code>icaro ui</code> for a one-time sign-in link, or paste a token from <code>icaro token create</code>. On your own machine,{" "}
              <code>daemon.ui_trust_loopback: true</code> skips signing in.
            </span>
          </label>
          <button className="primary" type="submit" disabled={!token}>
            Sign in
          </button>
        </form>
        {err && <ErrorBox error={err} />}
      </div>
    </main>
  );
}
