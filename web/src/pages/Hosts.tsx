import { useState } from "react";
import { api, type HostInfo } from "../api";
import { ErrorBox, Loading, PageHeader, When, useLoad } from "../components/common";
import { Icon } from "../components/icons";

// Hosts lists this machine and every remote Ícaro daemon the local daemon
// is signed in to. Any number can be signed in at once; each has an alias
// (the short name in references such as prod:deploy) and a label.
export function Hosts() {
  const { data, error, loading, reload } = useLoad(api.hosts, []);
  const [err, setErr] = useState<Error | undefined>();
  const signOut = async (h: HostInfo) => {
    if (!window.confirm(`Sign out of ${h.label} (${h.alias})? Its workflows leave the list until you sign in again.`)) return;
    setErr(undefined);
    try {
      await api.hostLogout(h.alias);
      reload();
    } catch (e) {
      setErr(e as Error);
    }
  };
  return (
    <section aria-labelledby="hosts-title">
      <PageHeader
        id="hosts-title"
        title="Hosts"
        subtitle="This machine and the remote Ícaro daemons it is signed in to. Workflows lists all of them at once."
      >
        <button onClick={reload}>
          <Icon name="refresh" />
          Refresh
        </button>
      </PageHeader>
      {loading && !data && <Loading what="hosts" />}
      {error && <ErrorBox error={error} retry={reload} />}
      {err && <ErrorBox error={err} />}
      {data && (
        <div className="scroll-x">
          <table>
            <thead>
              <tr>
                <th scope="col">Host</th>
                <th scope="col">URL</th>
                <th scope="col">Signed in as</th>
                <th scope="col">Since</th>
                <th scope="col">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {data.map((h) => (
                <tr key={h.alias}>
                  <td>
                    <HostRef label={h.label} alias={h.alias} />
                  </td>
                  <td className="muted">{h.local ? "this machine" : h.url}</td>
                  <td>{h.local ? <span className="muted">no sign-in needed</span> : `${h.principal} · ${h.scope}`}</td>
                  <td className="muted">{h.local ? "—" : <When ts={h.signed_in_at} />}</td>
                  <td>
                    {!h.local && (
                      <button className="ghost" onClick={() => signOut(h)}>
                        <Icon name="logout" />
                        Sign out
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <SignIn onDone={reload} />
    </section>
  );
}

// HostRef shows a host's label with its alias, as references do.
export function HostRef({ label, alias }: { label: string; alias: string }) {
  return (
    <span className="host-ref">
      <Icon name="hosts" />
      <span>{label}</span> <span className="tag">{alias}</span>
    </span>
  );
}

function SignIn({ onDone }: { onDone: () => void }) {
  const [form, setForm] = useState({ alias: "", label: "", url: "", token: "" });
  const [err, setErr] = useState<Error | undefined>();
  const [busy, setBusy] = useState(false);
  const set = (k: keyof typeof form) => (e: React.ChangeEvent<HTMLInputElement>) => setForm({ ...form, [k]: e.target.value });
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setErr(undefined);
    setBusy(true);
    try {
      await api.hostLogin(form);
      setForm({ alias: "", label: "", url: "", token: "" });
      onDone();
    } catch (e) {
      setErr(e as Error);
    } finally {
      setBusy(false);
    }
  };
  return (
    <form className="card form host-signin" onSubmit={submit} aria-labelledby="signin-title">
      <h2 id="signin-title">Sign in to a host</h2>
      <p className="muted small">
        Create a token on that host with <code>icaro token create --name NAME --scope read</code> (or <code>write</code>). The daemon checks it,
        seals it with its master key and only ever sends it to that URL. Signing in again with an alias replaces it.
      </p>
      <div className="row">
        <label className="field">
          <span className="label">Alias</span>
          <input value={form.alias} onChange={set("alias")} placeholder="prod" pattern="[a-z][a-z0-9-]{0,31}" required />
        </label>
        <label className="field">
          <span className="label">Label</span>
          <input value={form.label} onChange={set("label")} placeholder="Production" maxLength={100} />
        </label>
        <label className="field">
          <span className="label">URL</span>
          <input value={form.url} onChange={set("url")} placeholder="https://icaro.example.com" type="url" required />
        </label>
        <label className="field">
          <span className="label">API token</span>
          <input value={form.token} onChange={set("token")} type="password" autoComplete="off" placeholder="icaro_…" required />
        </label>
      </div>
      <button className="primary" type="submit" disabled={busy || !form.alias || !form.url || !form.token}>
        Sign in
      </button>
      {err && <ErrorBox error={err} />}
    </form>
  );
}
