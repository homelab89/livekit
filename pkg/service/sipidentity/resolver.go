// Package sipidentity provides a pluggable mechanism for resolving
// SIP trunk phone numbers to custom participant identities (e.g. Matrix User IDs)
// during inbound SIP dispatch in livekit-server.
//
// This package is intentionally decoupled from config and protocol/rpc:
//   - It does NOT import pkg/config (avoids import cycles)
//   - It does NOT import livekit/protocol/rpc (easy to mock in tests)
//
// The resolution chain is:
//
//	StaticMappingResolver (YAML map, highest priority)
//	  ↓ miss
//	HTTPResolver (remote API lookup)
//	  ↓ miss / error / timeout
//	Caller falls back to livekit's native "sip_xxx" identity
//
// Usage in cmd/server/main.go (or equivalent startup code):
//
//	conf, _ := config.LoadConfig(...)
//	conf.ValidateSIPInbound()
//	buildSIPIdentityResolver(conf)
//
// Usage in pkg/service/sip.go (EvaluateSIPDispatchRules):
//
//	override := sipidentity.Resolve(ctx, &sipIdentityAdapter{resp: resp})
//	if override != "" {
//	    resp.ParticipantIdentity = override
//	}
package sipidentity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ----------------------------------------------------------------------------
// 1. Core interfaces
// ----------------------------------------------------------------------------

// Resolver resolves a SIP trunk phone number to a participant identity.
// The second return value (ok) indicates whether a valid identity was found.
// Implementations MUST NOT return an error: failures should always be
// signalled by ok=false so the caller can fail-open (keep native identity).
type Resolver interface {
	Resolve(ctx context.Context, trunkPhoneNumber string) (identity string, ok bool)
}

// ParticipantIdentitySource is the minimal interface needed by the Resolve
// helper. It abstracts away *rpc.EvaluateSIPDispatchRulesResponse so this
// package does not need to import livekit/protocol/rpc.
type ParticipantIdentitySource interface {
	GetTrunkPhoneNumber() string
}

// ----------------------------------------------------------------------------
// 2. Global registry (set once at startup)
// ----------------------------------------------------------------------------

// globalResolver is the active resolver chain. nil means "use native logic".
var globalResolver Resolver = nil

// SetGlobalResolver installs the active resolver. Pass nil to disable.
// This should be called exactly once during server startup, after config load.
func SetGlobalResolver(r Resolver) {
	globalResolver = r
}

// ResetGlobalResolver clears the resolver (primarily for tests).
func ResetGlobalResolver() {
	globalResolver = nil
}

// Resolve is the single entry point called from pkg/service/sip.go.
// It returns "" when no override should be applied, in which case the caller
// must keep the native ParticipantIdentity unchanged.
func Resolve(ctx context.Context, src ParticipantIdentitySource) string {
	if globalResolver == nil || src == nil {
		return ""
	}

	trunkPhone := src.GetTrunkPhoneNumber()
	if trunkPhone == "" {
		return ""
	}

	if id, ok := globalResolver.Resolve(ctx, trunkPhone); ok && id != "" {
		return id
	}
	return ""
}

// ----------------------------------------------------------------------------
// 3. Static mapping resolver
// ----------------------------------------------------------------------------

// StaticMappingResolver looks up an identity in a fixed map keyed by
// SIP trunk phone number. This is the highest-priority resolver.
type StaticMappingResolver struct {
	Mapping map[string]string
}

// NewStaticMappingResolver creates a static resolver. Pass nil or empty map
// to effectively disable it (Resolve will always return ok=false).
func NewStaticMappingResolver(mapping map[string]string) *StaticMappingResolver {
	if mapping == nil {
		mapping = make(map[string]string)
	}
	return &StaticMappingResolver{Mapping: mapping}
}

