import { useState } from "react";
import { assistApi } from "../assist";
import { Empty, ErrorBox, Loading, PageHeader, Status, When, useLoad } from "../components/common";
import { Icon } from "../components/icons";
import { go, href } from "../router";
import { Editor } from "./Editor";

// Assistant lists the workflow assistant's chats (ADR 0035).
export function Assistant() {
  const chats = useLoad(assistApi.list, []);
  return (
    <section aria-labelledby="assist-title">
      <PageHeader
        id="assist-title"
        title="Assistant"
        subtitle="Chats with your agent CLI about workflows. It reads the schema, integrations and runs, and proposes drafts you apply; runs it asks for wait for you."
      >
        <NewChatButton />
      </PageHeader>
      {chats.loading && !chats.data && <Loading what="chats" />}
      {chats.error && <ErrorBox error={chats.error} retry={chats.reload} />}
      {chats.data && chats.data.length === 0 && (
        <Empty title="No chats yet" icon="assistant">
          <p>
            Start one here, or with <strong>Ask the assistant</strong> in any workflow's editor. It runs under your profile's own login, so a
            Claude or ChatGPT subscription works without an API key.
          </p>
        </Empty>
      )}
      {chats.data && chats.data.length > 0 && (
        <div className="scroll-x">
        <table>
          <thead>
            <tr>
              <th>Chat</th>
              <th>Workflow</th>
              <th>Agent</th>
              <th>Container</th>
              <th>Updated</th>
            </tr>
          </thead>
          <tbody>
            {chats.data.map((c) => (
              <tr key={c.id}>
                <td>
                  <a href={href("assistant", c.id)}>{c.title || "Untitled chat"}</a>
                </td>
                <td>{c.workflow ? <a href={href("workflows", c.workflow)}>{c.workflow}</a> : <span className="muted">—</span>}</td>
                <td>{c.agent}</td>
                <td>{c.session_status ? <Status status={c.session_status} /> : <span className="muted">not started</span>}</td>
                <td>
                  <When ts={c.updated_at} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        </div>
      )}
    </section>
  );
}

// NewChatButton starts a chat and opens it.
export function NewChatButton({ label = "New chat" }: { label?: string }) {
  const [error, setError] = useState<Error | undefined>();
  const [busy, setBusy] = useState(false);
  return (
    <>
      <button
        className="primary"
        disabled={busy}
        onClick={async () => {
          setBusy(true);
          setError(undefined);
          try {
            const d = await assistApi.create({});
            go("assistant", d.id);
          } catch (e) {
            setError(e as Error);
          } finally {
            setBusy(false);
          }
        }}
      >
        <Icon name="assistant" />
        {label}
      </button>
      {error && <ErrorBox error={error} />}
    </>
  );
}

// AssistantChat is one chat with its draft open in the editor.
export function AssistantChat({ id }: { id: string }) {
  const chat = useLoad(() => assistApi.get(id), [id]);
  // The editor opens on the draft as it was when the page loaded; later
  // reloads (a turn settled) only refresh the header.
  const [initial, setInitial] = useState<string | undefined | null>(null);
  if (chat.data && initial === null) setInitial(chat.data.draft?.yaml);
  if (chat.error) return <ErrorBox error={chat.error} />;
  if (!chat.data || initial === null) return <Loading what="chat" />;
  const d = chat.data;
  return (
    <section aria-labelledby="chat-title">
      <PageHeader
        id="chat-title"
        title={d.title || "Untitled chat"}
        subtitle={
          <>
            {d.workflow ? (
              <>
                About <a href={href("workflows", d.workflow)}>{d.workflow}</a>.{" "}
              </>
            ) : null}
            Changes the assistant proposes appear beside the editor; save them with Validate &amp; save.
          </>
        }
      >
        <a className="button" href={href("assistant")}>
          All chats
        </a>
        <button
          onClick={async () => {
            if (!window.confirm("Delete this chat, its container and its log?")) return;
            await assistApi.remove(id);
            go("assistant");
          }}
        >
          Delete chat
        </button>
      </PageHeader>
      <Editor key={id} chat={id} initial={initial} onChatSettled={() => chat.reload()} />
    </section>
  );
}
