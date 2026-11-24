// DraftSync keeps a chat's draft in step with the editor (ADR 0035): the
// person's edits are sent after a quiet period, so typing is not one write
// per key, and flush sends a pending edit at once.
export interface DraftSync {
  // edit records the editor's document; it is sent after the quiet period
  // unless the draft already holds it.
  edit: (yaml: string) => void;
  // synced records that the draft already holds yaml (the editor accepted
  // the assistant's proposal, or the chat was created with it).
  synced: (yaml: string) => void;
  // flush sends a pending edit now. Call it before a turn starts, so the
  // assistant reads what the editor holds and the write cannot land on top
  // of the draft the turn proposes.
  flush: () => Promise<void>;
  // cancel drops a pending edit.
  cancel: () => void;
}

export function draftSync(put: (yaml: string) => Promise<unknown>, initial: string | undefined, onError: (e: Error) => void, delay = 800): DraftSync {
  let last = initial;
  let pending: string | undefined;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const cancel = () => {
    clearTimeout(timer);
    timer = undefined;
    pending = undefined;
  };
  const send = async (yaml: string) => {
    cancel();
    last = yaml;
    await put(yaml);
  };
  return {
    edit(yaml) {
      cancel();
      if (yaml === last) return;
      pending = yaml;
      timer = setTimeout(() => send(yaml).catch(onError), delay);
    },
    synced(yaml) {
      cancel();
      last = yaml;
    },
    flush: () => (pending === undefined ? Promise.resolve() : send(pending)),
    cancel,
  };
}
