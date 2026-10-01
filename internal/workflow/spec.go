// Package workflow defines the workflow specification, generates its JSON
// Schema and validates workflow definitions. The Go types in this file are
// the single source of truth: the published schema is derived from them.
package workflow

// Workflow is a named sequence of steps with optional triggers.
type Workflow struct {
	Name        string              `json:"name" jsonschema:"required,pattern=^[a-z0-9][a-z0-9-]{0\\,63}$" jsonschema_description:"Unique workflow name: lowercase letters, digits and dashes."`
	Description string              `json:"description,omitempty" jsonschema_description:"Free-text description shown in listings."`
	Inputs      map[string]InputDef `json:"inputs,omitempty" jsonschema_description:"Declared run inputs. Values arrive as {{ inputs.<name> }} and in /icaro/input.json."`
	On          *Triggers           `json:"on,omitempty" jsonschema_description:"Triggers that start runs. Runs can always be started manually via API/CLI/MCP."`
	Steps       []Step              `json:"steps" jsonschema:"required,minItems=1" jsonschema_description:"Steps executed in order. Each step is exactly one of: run, http."`
}

// InputDef declares a run input.
type InputDef struct {
	Type        string `json:"type,omitempty" jsonschema:"enum=string,enum=number,enum=boolean,enum=object,enum=array,default=string"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Default     any    `json:"default,omitempty"`
}

// Triggers groups all trigger kinds.
type Triggers struct {
	Webhook  *WebhookTrigger   `json:"webhook,omitempty" jsonschema_description:"Expose POST /hooks/{workflow}/{token}; the request body becomes the run input."`
	Schedule []ScheduleTrigger `json:"schedule,omitempty" jsonschema_description:"Cron schedules (5-field, UTC)."`
}

// WebhookTrigger configures the generic webhook endpoint.
type WebhookTrigger struct {
	Secret string `json:"secret,omitempty" jsonschema:"enum=optional,enum=required,default=optional" jsonschema_description:"required: reject requests without a valid X-Icaro-Signature HMAC."`
}

// ScheduleTrigger starts a run on a cron schedule.
type ScheduleTrigger struct {
	Cron  string         `json:"cron" jsonschema:"required" jsonschema_description:"5-field cron expression evaluated in UTC, e.g. \"*/5 * * * *\"."`
	Input map[string]any `json:"input,omitempty" jsonschema_description:"Input payload for scheduled runs."`
}

// Step is one unit of execution. Exactly one of the kind fields must be set.
type Step struct {
	Name            string            `json:"name" jsonschema:"required,pattern=^[a-zA-Z0-9][a-zA-Z0-9_-]{0\\,63}$" jsonschema_description:"Step name, unique within the workflow; referenced as steps.<name>."`
	If              string            `json:"if,omitempty" jsonschema_description:"Template that must render to true/false; false skips the step."`
	Run             *RunStep          `json:"run,omitempty" jsonschema_description:"Run a script or command in a container."`
	HTTP            *HTTPStep         `json:"http,omitempty" jsonschema_description:"Perform an HTTP request (runs in the engine, no container)."`
	Connection      string            `json:"connection,omitempty" jsonschema_description:"Named connection whose fields are injected as ICARO_CONN_<FIELD> (run) or used for auth (http)."`
	Env             map[string]string `json:"env,omitempty" jsonschema_description:"Extra environment variables (templated)."`
	Sandbox         string            `json:"sandbox,omitempty" jsonschema:"enum=standard,enum=strict,default=standard" jsonschema_description:"strict additionally disables network unless declared and uses the strict runtime when configured."`
	Network         string            `json:"network,omitempty" jsonschema:"enum=egress,enum=none,default=egress" jsonschema_description:"none: no network access for the container."`
	Timeout         string            `json:"timeout,omitempty" jsonschema_description:"Step timeout as a Go duration (e.g. 10m). Must not exceed the server default."`
	Retry           *Retry            `json:"retry,omitempty"`
	ContinueOnError bool              `json:"continue_on_error,omitempty" jsonschema_description:"Mark the step failed but continue the run."`
}

// Retry controls re-execution of a failed step.
type Retry struct {
	Attempts int    `json:"attempts" jsonschema:"required,minimum=1,maximum=10" jsonschema_description:"Total attempts including the first."`
	Backoff  string `json:"backoff,omitempty" jsonschema_description:"Delay between attempts as a Go duration (default 10s)."`
}

// RunStep executes a script or command inside a container.
type RunStep struct {
	Image   string            `json:"image" jsonschema:"required" jsonschema_description:"Container image reference. Pin by tag or digest."`
	Script  string            `json:"script,omitempty" jsonschema_description:"Script written to /icaro/script.sh and executed with shell. Mutually exclusive with command."`
	Command []string          `json:"command,omitempty" jsonschema_description:"Explicit argv. Mutually exclusive with script."`
	Shell   string            `json:"shell,omitempty" jsonschema_description:"Shell used to run script (default: sh -e)."`
	WorkDir string            `json:"workdir,omitempty" jsonschema_description:"Working directory (default /workspace)."`
	Env     map[string]string `json:"env,omitempty"`
	Limits  *Limits           `json:"limits,omitempty" jsonschema_description:"Resource ceilings; may only tighten the server defaults."`
}

// Limits are per-step resource ceilings.
type Limits struct {
	MemoryMB int64   `json:"memory_mb,omitempty" jsonschema:"minimum=16"`
	CPUs     float64 `json:"cpus,omitempty" jsonschema:"exclusiveMinimum=0"`
	Pids     int64   `json:"pids,omitempty" jsonschema:"minimum=1"`
}

// HTTPStep performs an HTTP request from the engine.
type HTTPStep struct {
	URL          string            `json:"url" jsonschema:"required"`
	Method       string            `json:"method,omitempty" jsonschema:"enum=GET,enum=POST,enum=PUT,enum=PATCH,enum=DELETE,enum=HEAD,default=GET"`
	Headers      map[string]string `json:"headers,omitempty"`
	Body         any               `json:"body,omitempty" jsonschema_description:"Request body: a string (sent as-is) or an object/array (sent as JSON)."`
	Auth         string            `json:"auth,omitempty" jsonschema:"enum=none,enum=bearer,enum=basic,default=none" jsonschema_description:"Authorization derived from the step connection."`
	ExpectStatus []int             `json:"expect_status,omitempty" jsonschema_description:"Accepted status codes (default: any 2xx)."`
}

// Kind names the step kind.
type Kind string

// Step kinds.
const (
	KindRun  Kind = "run"
	KindHTTP Kind = "http"
)

// Kind returns the step kind, or "" when none/several are set.
func (s *Step) Kind() Kind {
	var kinds []Kind
	if s.Run != nil {
		kinds = append(kinds, KindRun)
	}
	if s.HTTP != nil {
		kinds = append(kinds, KindHTTP)
	}
	if len(kinds) != 1 {
		return ""
	}
	return kinds[0]
}
