package workflow

// fieldAnnotations carries the constraints and descriptions reflection
// cannot infer, keyed by <GoType>.<json field>, as JSON fragments merged
// over the reflected property. "notype" drops the inferred JSON type for
// const/enum-only properties.
var fieldAnnotations = map[string]string{
	"Spec.version":            "{\"const\": 1, \"notype\": true}",
	"Spec.name":               "{\"pattern\": \"^[a-z][a-z0-9-]{2,62}$\"}",
	"Spec.description":        "{\"maxLength\": 500}",
	"Spec.concurrency":        "{\"enum\": [\"forbid\", \"queue\", \"replace\", \"allow\"], \"notype\": true}",
	"Spec.env":                "{\"description\": \"Workflow-level environment, available to every step and to interpolation as ${{ env.<key> }}. Values may reference secrets via ${{ secrets.<key> }}; secrets are resolved by the daemon's secret store and never inlined.\"}",
	"Spec.triggers":           "{\"description\": \"Single source of truth for what starts a workflow. Built-in triggers are `manual` and `cron`; everything else is an integration trigger referencing a manifest event (ADR 0007). There is no separate top-level `schedule`; a cron trigger carries its own expression.\"}",
	"Spec.steps":              "{\"minItems\": 1}",
	"TriggerSpec.type":        "{\"enum\": [\"manual\", \"cron\"], \"notype\": true}",
	"TriggerSpec.cron":        "{\"description\": \"Required when type=cron; a standard cron expression.\", \"maxLength\": 100}",
	"TriggerSpec.integration": "{\"pattern\": \"^[a-z][a-z0-9-]{1,62}$\", \"description\": \"Integration manifest name, resolved across installed packs.\"}",
	"TriggerSpec.event":       "{\"description\": \"Event name declared by the manifest's triggers section.\", \"maxLength\": 200}",
	"TriggerSpec.filter":      "{\"description\": \"Optional boolean expression over the normalized ${{ trigger.* }} payload; the run is enqueued only when it holds.\", \"maxLength\": 2000}",
	"RepositoryRef.ref":       "{\"description\": \"Repository identifier/remote.\"}",
	"RepositoryRef.revision":  "{\"description\": \"Branch, tag, or commit. Defaults to the repo's default branch when omitted.\"}",
	"StepSpec.id":             "{\"pattern\": \"^[a-z][a-z0-9-]{1,62}$\"}",
	"StepSpec.image":          "{\"description\": \"Container image reference to run for this step (e.g. ghcr.io/icaro-dev/agent-base:<version> for an agent, or any image for shell/js work). Mutually exclusive with `uses`.\", \"minLength\": 1, \"maxLength\": 512}",
	"StepSpec.uses":           "{\"pattern\": \"^[a-z][a-z0-9-]{1,62}\\\\.[a-z][a-z0-9-]{1,62}$\", \"description\": \"Integration action reference as <integration>.<action> (e.g. slack.post-message), resolved against installed manifest packs. Executed in-process by the daemon's HTTP action executor with host-bound credentials. Mutually exclusive with `image`.\"}",
	"StepSpec.needs":          "{\"uniqueItems\": true}",
	"StepSpec.repository":     "{}",
	"StepSpec.timeout":        "{\"pattern\": \"^[0-9]+(ms|s|m|h)$\"}",
	"StepSpec.retry":          "{\"minimum\": 0, \"maximum\": 5}",
	"StepSpec.command":        "{\"description\": \"Command run inside the image (image steps only; semantic validation rejects it on uses steps). For an agent image this is the agent CLI invocation.\", \"maxLength\": 20000}",
	"StepSpec.env":            "{\"description\": \"Step-level environment, merged over workflow env. Supports interpolation. Image steps only.\"}",
	"StepSpec.input":          "{\"description\": \"Inputs to the step. For uses steps these are validated against the action's typed input declaration in the manifest. Values may use ${{ steps.<id>.outputs.<key> }}, ${{ trigger.<key> }}, ${{ env.<key> }}, and ${{ secrets.<key> }} interpolation.\"}",
	"StepSpec.outputs":        "{\"description\": \"Named outputs this step produces, consumable by dependent steps. For image steps, values describe how each is captured (e.g. from stdout, a file, or a JSON path). For uses steps, outputs are declared by the action's manifest and this field is not set (semantic validation rejects it).\"}",
	"StepSpec.when":           "{\"description\": \"Optional gating on any step: a boolean expression over interpolated values that must hold for the step to run. This replaces a dedicated condition step type. The expression grammar is defined separately and validated semantically.\", \"maxLength\": 2000}",
}

// Document-level and $defs-level schema text.
const (
	schemaTitle                   = "Icaro Workflow v1"
	schemaDescription             = "A workflow orchestrates sessions (ADR 0002). It is not general-purpose CI. Data flow between steps is part of the contract: steps expose `outputs`, later steps reference them via ${{ steps.<id>.outputs.<key> }} interpolation. Runs started by an integration trigger also expose the normalized event payload as ${{ trigger.<key> }} (ADR 0007)."
	integrationTriggerDescription = "Subscribes the workflow to an event defined by an integration manifest (e.g. integration: github, event: pull_request.opened). The manifest verifies the inbound webhook and normalizes its payload into the ${{ trigger.* }} context. Manual runs prompt for the payload fields."
	repositoryRefDescription      = "A repository resolved once per (ref, revision) for the run."
	stepDescription               = "A step takes exactly one of two forms (ADR 0007): `image` runs a container image — the general compute primitive (an agent, a shell command, a JS runtime are all just images); `uses` invokes a declarative integration action (e.g. slack.post-message) executed in-process by the daemon — no container. Both forms are full DAG citizens: needs, when, retry, timeout, and outputs behave identically. Conditional execution is expressed with `when` rather than a dedicated step type."
)