// Resolve returns the mapped identity for trunkPhoneNumber, or ok=false.
func (r *StaticMappingResolver) Resolve(_ context.Context, trunkPhoneNumber string) (string, bool) {
	id, ok := r.Mapping[trunkPhoneNumber]
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// ----------------------------------------------------------------------------
// 4. HTTP resolver (calls a remote service)
// ----------------------------------------------------------------------------

// HTTPResolverConfig holds the configuration for an HTTP-based resolver.
// It mirrors the YAML structure but uses native Go types.
type HTTPResolverConfig struct {
	// ResolverURL is the full URL of the resolver endpoint.
	// It will be called as GET <ResolverURL>?number=<trunkPhoneNumber>
	ResolverURL string

	// ResolverToken is sent in the X-Resolver-Token header.
	ResolverToken string

	// ResolverTimeout is the maximum time to wait for the resolver.
	// It is expressed in milliseconds and will be clamped to [1, 5000]
	// by ValidateAndClampTimeout (or the config layer equivalent).
	ResolverTimeoutMs int
}

// ValidateAndClampTimeout ensures the timeout is within sane bounds.
//   - <= 0  → default 2000 ms
//   - > 5000 → clamped to 5000 ms
//
// Returns the clamped value and whether it was changed.
func ValidateAndClampTimeout(ms int) (clamped int, wasClamped bool) {
	if ms <= 0 {
		return 2000, true
	}
	if ms > 5000 {
		return 5000, true
	}
	return ms, false
}

// HTTPResolver calls a remote service to resolve a trunk phone number
// to a participant identity. It is designed to fail-open: any error,
// timeout, non-200 status, or empty body results in ok=false.
type HTTPResolver struct {
	url     string
	token   string
	timeout time.Duration
	client  *http.Client
}

// NewHTTPResolver constructs an HTTP resolver from config.
// timeoutMs is in milliseconds and will be clamped by ValidateAndClampTimeout.
func NewHTTPResolver(cfg HTTPResolverConfig) *HTTPResolver {
	ms, _ := ValidateAndClampTimeout(cfg.ResolverTimeoutMs)
	return &HTTPResolver{
		url:     cfg.ResolverURL,
		token:   cfg.ResolverToken,
		timeout: time.Duration(ms) * time.Millisecond,
		client:  &http.Client{Timeout: time.Duration(ms) * time.Millisecond},
	}
}

// resolveResponse is the expected JSON body from the resolver service.
// Example success: {"matrix_user_id": "@sip-201:matrix.test.com:abc"}
// Example not-found: {"matrix_user_id": ""}
type resolveResponse struct {
	MatrixUserID string `json:"matrix_user_id"`
}

// Resolve performs the HTTP call. It never returns an error: failures are
// converted to ok=false so the caller can fall back to native identity.
//
// Request:
//
//	GET <url>?number=<trunkPhoneNumber>
//	Header: X-Resolver-Token: <token>
//	Header: Accept: application/json
//
// Success response (200 OK):
//
//	{"matrix_user_id": "@sip-201:matrix.test.com:abc"}
//
// Any of the following → ok=false (logged by caller):
//   - context deadline exceeded (timeout)
//   - non-200 HTTP status
//   - invalid JSON
//   - empty matrix_user_id
func (r *HTTPResolver) Resolve(ctx context.Context, trunkPhoneNumber string) (string, bool) {
	if r.url == "" || trunkPhoneNumber == "" {
		return "", false
	}

	// Honor the caller's context but enforce our own timeout cap.
	callCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	// Build URL with query parameter. url.QueryEscape prevents injection
	// of malicious characters into the query string.
	target := r.url
	if q := url.QueryEscape(trunkPhoneNumber); q != "" {
		sep := "?"
		for i := 0; i < len(r.url); i++ {
			if r.url[i] == '?' {
				sep = "&"
				break
			}
		}
		target = r.url + sep + "number=" + q
	}

	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, target, nil)
	if err != nil {
		// Should never happen for a well-formed URL, but fail-open regardless.
		return "", false
	}

	req.Header.Set("X-Resolver-Token", r.token)
	req.Header.Set("Accept", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		// Network error or timeout (context deadline exceeded).
		return "", false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", false
	}

	// Limit body size to prevent abuse (e.g. resolver returning GB of data).
	body := io.LimitReader(resp.Body, 64*1024)

	var out resolveResponse
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		return "", false
	}

	if out.MatrixUserID == "" {
		return "", false
	}

	return out.MatrixUserID, true
}

// ----------------------------------------------------------------------------
// 5. Chain resolver (static first, then HTTP)
// ----------------------------------------------------------------------------

// ChainResolver tries a sequence of resolvers in order, returning the first
// successful result. This is the recommended resolver for production: it
// gives you instant local lookup with a remote fallback.
type ChainResolver struct {
	Static *StaticMappingResolver
	HTTP   *HTTPResolver
}

// NewChainResolver builds a chain. Either Static or HTTP may be nil,
// in which case that step is skipped.
func NewChainResolver(static *StaticMappingResolver, httpResolver *HTTPResolver) *ChainResolver {
	return &ChainResolver{Static: static, HTTP: httpResolver}
}

// Resolve walks the chain in priority order:
//  1. StaticMappingResolver (if configured)
//  2. HTTPResolver         (if configured)
//
// Returns ok=false if no resolver is configured or all missed.
func (c *ChainResolver) Resolve(ctx context.Context, trunkPhoneNumber string) (string, bool) {
	if c == nil {
		return "", false
	}
	if c.Static != nil {
		if id, ok := c.Static.Resolve(ctx, trunkPhoneNumber); ok {
			return id, true
		}
	}
	if c.HTTP != nil {
		return c.HTTP.Resolve(ctx, trunkPhoneNumber)
	}
	return "", false
}

