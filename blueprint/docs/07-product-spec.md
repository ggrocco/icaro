# Product — clear definition

## Personas

- An individual developer who switches between Claude Code/Codex/Pi and needs a reproducible environment.
- A tech lead who automates review, migration, and testing through local workflows/webhooks.
- A developer who uses Git worktrees and wants stable local previews.

## Jobs-to-be-done

1. "When I start a project on another machine, I want a reproducible agent environment without polluting my host."
2. "When a branch/worktree appears, I want to see the application at a predictable URL without configuring a proxy manually."
3. "When several agents work on the same project, I want to preserve decisions and context from a verifiable source."
4. "When a repetitive routine occurs, I want an observable, cancelable, and auditable workflow."

## Success metrics

- initial setup < 10 min on a machine with Docker ready;
- starting an isolated CLI < 3 s once the image is available;
- a simple workflow with streaming logs and correct status;
- a preview detected and routed in < 10 s after a valid compose;
- zero duplicate processes and zero orphan previews in the test scenarios.
