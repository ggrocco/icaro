// Package integration is the manifest hub (ADR 0007): declarative YAML
// manifests in git-backed packs, a webhook router per integration, and an
// in-daemon HTTP action executor for `uses:` steps. Manifests execute no
// code; everything they can do is listed in the permission summary.
package integration

// ManifestVersion is the manifest schema version this binary understands.
const ManifestVersion = 1

// Manifest is one integration.
type Manifest struct {
	Version     int                    `json:"version" yaml:"version"`
	Name        string                 `json:"name" yaml:"name"`
	Description string                 `json:"description,omitempty" yaml:"description,omitempty"`
	Hosts       []string               `json:"hosts,omitempty" yaml:"hosts,omitempty"`
	Config      map[string]ConfigField `json:"config,omitempty" yaml:"config,omitempty"`
	Auth        map[string]AuthRef     `json:"auth,omitempty" yaml:"auth,omitempty"`
	Triggers    *Triggers              `json:"triggers,omitempty" yaml:"triggers,omitempty"`
	Actions     map[string]Action      `json:"actions,omitempty" yaml:"actions,omitempty"`
}

// ConfigField is a setting with a default. Only `base_url` can be
// overridden by the user (config integrations.<name>.base_url).
type ConfigField struct {
	Default     string `json:"default,omitempty" yaml:"default,omitempty"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

// Auth kinds.
const (
	AuthBearer = "bearer"
	AuthHeader = "header"
	AuthBasic  = "basic"
	AuthQuery  = "query"
	AuthNone   = "none"
)

// AuthRef says how a stored credential is attached to a request. The
// credential is the secret `<integration>.<credential>`; it is attached
// only when the request host is inside the secret's host binding.
type AuthRef struct {
	Kind       string `json:"kind" yaml:"kind"`
	Credential string `json:"credential,omitempty" yaml:"credential,omitempty"`
	// Header names the header for kind=header (e.g. X-Api-Key) and may
	// carry a prefix (e.g. "token ") via Prefix.
	Header string `json:"header,omitempty" yaml:"header,omitempty"`
	Prefix string `json:"prefix,omitempty" yaml:"prefix,omitempty"`
	// Param names the query parameter for kind=query.
	Param string `json:"param,omitempty" yaml:"param,omitempty"`
	// Username is a template for kind=basic (the credential is the password);
	// empty means the credential holds "user:password".
	Username string `json:"username,omitempty" yaml:"username,omitempty"`
}

// Triggers is the inbound side of a manifest.
type Triggers struct {
	Verify     Verify           `json:"verify" yaml:"verify"`
	Timestamp  *Timestamp       `json:"timestamp,omitempty" yaml:"timestamp,omitempty"`
	DeliveryID string           `json:"delivery_id,omitempty" yaml:"delivery_id,omitempty"`
	Challenges []Challenge      `json:"challenges,omitempty" yaml:"challenges,omitempty"`
	Events     map[string]Event `json:"events" yaml:"events"`
}

// Verification kinds.
const (
	VerifyHMACSHA256   = "hmac-sha256"
	VerifyHMACSHA1     = "hmac-sha1"
	VerifySharedSecret = "shared-secret"
	VerifyURLToken     = "url-token"
)

// Verify declares how an inbound request is authenticated.
type Verify struct {
	Kind string `json:"kind" yaml:"kind"`
	// Header carries the signature or shared secret.
	Header string `json:"header,omitempty" yaml:"header,omitempty"`
	// Prefix is stripped from the header value (e.g. "sha256=").
	Prefix string `json:"prefix,omitempty" yaml:"prefix,omitempty"`
	// Encoding of the signature: hex (default) or base64.
	Encoding string `json:"encoding,omitempty" yaml:"encoding,omitempty"`
	// Message is the signed string as a template over headers/raw_body/query
	// (default "${{ raw_body }}").
	Message string `json:"message,omitempty" yaml:"message,omitempty"`
	// Credential names the secret `<integration>.<credential>` (default webhook_secret).
	Credential string `json:"credential,omitempty" yaml:"credential,omitempty"`
}

// Timestamp enables replay protection on a signed timestamp header.
type Timestamp struct {
	Header string `json:"header" yaml:"header"`
	// Format is unix (seconds, default), unix_ms or rfc3339.
	Format string `json:"format,omitempty" yaml:"format,omitempty"`
	// MaxSkew overrides webhooks.max_skew (Go duration).
	MaxSkew string `json:"max_skew,omitempty" yaml:"max_skew,omitempty"`
}

// Challenge answers a provider handshake (e.g. Slack url_verification)
// after verification, without starting any workflow.
type Challenge struct {
	Match       string `json:"match" yaml:"match"`
	Body        string `json:"body" yaml:"body"`
	ContentType string `json:"content_type,omitempty" yaml:"content_type,omitempty"`
}

// Event is one subscribable trigger event.
type Event struct {
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	// Match is a condition over headers/body/query selecting this event.
	Match string `json:"match" yaml:"match"`
	// Payload normalizes the raw request into ${{ trigger.* }}.
	Payload map[string]string `json:"payload" yaml:"payload"`
}

// Action is one outbound HTTP call usable as `uses: <integration>.<action>`.
type Action struct {
	Description  string            `json:"description,omitempty" yaml:"description,omitempty"`
	Inputs       map[string]Input  `json:"inputs,omitempty" yaml:"inputs,omitempty"`
	Request      Request           `json:"request" yaml:"request"`
	Outputs      map[string]string `json:"outputs,omitempty" yaml:"outputs,omitempty"`
	ExpectStatus []int             `json:"expect_status,omitempty" yaml:"expect_status,omitempty"`
	// Success is an optional condition over response that must also hold
	// (for APIs that answer 200 with {"ok": false}).
	Success string `json:"success,omitempty" yaml:"success,omitempty"`
}

// Input is a typed action input.
type Input struct {
	Type        string `json:"type" yaml:"type"`
	Required    bool   `json:"required,omitempty" yaml:"required,omitempty"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	Default     any    `json:"default,omitempty" yaml:"default,omitempty"`
}

// Request is an HTTP request template over input/config.
type Request struct {
	Method  string            `json:"method" yaml:"method"`
	URL     string            `json:"url" yaml:"url"`
	Headers map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	Query   map[string]string `json:"query,omitempty" yaml:"query,omitempty"`
	Body    *Body             `json:"body,omitempty" yaml:"body,omitempty"`
	Auth    string            `json:"auth,omitempty" yaml:"auth,omitempty"`
}

// Body is exactly one of JSON, Form or Raw.
type Body struct {
	JSON        any               `json:"json,omitempty" yaml:"json,omitempty"`
	Form        map[string]string `json:"form,omitempty" yaml:"form,omitempty"`
	Raw         string            `json:"raw,omitempty" yaml:"raw,omitempty"`
	ContentType string            `json:"content_type,omitempty" yaml:"content_type,omitempty"`
}

// Expression roots per manifest section (ADR 0009).
var (
	RootsTrigger = []string{"headers", "body", "query", "raw_body"}
	RootsRequest = []string{"input", "config"}
	RootsOutput  = []string{"input", "config", "response"}
)