// ----------------------------------------------------------------------------
// 6. Builder helper (used by cmd/server/main.go)
// ----------------------------------------------------------------------------

// BuildConfig mirrors the relevant subset of *config.Config needed to build
// a resolver. Defining it here (rather than importing config) keeps the
// package dependency-free. The startup code in main.go is responsible for
// translating config.SIP into this struct.
type BuildConfig struct {
	// IdentityMapping is the static YAML map: trunkPhoneNumber → identity.
	IdentityMapping map[string]string

	// HTTPURL / HTTPToken / HTTPTimeoutMs configure the remote resolver.
	// A zero/empty HTTPURL disables the HTTP resolver.
	HTTPURL       string
	HTTPToken     string
	HTTPTimeoutMs int
}

// BuildResolver constructs the appropriate resolver from a BuildConfig.
//   - If both mapping and HTTP are empty/nil, returns nil (no override).
//   - If only mapping is set, returns a StaticMappingResolver.
//   - If only HTTP is set, returns an HTTPResolver.
//   - If both are set, returns a ChainResolver (static first).
func BuildResolver(cfg BuildConfig) Resolver {
	hasMapping := len(cfg.IdentityMapping) > 0
	hasHTTP := cfg.HTTPURL != ""

	if !hasMapping && !hasHTTP {
		return nil
	}

	var static *StaticMappingResolver
	if hasMapping {
		static = NewStaticMappingResolver(cfg.IdentityMapping)
	}

	var httpResolver *HTTPResolver
	if hasHTTP {
		httpResolver = NewHTTPResolver(HTTPResolverConfig{
			ResolverURL:       cfg.HTTPURL,
			ResolverToken:     cfg.HTTPToken,
			ResolverTimeoutMs: cfg.HTTPTimeoutMs,
		})
	}

	if hasMapping && hasHTTP {
		return NewChainResolver(static, httpResolver)
	}
	if hasMapping {
		return static
	}
	return httpResolver
}

// ----------------------------------------------------------------------------
// 7. Adapter helper (used by pkg/service/sip.go)
// ----------------------------------------------------------------------------

// ResponseAdapter adapts *rpc.EvaluateSIPDispatchRulesResponse to
// ParticipantIdentitySource without importing livekit/protocol/rpc.
//
// Usage in sip.go:
//
//	type sipIdentityAdapter struct {
//	    resp *rpc.EvaluateSIPDispatchRulesResponse
//	}
//
//	func (a *sipIdentityAdapter) GetTrunkPhoneNumber() string {
//	    if a.resp == nil {
//	        return ""
//	    }
//	    return a.resp.ParticipantAttributes[livekit.AttrSIPTrunkNumber]
//	}
//
//	override := sipidentity.Resolve(ctx, &sipIdentityAdapter{resp: resp})
//
// (The adapter struct itself lives in sip.go because it references the
// rpc type. Only the interface ParticipantIdentitySource is defined here.)
type ResponseAdapter = ParticipantIdentitySource

// ----------------------------------------------------------------------------
// 8. Constants for logging / telemetry
// ----------------------------------------------------------------------------

// LogTag is the consistent log prefix used by callers when logging resolver
// events. Centralising it avoids typos and makes filtering logs easy.
const LogTag = "sip-identity-resolver"

// ResolutionSource identifies which resolver produced a result.
type ResolutionSource string

const (
	// SourceStatic indicates the identity came from the YAML mapping.
	SourceStatic ResolutionSource = "static_mapping"

	// SourceHTTP indicates the identity came from the remote resolver API.
	SourceHTTP ResolutionSource = "http_resolver"

	// SourceNative indicates no override was applied; native "sip_xxx" is kept.
	SourceNative ResolutionSource = "native"
)

// ResolutionResult is a structured outcome for logging/metrics.
type ResolutionResult struct {
	Source     ResolutionSource
	TrunkPhone string
	Identity   string // empty when Source == SourceNative
	DurationMs int64
	Error      error // non-fatal; for logging only
}

// String formats a ResolutionResult for logs.
func (r ResolutionResult) String() string {
	switch r.Source {
	case SourceNative:
		return fmt.Sprintf("[%s] trunk=%s → native (no override)", LogTag, r.TrunkPhone)
	case SourceStatic, SourceHTTP:
		return fmt.Sprintf("[%s] trunk=%s → %s (%s, %dms)",
			LogTag, r.TrunkPhone, r.Identity, r.Source, r.DurationMs)
	default:
		return fmt.Sprintf("[%s] trunk=%s → unknown source", LogTag, r.TrunkPhone)
	}
}
