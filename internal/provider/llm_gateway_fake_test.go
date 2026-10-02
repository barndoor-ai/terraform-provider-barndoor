// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// --- in-process fake llm-gateway-service --------------------------------------
//
// fakeLlmGatewayServer emulates the llm-gateway admin REST surface the
// provider binds (`/api/llm-gateway/admin/connections|providers|model-mappings|
// model-access|model-route-groups|rate-limits|budgets|model-pricing|
// governance-config|routing-policies|routing-rules`)
// faithfully enough to drive real plan/apply cycles: write-only connection
// secrets (stored, never echoed), providers bound to connections with inline
// keys rejected (BCP-3647), per-model-provider auth_type
// defaulting, settings-must-be-object validation, the model-mapping
// orphan-alias guard and 1:1 PATCH-or-create upsert with timeout
// materialization, listing-only reads (no get-by-id) for mappings/policies/
// budgets, the rate-limit tri-state metric PATCH, the scope-uniqueness 409s
// of rate limits and budgets, the append-only versioned pricing store
// (scheduled changes, skip-on-no-op, archive tombstones, per-instant
// uniqueness), route groups with the membership sweeps that mapping renames
// and deletes trigger, and the governance-config singleton upsert. Errors use the
// service's OpenAI envelope (`{"error": {"message": ...}}`).

// fakeLlmOrgID matches the BARNDOOR_ORGANIZATION_ID set by setupLlmGatewayTest.
const fakeLlmOrgID = "11111111-2222-3333-4444-555555555555"

const fakeLlmTime = "2026-07-02T00:00:00Z"

// Platform timeout defaults materialized at mapping insert
// (`STREAM_IDLE_TIMEOUT_DEFAULT_SECS` / `TOTAL_REQUEST_TIMEOUT_DEFAULT_SECS`).
const (
	fakeLlmStreamIdleDefault = 180
	fakeLlmRequestDefault    = 120
)

type fakeLlmProvider struct {
	ID                 string
	Name               string
	ModelProvider      string
	AuthType           string
	BaseURL            string
	Settings           json.RawMessage
	Enabled            bool
	EnforceHealthCheck bool
	BillingMode        string
	BillingReason      *string
	BillingNote        *string
	ConnectionID       *string
	CatalogID          *string
	ModelSyncMode      string
	RequestTimeout     *int64
	StreamIdleTimeout  *int64
	LastChangeNote     *string
}

// fakeLlmCatalogID is the one provider_catalog entry the fake knows, and
// fakeLlmCatalogBaseURL its default endpoint.
const (
	fakeLlmCatalogID      = "c0c0c0c0-0000-0000-0000-000000000001"
	fakeLlmCatalogBaseURL = "https://catalog-default.example.com"
)

// validLlmProviderSync mirrors the handler's catalog-sync and
// validate_provider_timeouts checks.
func validLlmProviderSync(w http.ResponseWriter, p *fakeLlmProvider) bool {
	if p.ModelSyncMode != "off" && p.CatalogID == nil {
		writeLlmError(w, http.StatusBadRequest,
			"catalog model sync requires a provider created from the catalog (catalog_id)")
		return false
	}
	if p.RequestTimeout != nil && (*p.RequestTimeout < 1 || *p.RequestTimeout > 600) {
		writeLlmError(w, http.StatusBadRequest, "request_timeout_secs must be between 1 and 600")
		return false
	}
	if p.StreamIdleTimeout != nil && (*p.StreamIdleTimeout < 1 || *p.StreamIdleTimeout > 300) {
		writeLlmError(w, http.StatusBadRequest, "stream_idle_timeout_secs must be between 1 and 300")
		return false
	}
	return true
}

type fakeLlmModelMapping struct {
	ID                    string
	ProviderID            string
	ModelAlias            string
	UpstreamModel         string
	Enabled               bool
	Priority              int64
	RetryOn429Count       int64
	RetryOn429MaxWaitSecs int64
	BareAlias             bool
	StreamIdleTimeoutSecs int64
	RequestTimeoutSecs    int64
	Cooldown              fakeLlmCooldown
	LastChangeNote        *string
}

// fakeLlmCooldown is a route's passive cooldown policy (BCP-2671 H5): six NOT
// NULL columns with defaults, rendered flat on the mapping.
type fakeLlmCooldown struct {
	FailureThreshold int64 `json:"cooldown_failure_threshold"`
	WindowSecs       int64 `json:"cooldown_window_secs"`
	BaseSecs         int64 `json:"cooldown_base_secs"`
	MaxSecs          int64 `json:"cooldown_max_secs"`
	Default429Secs   int64 `json:"cooldown_429_default_secs"`
	OverloadedSecs   int64 `json:"cooldown_overloaded_secs"`
}

// fakeLlmCooldownDefaults are the V-migration column defaults.
var fakeLlmCooldownDefaults = fakeLlmCooldown{
	FailureThreshold: 10, WindowSecs: 60, BaseSecs: 30, MaxSecs: 300, Default429Secs: 30, OverloadedSecs: 10,
}

// fakeLlmCooldownPatch carries the request's cooldown keys; nil keeps.
type fakeLlmCooldownPatch struct {
	FailureThreshold *int64 `json:"cooldown_failure_threshold"`
	WindowSecs       *int64 `json:"cooldown_window_secs"`
	BaseSecs         *int64 `json:"cooldown_base_secs"`
	MaxSecs          *int64 `json:"cooldown_max_secs"`
	Default429Secs   *int64 `json:"cooldown_429_default_secs"`
	OverloadedSecs   *int64 `json:"cooldown_overloaded_secs"`
}

func (c fakeLlmCooldown) apply(p fakeLlmCooldownPatch) fakeLlmCooldown {
	for _, f := range []struct {
		dst *int64
		src *int64
	}{
		{&c.FailureThreshold, p.FailureThreshold}, {&c.WindowSecs, p.WindowSecs},
		{&c.BaseSecs, p.BaseSecs}, {&c.MaxSecs, p.MaxSecs},
		{&c.Default429Secs, p.Default429Secs}, {&c.OverloadedSecs, p.OverloadedSecs},
	} {
		if f.src != nil {
			*f.dst = *f.src
		}
	}
	return c
}

// validLlmCooldown mirrors CooldownPolicy::validate: per-field ranges, then
// every window capped by cooldown_max_secs (0 exempts the 529 cooldown).
func validLlmCooldown(w http.ResponseWriter, c fakeLlmCooldown) bool {
	for _, r := range []struct {
		name      string
		v, lo, hi int64
	}{
		{"cooldown_failure_threshold", c.FailureThreshold, 0, 100},
		{"cooldown_window_secs", c.WindowSecs, 1, 3600},
		{"cooldown_base_secs", c.BaseSecs, 1, 3600},
		{"cooldown_max_secs", c.MaxSecs, 1, 86400},
		{"cooldown_429_default_secs", c.Default429Secs, 1, 3600},
		{"cooldown_overloaded_secs", c.OverloadedSecs, 0, 3600},
	} {
		if r.v < r.lo || r.v > r.hi {
			writeLlmError(w, http.StatusBadRequest, fmt.Sprintf("%s must be between %d and %d", r.name, r.lo, r.hi))
			return false
		}
	}
	if c.BaseSecs > c.MaxSecs || c.Default429Secs > c.MaxSecs || (c.OverloadedSecs != 0 && c.OverloadedSecs > c.MaxSecs) {
		writeLlmError(w, http.StatusBadRequest,
			"cooldown_base_secs, cooldown_429_default_secs and cooldown_overloaded_secs must be at most cooldown_max_secs")
		return false
	}
	return true
}

type fakeLlmModelAccessPolicy struct {
	ID             string
	Name           string
	ScopeType      string
	ScopeID        *string
	ScopeValue     *string
	PolicyType     string
	Targets        []json.RawMessage
	TrafficType    string
	Enabled        bool
	LastChangeNote *string
}

type fakeLlmRateLimit struct {
	ID                string
	Name              string
	ScopeType         string
	ScopeID           *string
	ScopeValue        *string
	RequestsPerMinute *int64
	TokensPerMinute   *int64
	MemberOfGroup     *string
	TrafficType       string
	Enabled           bool
	Targets           fakeLlmTargets
	LastChangeNote    *string
}

// fakeLlmTargets are the governance target columns (BCP-2813, BCP-4053),
// shared by budgets and rate limits.
type fakeLlmTargets struct {
	ProviderID    *string `json:"target_provider_id"`
	UpstreamModel *string `json:"target_upstream_model"`
	ModelAlias    *string `json:"target_model_alias"`
	McpServerID   *string `json:"target_mcp_server_id"`
}

func (t fakeLlmTargets) eq(o fakeLlmTargets) bool {
	return strPtrEq(t.ProviderID, o.ProviderID) && strPtrEq(t.UpstreamModel, o.UpstreamModel) &&
		strPtrEq(t.ModelAlias, o.ModelAlias) && strPtrEq(t.McpServerID, o.McpServerID)
}

// validLlmTargetShape mirrors governance_shape::validate_target_shape, in
// production's order.
func validLlmTargetShape(w http.ResponseWriter, t fakeLlmTargets, trafficType string) bool {
	llm := t.ProviderID != nil || t.UpstreamModel != nil || t.ModelAlias != nil
	var msg string
	switch {
	case t.ModelAlias != nil && (t.ProviderID != nil || t.UpstreamModel != nil):
		msg = "target_model_alias cannot be combined with target_provider_id or target_upstream_model"
	case t.UpstreamModel != nil && t.ProviderID == nil:
		msg = "target_upstream_model requires target_provider_id"
	case llm && trafficType == "mcp":
		msg = "an LLM target cannot be combined with traffic_type mcp"
	case t.McpServerID != nil && llm:
		msg = "target_mcp_server_id cannot be combined with an LLM target"
	case t.McpServerID != nil && trafficType == "llm":
		msg = "target_mcp_server_id requires traffic_type mcp or all"
	default:
		return true
	}
	writeLlmError(w, http.StatusBadRequest, msg)
	return false
}

// fakeLlmChangeNote mirrors normalize_change_note: trim, blank means none,
// at most 500 characters. It writes the 400 and returns false when too long.
func fakeLlmChangeNote(w http.ResponseWriter, raw *string) (*string, bool) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, true
	}
	note := strings.TrimSpace(*raw)
	if len([]rune(note)) > 500 {
		writeLlmError(w, http.StatusBadRequest, "change_note must be at most 500 characters")
		return nil, false
	}
	return &note, true
}

type fakeLlmTokenBudget struct {
	ID              string
	Name            string
	ScopeType       string
	ScopeID         *string
	ScopeValue      *string
	MemberOfGroup   *string
	Period          string
	TokenLimit      int64
	CostLimit       *float64
	Currency        string
	AlertThresholds []int64
	ActionOnExhaust string
	TrafficType     string
	Enabled         bool
	Targets         fakeLlmTargets
	LastChangeNote  *string
}

// fakeLlmDecimal4 rounds like the DECIMAL(12, 4) cost_limit column.
func fakeLlmDecimal4(v float64) float64 {
	return math.Round(v*10000) / 10000
}

// fakeLlmPricingVersion is one row of the append-only, versioned
// llm_gw.model_pricing store (V34/V35): a logical rule is the group of rows
// sharing (model_pattern, COALESCE(model_provider, ”)), the latest
// effective_from <= now() row is the billed one, future rows are scheduled
// changes, and archive appends an is_archived tombstone.
type fakeLlmPricingVersion struct {
	ID               string
	ModelProvider    *string
	ModelPattern     string
	InputCost        float64
	OutputCost       float64
	CacheRead        *float64
	CacheWrite       *float64
	LongContext      *fakeLlmLongContext
	EffectiveFrom    time.Time
	EffectiveFromRaw string // exact wire echo, like chrono round-tripping the input
	ProviderID       *string
	CatalogSlug      *string
	SyncMode         string
	ChangeSource     string
	ChangeReason     *string
	IsArchived       bool
}

type fakeLlmGatewayServer struct {
	mu sync.Mutex

	nextID int

	// providers etc. keep insertion order for stable listings.
	providers   []*fakeLlmProvider
	connections map[string]*fakeLlmConnection
	mappings    []*fakeLlmModelMapping
	policies    []*fakeLlmModelAccessPolicy
	rateLimits  []*fakeLlmRateLimit
	budgets     []*fakeLlmTokenBudget
	pricing     []*fakeLlmPricingVersion
	routeGroups []*fakeLlmRouteGroup

	// routingPolicies and routingRules back the routing surface; see
	// llm_gateway_fake_routing_test.go.
	routingPolicies []*fakeLlmRoutingPolicy
	routingRules    []*fakeLlmRoutingRule

	// governance is the org's singleton governance_config row; nil means no
	// row yet (the API then reports the column defaults).
	governance *bool
	// defaultModelAccess ("" = allow) and requireRoutingPolicy are the later
	// governance_config columns (V70, V78).
	defaultModelAccess   string
	requireRoutingPolicy bool

	// forbidden simulates a credential that fails the Cerbos authorize()
	// check every admin handler runs; when set, the governance-config and
	// model-pricing families answer 403.
	forbidden bool
}

func newFakeLlmGatewayServer() *fakeLlmGatewayServer {
	return &fakeLlmGatewayServer{connections: map[string]*fakeLlmConnection{}}
}

// newID mints a deterministic UUID-shaped id. Callers hold f.mu.
func (f *fakeLlmGatewayServer) newID(prefix string) string {
	f.nextID++
	return fmt.Sprintf("%s0000-0000-0000-0000-%012d", prefix, f.nextID)
}

func (f *fakeLlmGatewayServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			writeToken(w)
		case strings.HasPrefix(r.URL.Path, "/api/llm-gateway/admin/providers"):
			f.handleProviders(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/llm-gateway/admin/connections"):
			f.handleConnections(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/llm-gateway/admin/model-mappings"):
			f.handleModelMappings(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/llm-gateway/admin/model-route-groups"):
			f.handleRouteGroups(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/llm-gateway/admin/model-access"):
			f.handleModelAccess(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/llm-gateway/admin/rate-limits"):
			f.handleRateLimits(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/llm-gateway/admin/budgets"):
			f.handleBudgets(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/llm-gateway/admin/model-pricing"):
			f.handleModelPricing(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/llm-gateway/admin/routing-policies"):
			f.handleRoutingPolicies(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/llm-gateway/admin/routing-rules"):
			f.handleRoutingRules(w, r)
		case r.URL.Path == "/api/llm-gateway/admin/governance-config":
			f.handleGovernanceConfig(w, r)
		default:
			http.NotFound(w, r)
		}
	}
}

// setupLlmGatewayTest starts the fake llm-gateway-service and points the
// provider's BARNDOOR_* environment at it. REST traffic and token minting
// ride the same httptest server, exactly like production shares one platform
// host.
func setupLlmGatewayTest(t *testing.T) *fakeLlmGatewayServer {
	t.Helper()

	fake := newFakeLlmGatewayServer()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	t.Setenv("BARNDOOR_BASE_URL", srv.URL)
	t.Setenv("BARNDOOR_TOKEN_URL", srv.URL+"/token")
	t.Setenv("BARNDOOR_CLIENT_ID", "test-client")
	t.Setenv("BARNDOOR_CLIENT_SECRET", "test-secret")
	t.Setenv("BARNDOOR_ORGANIZATION_ID", fakeLlmOrgID)

	return fake
}

// writeLlmError renders the llm-gateway error envelope
// (`{"error": {"message", "type"}}` — the OpenAI shape).
func writeLlmError(w http.ResponseWriter, status int, message string) {
	errType := "invalid_request_error"
	switch status {
	case http.StatusNotFound:
		errType = "not_found_error"
	case http.StatusConflict:
		errType = "conflict_error"
	case http.StatusUnauthorized:
		errType = "authentication_error"
	case http.StatusForbidden:
		errType = "permission_error"
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": message, "type": errType},
	})
}

// --- providers -----------------------------------------------------------------

var fakeLlmModelProviders = []string{
	"openai", "anthropic", "azure_openai", "azure_foundry", "google_ai", "bedrock",
	"vertex", "groq", "together", "mistral", "cohere", "xai", "fireworks",
	"perplexity", "openrouter", "deepseek", "typesafe", "custom",
}

// fakeLlmDefaultAuthType mirrors production's default_auth_type.
func fakeLlmDefaultAuthType(modelProvider string, requested *string) string {
	if requested != nil && *requested != "" {
		return *requested
	}
	switch modelProvider {
	case "bedrock":
		return "aws_role"
	case "vertex":
		return "google_adc"
	case "azure_openai":
		return "azure_api_key"
	case "azure_foundry":
		return "azure_foundry_api_key"
	case "anthropic":
		return "x_api_key"
	default:
		return "bearer_api_key"
	}
}

// validLlmSettings mirrors production's normalize_settings leading check:
// settings, when present, must be a JSON object.
func validLlmSettings(w http.ResponseWriter, raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage("{}"), true
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		writeLlmError(w, http.StatusBadRequest, "settings must be a JSON object")
		return nil, false
	}
	return raw, true
}

func (f *fakeLlmGatewayServer) findProvider(id string) *fakeLlmProvider {
	for _, p := range f.providers {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func providerJSON(p *fakeLlmProvider) map[string]any {
	out := map[string]any{
		"id":             p.ID,
		"org_id":         fakeLlmOrgID,
		"catalog_id":     p.CatalogID,
		"connection_id":  p.ConnectionID,
		"name":           p.Name,
		"model_provider": p.ModelProvider,
		"auth_type":      p.AuthType,
		"base_url":       p.BaseURL,
		// The secret lives on the connection; only its path is echoed.
		"secret_path":              "pending",
		"enabled":                  p.Enabled,
		"settings":                 p.Settings,
		"created_at":               fakeLlmTime,
		"updated_at":               fakeLlmTime,
		"health_status":            "unverified",
		"enforce_health_check":     p.EnforceHealthCheck,
		"billing_mode":             p.BillingMode,
		"model_sync_mode":          p.ModelSyncMode,
		"request_timeout_secs":     p.RequestTimeout,
		"stream_idle_timeout_secs": p.StreamIdleTimeout,
		"last_change_note":         p.LastChangeNote,
	}
	// Omitted when null, like production's skip_serializing_if.
	if p.BillingReason != nil {
		out["billing_reason"] = *p.BillingReason
	}
	if p.BillingNote != nil {
		out["billing_note"] = *p.BillingNote
	}
	return out
}

// fakeLlmBillingNote mirrors normalize_billing_note: trimmed, blank is absent.
func fakeLlmBillingNote(note *string) *string {
	if note == nil || strings.TrimSpace(*note) == "" {
		return nil
	}
	trimmed := strings.TrimSpace(*note)
	return &trimmed
}

// validLlmBilling mirrors validate_billing_fields on the resulting row, plus
// the enum checks serde performs on the way in.
func validLlmBilling(w http.ResponseWriter, p *fakeLlmProvider) bool {
	if !slices.Contains([]string{"per_token", "not_metered"}, p.BillingMode) {
		writeLlmError(w, http.StatusBadRequest, "unknown billing_mode: "+p.BillingMode)
		return false
	}
	if p.BillingReason != nil &&
		!slices.Contains([]string{"subscription", "local", "external", "other"}, *p.BillingReason) {
		writeLlmError(w, http.StatusBadRequest, "unknown billing_reason: "+*p.BillingReason)
		return false
	}
	if p.BillingMode == "not_metered" && p.BillingReason == nil {
		writeLlmError(w, http.StatusBadRequest,
			"billing_reason is required when billing_mode is 'not_metered'; one of "+
				"'subscription', 'local', 'external', 'other'.")
		return false
	}
	if p.BillingNote != nil && len([]rune(*p.BillingNote)) > 200 {
		writeLlmError(w, http.StatusBadRequest, "billing_note must be at most 200 characters")
		return false
	}
	return true
}

func (f *fakeLlmGatewayServer) handleProviders(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/llm-gateway/admin/providers"), "/")

	switch {
	case id == "" && r.Method == http.MethodPost:
		f.createProvider(w, r)
	case id == "" && r.Method == http.MethodGet:
		items := make([]map[string]any, 0, len(f.providers))
		for _, p := range f.providers {
			items = append(items, providerJSON(p))
		}
		_ = json.NewEncoder(w).Encode(items)
	case id != "" && r.Method == http.MethodGet:
		p := f.findProvider(id)
		if p == nil {
			writeLlmError(w, http.StatusNotFound, fmt.Sprintf("provider {id: %s} not found", id))
			return
		}
		_ = json.NewEncoder(w).Encode(providerJSON(p))
	case id != "" && r.Method == http.MethodPut:
		f.updateProvider(w, r, id)
	case id != "" && r.Method == http.MethodDelete:
		for i, p := range f.providers {
			if p.ID == id {
				f.providers = append(f.providers[:i], f.providers[i+1:]...)
				_ = json.NewEncoder(w).Encode(map[string]any{"deleted": true})
				return
			}
		}
		writeLlmError(w, http.StatusNotFound, fmt.Sprintf("provider {id: %s} not found", id))
	default:
		http.NotFound(w, r)
	}
}

// fakeLlmRequestScopedAuth are the auth types that store no upstream secret
// (production's REQUEST_SCOPED_AUTH_TYPES): the only ones a provider may use
// without a connection.
var fakeLlmRequestScopedAuth = []string{"claude_oauth", "codex_oauth"}

// fakeLlmVersionedBaseFamilies mirrors forwards_gateway_version_prefix.
var fakeLlmVersionedBaseFamilies = []string{
	"openai", "anthropic", "groq", "together", "mistral", "cohere", "xai",
	"fireworks", "perplexity", "openrouter", "deepseek", "typesafe", "custom",
}

// validLlmBaseURL mirrors validate_base_url's /v1 rule, applied only to a
// base_url the request supplied.
func validLlmBaseURL(w http.ResponseWriter, baseURL, modelProvider, authType string) bool {
	if !slices.Contains(fakeLlmVersionedBaseFamilies, modelProvider) ||
		(modelProvider == "openai" && authType == "codex_oauth") {
		return true
	}
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if corrected, found := strings.CutSuffix(trimmed, "/v1"); found && corrected != "" {
		writeLlmError(w, http.StatusBadRequest, fmt.Sprintf(
			"base_url must not end in '/v1': the gateway appends the API version itself. Use '%s' instead", corrected))
		return false
	}
	return true
}

// fakeLlmMergeSettings mirrors merge_connection_settings: the connection's
// settings are the base, the provider's overlay them.
func fakeLlmMergeSettings(connection, provider json.RawMessage) json.RawMessage {
	merged := map[string]json.RawMessage{}
	_ = json.Unmarshal(connection, &merged)
	overlay := map[string]json.RawMessage{}
	_ = json.Unmarshal(provider, &overlay)
	for k, v := range overlay {
		merged[k] = v
	}
	out, _ := json.Marshal(merged)
	return out
}

func (f *fakeLlmGatewayServer) createProvider(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name               string          `json:"name"`
		ModelProvider      string          `json:"model_provider"`
		AuthType           *string         `json:"auth_type"`
		BaseURL            string          `json:"base_url"`
		ConnectionID       *string         `json:"connection_id"`
		APIKey             *string         `json:"api_key"`
		Credentials        json.RawMessage `json:"credentials"`
		Settings           json.RawMessage `json:"settings"`
		EnforceHealthCheck *bool           `json:"enforce_health_check"`
		BillingMode        *string         `json:"billing_mode"`
		BillingReason      *string         `json:"billing_reason"`
		BillingNote        *string         `json:"billing_note"`
		CatalogID          *string         `json:"catalog_id"`
		ModelSyncMode      *string         `json:"model_sync_mode"`
		RequestTimeout     *int64          `json:"request_timeout_secs"`
		StreamIdleTimeout  *int64          `json:"stream_idle_timeout_secs"`
		ChangeNote         *string         `json:"change_note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeLlmError(w, http.StatusBadRequest, err.Error())
		return
	}
	note, ok := fakeLlmChangeNote(w, body.ChangeNote)
	if !ok {
		return
	}
	// catalog_id has an FK to provider_catalog and no handler check, so an
	// unknown id fails the insert as a 500.
	if body.CatalogID != nil && *body.CatalogID != fakeLlmCatalogID {
		writeLlmError(w, http.StatusInternalServerError, `insert or update on table "providers" violates `+
			`foreign key constraint "providers_catalog_id_fkey"`)
		return
	}
	if !slices.Contains(fakeLlmModelProviders, body.ModelProvider) {
		writeLlmError(w, http.StatusBadRequest, "unknown model provider: "+body.ModelProvider)
		return
	}
	settings, ok := validLlmSettings(w, body.Settings)
	if !ok {
		return
	}

	// BCP-3647: a provider's secret lives on a connection; only the
	// request-scoped OAuth passthroughs may arrive without one.
	authType := fakeLlmDefaultAuthType(body.ModelProvider, body.AuthType)
	storesSecret := !slices.Contains(fakeLlmRequestScopedAuth, authType)
	if storesSecret && (body.APIKey != nil || len(body.Credentials) > 0) {
		writeLlmError(w, http.StatusBadRequest, "a provider cannot store its own key: create a credential "+
			"(POST /admin/connections) and reference it with connection_id")
		return
	}
	if body.ConnectionID == nil && storesSecret {
		writeLlmError(w, http.StatusBadRequest,
			"connection_id is required: create a credential first, then reference it")
		return
	}
	baseURL := body.BaseURL
	if body.ConnectionID != nil {
		conn := f.connections[*body.ConnectionID]
		if conn == nil {
			writeLlmError(w, http.StatusNotFound, fmt.Sprintf("connection {id: %s} not found", *body.ConnectionID))
			return
		}
		// The connection is the source of truth for auth_type and the
		// resource settings once bound.
		authType = conn.AuthType
		settings = fakeLlmMergeSettings(conn.Settings, settings)
		if strings.TrimSpace(baseURL) == "" {
			baseURL = conn.BaseURL
		}
	}
	if strings.TrimSpace(baseURL) == "" && body.CatalogID != nil {
		baseURL = fakeLlmCatalogBaseURL
	}
	if body.BaseURL != "" && !validLlmBaseURL(w, body.BaseURL, body.ModelProvider, authType) {
		return
	}

	enforce := true
	if body.EnforceHealthCheck != nil {
		enforce = *body.EnforceHealthCheck
	}

	billingMode := "per_token" // never inferred server-side
	if body.BillingMode != nil {
		billingMode = *body.BillingMode
	}
	syncMode := "off"
	if body.ModelSyncMode != nil {
		syncMode = *body.ModelSyncMode
	}

	p := &fakeLlmProvider{
		ID:                 f.newID("aaaa"),
		Name:               body.Name,
		ModelProvider:      body.ModelProvider,
		AuthType:           authType,
		BaseURL:            baseURL,
		ConnectionID:       body.ConnectionID,
		Settings:           settings,
		Enabled:            true, // create has no enabled field
		EnforceHealthCheck: enforce,
		BillingMode:        billingMode,
		BillingReason:      body.BillingReason,
		BillingNote:        fakeLlmBillingNote(body.BillingNote),
		CatalogID:          body.CatalogID,
		ModelSyncMode:      syncMode,
		RequestTimeout:     body.RequestTimeout,
		StreamIdleTimeout:  body.StreamIdleTimeout,
		LastChangeNote:     note,
	}
	if !validLlmBilling(w, p) || !validLlmProviderSync(w, p) {
		return
	}
	f.providers = append(f.providers, p)
	_ = json.NewEncoder(w).Encode(providerJSON(p))
}

func (f *fakeLlmGatewayServer) updateProvider(w http.ResponseWriter, r *http.Request, id string) {
	p := f.findProvider(id)
	if p == nil {
		writeLlmError(w, http.StatusNotFound, fmt.Sprintf("provider {id: %s} not found", id))
		return
	}

	var body struct {
		Name               *string         `json:"name"`
		BaseURL            *string         `json:"base_url"`
		AuthType           *string         `json:"auth_type"`
		APIKey             *string         `json:"api_key"`
		Credentials        json.RawMessage `json:"credentials"`
		Enabled            *bool           `json:"enabled"`
		Settings           json.RawMessage `json:"settings"`
		EnforceHealthCheck *bool           `json:"enforce_health_check"`
		BillingMode        *string         `json:"billing_mode"`
		// Tri-state keys: absent keeps, null clears, a value sets.
		ConnectionID      json.RawMessage `json:"connection_id"`
		BillingReason     json.RawMessage `json:"billing_reason"`
		BillingNote       json.RawMessage `json:"billing_note"`
		RequestTimeout    json.RawMessage `json:"request_timeout_secs"`
		StreamIdleTimeout json.RawMessage `json:"stream_idle_timeout_secs"`
		ModelSyncMode     *string         `json:"model_sync_mode"`
		ChangeNote        *string         `json:"change_note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeLlmError(w, http.StatusBadRequest, err.Error())
		return
	}
	note, ok := fakeLlmChangeNote(w, body.ChangeNote)
	if !ok {
		return
	}

	updated := *p
	updated.LastChangeNote = note
	if body.ModelSyncMode != nil {
		updated.ModelSyncMode = *body.ModelSyncMode
	}
	for raw, dst := range map[*json.RawMessage]**int64{
		&body.RequestTimeout: &updated.RequestTimeout, &body.StreamIdleTimeout: &updated.StreamIdleTimeout,
	} {
		if *raw != nil {
			*dst = nil
			_ = json.Unmarshal(*raw, dst)
		}
	}
	if body.Name != nil {
		updated.Name = *body.Name
	}
	if body.AuthType != nil {
		updated.AuthType = *body.AuthType
	}
	if body.APIKey != nil || len(body.Credentials) > 0 {
		if !slices.Contains(fakeLlmRequestScopedAuth, updated.AuthType) {
			writeLlmError(w, http.StatusBadRequest, "a provider cannot store its own key: create a credential "+
				"(POST /admin/connections) and reference it with connection_id")
			return
		}
	}
	if body.ConnectionID != nil {
		var connID *string
		_ = json.Unmarshal(body.ConnectionID, &connID)
		if connID == nil {
			if p.ConnectionID != nil && !slices.Contains(fakeLlmRequestScopedAuth, updated.AuthType) {
				writeLlmError(w, http.StatusBadRequest, "a provider cannot be detached from its credential: "+
					"pick a different credential, or delete the provider")
				return
			}
			updated.ConnectionID = nil
		} else {
			conn := f.connections[*connID]
			if conn == nil {
				writeLlmError(w, http.StatusNotFound, fmt.Sprintf("connection {id: %s} not found", *connID))
				return
			}
			updated.ConnectionID = connID
			updated.AuthType = conn.AuthType
			// The credential's settings are the base on a (re)bind; the
			// provider's own keys overlay them.
			overlay := body.Settings
			if len(overlay) == 0 || string(overlay) == "null" {
				overlay = p.Settings
			}
			updated.Settings = fakeLlmMergeSettings(conn.Settings, overlay)
		}
	}
	if body.BaseURL != nil {
		if !validLlmBaseURL(w, *body.BaseURL, updated.ModelProvider, updated.AuthType) {
			return
		}
		updated.BaseURL = *body.BaseURL
	}
	if body.Enabled != nil {
		updated.Enabled = *body.Enabled
	}
	if len(body.Settings) > 0 && string(body.Settings) != "null" {
		settings, ok := validLlmSettings(w, body.Settings)
		if !ok {
			return
		}
		if updated.ConnectionID != nil {
			settings = fakeLlmMergeSettings(f.connections[*updated.ConnectionID].Settings, settings)
		}
		updated.Settings = settings
	}
	if body.EnforceHealthCheck != nil {
		updated.EnforceHealthCheck = *body.EnforceHealthCheck
	}
	if body.BillingMode != nil {
		updated.BillingMode = *body.BillingMode
	}
	if body.BillingReason != nil {
		updated.BillingReason = nil
		_ = json.Unmarshal(body.BillingReason, &updated.BillingReason)
	}
	if body.BillingNote != nil {
		var note *string
		_ = json.Unmarshal(body.BillingNote, &note)
		updated.BillingNote = fakeLlmBillingNote(note)
	}
	if !validLlmBilling(w, &updated) || !validLlmProviderSync(w, &updated) {
		return
	}

	*p = updated
	_ = json.NewEncoder(w).Encode(providerJSON(p))
}

// seedProvider plants an openai provider out-of-band, as if created in the
// app, bound to a seeded connection.
func (f *fakeLlmGatewayServer) seedProvider() *fakeLlmProvider {
	conn := f.seedConnection("openai")
	f.mu.Lock()
	defer f.mu.Unlock()
	p := &fakeLlmProvider{
		ID:                 f.newID("aaaa"),
		Name:               "Seeded openai",
		ModelProvider:      "openai",
		AuthType:           conn.AuthType,
		BaseURL:            conn.BaseURL,
		ConnectionID:       &conn.ID,
		Settings:           json.RawMessage("{}"),
		Enabled:            true,
		EnforceHealthCheck: true,
		BillingMode:        "per_token",
		ModelSyncMode:      "off",
	}
	f.providers = append(f.providers, p)
	return p
}

// --- connections ----------------------------------------------------------------

type fakeLlmConnection struct {
	ID            string
	Name          string
	ModelProvider string
	AuthType      string
	BaseURL       string
	Settings      json.RawMessage
	// Secret is the last api_key / credentials written. Stored to let tests
	// assert the write-only round trip; never rendered into a response.
	Secret         string
	LastChangeNote *string
}

func connectionJSON(c *fakeLlmConnection) map[string]any {
	out := map[string]any{
		"id":                  c.ID,
		"org_id":              fakeLlmOrgID,
		"name":                c.Name,
		"model_provider":      c.ModelProvider,
		"auth_type":           c.AuthType,
		"base_url":            c.BaseURL,
		"secret_path":         fmt.Sprintf("orgs/%s/connections/%s", fakeLlmOrgID, c.ID),
		"settings":            c.Settings,
		"key_last4":           nil,
		"stores_key_material": fakeLlmStoresKeyMaterial(c.AuthType),
		"created_at":          fakeLlmTime,
		"updated_at":          fakeLlmTime,
		"last_change_note":    c.LastChangeNote,
	}
	if out["stores_key_material"] == true && len(c.Secret) >= 4 {
		out["key_last4"] = c.Secret[len(c.Secret)-4:]
	}
	return out
}

// fakeLlmStoresKeyMaterial: the ambient-identity auth types hold no secret.
func fakeLlmStoresKeyMaterial(authType string) bool {
	return !slices.Contains([]string{
		"aws_role", "google_adc", "google_service_account_impersonation", "claude_oauth", "codex_oauth",
	}, authType)
}

// fakeLlmConnectionSecret mirrors the api-key arm of normalize_credentials:
// API-key auth types need api_key or credentials; ambient ones need nothing.
func fakeLlmConnectionSecret(w http.ResponseWriter, authType string, apiKey *string, credentials json.RawMessage) (string, bool) {
	if len(credentials) > 0 && string(credentials) != "null" {
		var obj map[string]any
		if json.Unmarshal(credentials, &obj) != nil {
			writeLlmError(w, http.StatusBadRequest, "credentials must be a JSON object")
			return "", false
		}
		return string(credentials), true
	}
	if apiKey != nil && *apiKey != "" {
		return *apiKey, true
	}
	if !fakeLlmStoresKeyMaterial(authType) {
		return "", true
	}
	writeLlmError(w, http.StatusBadRequest, "api_key or credentials.key is required for API-key providers")
	return "", false
}

// fakeLlmNormalizeConnectionSettings mirrors the Bedrock arm of
// normalize_settings: it validates required keys and adds derived ones
// (a generated external_id for aws_role, a defaulted model_api_family).
func (f *fakeLlmGatewayServer) fakeLlmNormalizeConnectionSettings(w http.ResponseWriter, modelProvider, authType string, raw json.RawMessage) (json.RawMessage, bool) {
	settings, ok := validLlmSettings(w, raw)
	if !ok {
		return nil, false
	}
	if modelProvider != "bedrock" {
		return settings, true
	}
	obj := map[string]any{}
	_ = json.Unmarshal(settings, &obj)
	if s, _ := obj["region"].(string); s == "" {
		writeLlmError(w, http.StatusBadRequest, "settings.region is required for Bedrock providers")
		return nil, false
	}
	if authType == "aws_role" {
		if s, _ := obj["iam_role_arn"].(string); s == "" {
			writeLlmError(w, http.StatusBadRequest, "settings.iam_role_arn is required for aws_role Bedrock providers")
			return nil, false
		}
		if _, ok := obj["external_id"]; !ok {
			obj["external_id"] = f.newID("ffff")
		}
	}
	if _, ok := obj["model_api_family"]; !ok {
		obj["model_api_family"] = "bedrock_converse"
	}
	out, _ := json.Marshal(obj)
	return out, true
}

func (f *fakeLlmGatewayServer) handleConnections(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/llm-gateway/admin/connections"), "/")
	conn := f.connections[id]

	switch {
	case id == "" && r.Method == http.MethodPost:
		f.createConnection(w, r)
	case id == "" && r.Method == http.MethodGet:
		items := make([]map[string]any, 0, len(f.connections))
		for _, c := range f.connections {
			items = append(items, connectionJSON(c))
		}
		_ = json.NewEncoder(w).Encode(items)
	case conn == nil:
		writeLlmError(w, http.StatusNotFound, fmt.Sprintf("connection {id: %s} not found", id))
	case r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(connectionJSON(conn))
	case r.Method == http.MethodPut:
		f.updateConnection(w, r, conn)
	case r.Method == http.MethodDelete:
		// BCP-3655: refuse while providers still read their key from it.
		var users []string
		for _, p := range f.providers {
			if p.ConnectionID != nil && *p.ConnectionID == id {
				users = append(users, p.Name)
			}
		}
		if len(users) > 0 {
			writeLlmError(w, http.StatusConflict, fmt.Sprintf(
				"these credentials are in use by %d provider(s): %s", len(users), strings.Join(users, ", ")))
			return
		}
		delete(f.connections, id)
		_ = json.NewEncoder(w).Encode(map[string]any{"deleted": true})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeLlmGatewayServer) createConnection(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name          string          `json:"name"`
		ModelProvider string          `json:"model_provider"`
		AuthType      *string         `json:"auth_type"`
		BaseURL       string          `json:"base_url"`
		APIKey        *string         `json:"api_key"`
		Credentials   json.RawMessage `json:"credentials"`
		Settings      json.RawMessage `json:"settings"`
		ChangeNote    *string         `json:"change_note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeLlmError(w, http.StatusBadRequest, err.Error())
		return
	}
	note, ok := fakeLlmChangeNote(w, body.ChangeNote)
	if !ok {
		return
	}
	if !slices.Contains(fakeLlmModelProviders, body.ModelProvider) {
		writeLlmError(w, http.StatusBadRequest, "unknown model provider: "+body.ModelProvider)
		return
	}
	authType := fakeLlmDefaultAuthType(body.ModelProvider, body.AuthType)
	if !validLlmBaseURL(w, body.BaseURL, body.ModelProvider, authType) {
		return
	}
	settings, ok := f.fakeLlmNormalizeConnectionSettings(w, body.ModelProvider, authType, body.Settings)
	if !ok {
		return
	}
	secret, ok := fakeLlmConnectionSecret(w, authType, body.APIKey, body.Credentials)
	if !ok {
		return
	}
	c := &fakeLlmConnection{
		ID:             f.newID("cccc"),
		Name:           body.Name,
		ModelProvider:  body.ModelProvider,
		AuthType:       authType,
		BaseURL:        body.BaseURL,
		Settings:       settings,
		Secret:         secret,
		LastChangeNote: note,
	}
	f.connections[c.ID] = c
	_ = json.NewEncoder(w).Encode(connectionJSON(c))
}

// updateConnection applies COALESCE semantics, and like production moves
// every provider that was following the old endpoint.
func (f *fakeLlmGatewayServer) updateConnection(w http.ResponseWriter, r *http.Request, c *fakeLlmConnection) {
	var body struct {
		Name        *string         `json:"name"`
		BaseURL     *string         `json:"base_url"`
		AuthType    *string         `json:"auth_type"`
		APIKey      *string         `json:"api_key"`
		Credentials json.RawMessage `json:"credentials"`
		Settings    json.RawMessage `json:"settings"`
		ChangeNote  *string         `json:"change_note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeLlmError(w, http.StatusBadRequest, err.Error())
		return
	}
	note, ok := fakeLlmChangeNote(w, body.ChangeNote)
	if !ok {
		return
	}
	updated := *c
	updated.LastChangeNote = note
	if body.Name != nil {
		updated.Name = *body.Name
	}
	if body.AuthType != nil {
		updated.AuthType = *body.AuthType
	}
	if body.BaseURL != nil {
		if !validLlmBaseURL(w, *body.BaseURL, c.ModelProvider, updated.AuthType) {
			return
		}
		updated.BaseURL = *body.BaseURL
	}
	if len(body.Settings) > 0 && string(body.Settings) != "null" {
		// Stored derived keys survive a re-save that omits them.
		merged := fakeLlmMergeSettings(c.Settings, body.Settings)
		settings, ok := f.fakeLlmNormalizeConnectionSettings(w, c.ModelProvider, updated.AuthType, merged)
		if !ok {
			return
		}
		updated.Settings = settings
	}
	if body.APIKey != nil || len(body.Credentials) > 0 {
		secret, ok := fakeLlmConnectionSecret(w, updated.AuthType, body.APIKey, body.Credentials)
		if !ok {
			return
		}
		updated.Secret = secret
	}
	for _, p := range f.providers {
		if p.ConnectionID != nil && *p.ConnectionID == c.ID && p.BaseURL == c.BaseURL {
			p.BaseURL = updated.BaseURL
		}
	}
	*c = updated
	_ = json.NewEncoder(w).Encode(connectionJSON(c))
}

// seedConnection plants an API-key connection out-of-band.
func (f *fakeLlmGatewayServer) seedConnection(modelProvider string) *fakeLlmConnection {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := &fakeLlmConnection{
		ID:            f.newID("cccc"),
		Name:          "Seeded " + modelProvider + " key",
		ModelProvider: modelProvider,
		AuthType:      fakeLlmDefaultAuthType(modelProvider, nil),
		BaseURL:       "https://upstream.example.com",
		Settings:      json.RawMessage("{}"),
		Secret:        "seeded-key",
	}
	f.connections[c.ID] = c
	return c
}

// connectionSecret reads a connection's stored (never-echoed) secret.
func (f *fakeLlmGatewayServer) connectionSecret(t *testing.T, id string) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.connections[id]
	if c == nil {
		t.Fatalf("fake has no connection %q", id)
	}
	return c.Secret
}

// checkAllLlmConnectionsDeleted is the CheckDestroy for connection tests.
func checkAllLlmConnectionsDeleted(fake *fakeLlmGatewayServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, c := range fake.connections {
			return fmt.Errorf("LLM connection %s (%s) was not deleted on destroy", c.ID, c.Name)
		}
		return nil
	}
}

// markProviderDeleted removes a stored provider out-of-band.
func (f *fakeLlmGatewayServer) markProviderDeleted(t *testing.T, id string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, p := range f.providers {
		if p.ID == id {
			f.providers = append(f.providers[:i], f.providers[i+1:]...)
			return
		}
	}
	t.Fatalf("fake has no provider %q to delete", id)
}

// checkAllLlmProvidersDeleted is the CheckDestroy for provider tests.
func checkAllLlmProvidersDeleted(fake *fakeLlmGatewayServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, p := range fake.providers {
			return fmt.Errorf("LLM provider %s (%s) was not deleted on destroy", p.ID, p.Name)
		}
		return nil
	}
}

// --- model mappings --------------------------------------------------------------

func (f *fakeLlmGatewayServer) findMapping(id string) *fakeLlmModelMapping {
	for _, m := range f.mappings {
		if m.ID == id {
			return m
		}
	}
	return nil
}

func mappingJSON(f *fakeLlmGatewayServer, m *fakeLlmModelMapping, withProvider bool) map[string]any {
	out := map[string]any{
		"id":                         m.ID,
		"provider_id":                m.ProviderID,
		"model_alias":                m.ModelAlias,
		"upstream_model":             m.UpstreamModel,
		"enabled":                    m.Enabled,
		"priority":                   m.Priority,
		"retry_on_429_count":         m.RetryOn429Count,
		"retry_on_429_max_wait_secs": m.RetryOn429MaxWaitSecs,
		"bare_alias":                 m.BareAlias,
		"stream_idle_timeout_secs":   m.StreamIdleTimeoutSecs,
		"request_timeout_secs":       m.RequestTimeoutSecs,
		"last_change_note":           m.LastChangeNote,
	}
	raw, _ := json.Marshal(m.Cooldown)
	_ = json.Unmarshal(raw, &out)
	if withProvider {
		// The org-wide listing wraps rows with provider annotations the
		// provider must ignore.
		providerName, providerAuthType := "unknown", "bearer_api_key"
		if p := f.findProvider(m.ProviderID); p != nil {
			providerName, providerAuthType = p.Name, p.AuthType
		}
		out["provider_name"] = providerName
		out["provider_auth_type"] = providerAuthType
	}
	return out
}

func (f *fakeLlmGatewayServer) handleModelMappings(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/llm-gateway/admin/model-mappings"), "/")

	switch {
	case id == "" && r.Method == http.MethodPost:
		f.createModelMapping(w, r)
	case id == "" && r.Method == http.MethodGet:
		items := make([]map[string]any, 0, len(f.mappings))
		for _, m := range f.mappings {
			items = append(items, mappingJSON(f, m, true))
		}
		_ = json.NewEncoder(w).Encode(items)
	case id != "" && id != "reorder" && r.Method == http.MethodPut:
		f.updateModelMapping(w, r, id)
	case id != "" && r.Method == http.MethodDelete:
		for i, m := range f.mappings {
			if m.ID == id {
				f.mappings = append(f.mappings[:i], f.mappings[i+1:]...)
				f.forgetRouteGroupAliasIfGone(m.ModelAlias)
				_ = json.NewEncoder(w).Encode(map[string]any{"deleted": true})
				return
			}
		}
		writeLlmError(w, http.StatusNotFound, fmt.Sprintf("model mapping {id: %s} not found", id))
	default:
		http.NotFound(w, r)
	}
}

func validLlmMappingRanges(w http.ResponseWriter, retryCount, retryMaxWait int64, streamIdle, requestTimeout *int64) bool {
	if retryCount < 0 || retryCount > 10 {
		writeLlmError(w, http.StatusBadRequest, "retry_on_429_count must be between 0 and 10")
		return false
	}
	if retryMaxWait < 0 || retryMaxWait > 180 {
		writeLlmError(w, http.StatusBadRequest, "retry_on_429_max_wait_secs must be between 0 and 180")
		return false
	}
	if streamIdle != nil && (*streamIdle < 1 || *streamIdle > 300) {
		writeLlmError(w, http.StatusBadRequest, "stream_idle_timeout_secs must be between 1 and 300")
		return false
	}
	if requestTimeout != nil && (*requestTimeout < 1 || *requestTimeout > 600) {
		writeLlmError(w, http.StatusBadRequest, "request_timeout_secs must be between 1 and 600")
		return false
	}
	return true
}

func (f *fakeLlmGatewayServer) createModelMapping(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProviderID            string  `json:"provider_id"`
		ModelAlias            string  `json:"model_alias"`
		UpstreamModel         string  `json:"upstream_model"`
		Enabled               *bool   `json:"enabled"`
		Priority              int64   `json:"priority"`
		RetryOn429Count       int64   `json:"retry_on_429_count"`
		RetryOn429MaxWaitSecs int64   `json:"retry_on_429_max_wait_secs"`
		BareAlias             *bool   `json:"bare_alias"`
		StreamIdleTimeoutSecs *int64  `json:"stream_idle_timeout_secs"`
		RequestTimeoutSecs    *int64  `json:"request_timeout_secs"`
		ChangeNote            *string `json:"change_note"`
		fakeLlmCooldownPatch
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeLlmError(w, http.StatusBadRequest, err.Error())
		return
	}
	note, ok := fakeLlmChangeNote(w, body.ChangeNote)
	if !ok {
		return
	}
	if !validLlmMappingRanges(w, body.RetryOn429Count, body.RetryOn429MaxWaitSecs,
		body.StreamIdleTimeoutSecs, body.RequestTimeoutSecs) {
		return
	}
	// Tenant isolation: the provider must exist in the org.
	if f.findProvider(body.ProviderID) == nil {
		writeLlmError(w, http.StatusNotFound, fmt.Sprintf("provider %s not found", body.ProviderID))
		return
	}

	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	alias1to1 := body.ModelAlias == body.UpstreamModel
	bareAlias := !alias1to1
	if body.BareAlias != nil {
		bareAlias = *body.BareAlias
	}

	// The anchor is the 1:1 enablement row for (provider, upstream).
	var anchor *fakeLlmModelMapping
	for _, m := range f.mappings {
		if m.ProviderID == body.ProviderID && m.UpstreamModel == body.UpstreamModel &&
			m.ModelAlias == m.UpstreamModel {
			anchor = m
			break
		}
	}

	// Orphan-alias guard (BCP-2920): a custom alias without its 1:1
	// enablement would never resolve at request time.
	if !alias1to1 && anchor == nil {
		writeLlmError(w, http.StatusBadRequest, fmt.Sprintf(
			"Cannot route to '%s' on this provider: the model is not enabled. "+
				"Enable it under LLM Configuration → Models first, then create the route.",
			body.UpstreamModel))
		return
	}

	// PATCH-or-create for 1:1 rows: a second POST folds into the existing
	// anchor instead of duplicating.
	if alias1to1 && anchor != nil {
		// Upsert-only for the cooldown policy: omitted keys keep the stored
		// value rather than resetting it.
		cooldown := anchor.Cooldown.apply(body.fakeLlmCooldownPatch)
		if !validLlmCooldown(w, cooldown) {
			return
		}
		anchor.Cooldown = cooldown
		anchor.LastChangeNote = note
		anchor.Enabled = enabled
		anchor.Priority = body.Priority
		anchor.RetryOn429Count = body.RetryOn429Count
		anchor.RetryOn429MaxWaitSecs = body.RetryOn429MaxWaitSecs
		anchor.BareAlias = bareAlias
		if body.StreamIdleTimeoutSecs != nil {
			anchor.StreamIdleTimeoutSecs = *body.StreamIdleTimeoutSecs
		}
		if body.RequestTimeoutSecs != nil {
			anchor.RequestTimeoutSecs = *body.RequestTimeoutSecs
		}
		_ = json.NewEncoder(w).Encode(mappingJSON(f, anchor, false))
		return
	}

	// Materialize the platform timeout defaults at insert, like production.
	streamIdle := int64(fakeLlmStreamIdleDefault)
	if body.StreamIdleTimeoutSecs != nil {
		streamIdle = *body.StreamIdleTimeoutSecs
	}
	requestTimeout := int64(fakeLlmRequestDefault)
	if body.RequestTimeoutSecs != nil {
		requestTimeout = *body.RequestTimeoutSecs
	}
	cooldown := fakeLlmCooldownDefaults.apply(body.fakeLlmCooldownPatch)
	if !validLlmCooldown(w, cooldown) {
		return
	}

	m := &fakeLlmModelMapping{
		ID:                    f.newID("bbbb"),
		ProviderID:            body.ProviderID,
		ModelAlias:            body.ModelAlias,
		UpstreamModel:         body.UpstreamModel,
		Enabled:               enabled,
		Priority:              body.Priority,
		RetryOn429Count:       body.RetryOn429Count,
		RetryOn429MaxWaitSecs: body.RetryOn429MaxWaitSecs,
		BareAlias:             bareAlias,
		StreamIdleTimeoutSecs: streamIdle,
		RequestTimeoutSecs:    requestTimeout,
		Cooldown:              cooldown,
		LastChangeNote:        note,
	}
	f.mappings = append(f.mappings, m)
	_ = json.NewEncoder(w).Encode(mappingJSON(f, m, false))
}

func (f *fakeLlmGatewayServer) updateModelMapping(w http.ResponseWriter, r *http.Request, id string) {
	m := f.findMapping(id)
	if m == nil {
		writeLlmError(w, http.StatusNotFound, fmt.Sprintf("model mapping {id: %s} not found", id))
		return
	}

	var body struct {
		ModelAlias            *string `json:"model_alias"`
		UpstreamModel         *string `json:"upstream_model"`
		Enabled               *bool   `json:"enabled"`
		Priority              *int64  `json:"priority"`
		RetryOn429Count       *int64  `json:"retry_on_429_count"`
		RetryOn429MaxWaitSecs *int64  `json:"retry_on_429_max_wait_secs"`
		BareAlias             *bool   `json:"bare_alias"`
		StreamIdleTimeoutSecs *int64  `json:"stream_idle_timeout_secs"`
		RequestTimeoutSecs    *int64  `json:"request_timeout_secs"`
		ChangeNote            *string `json:"change_note"`
		fakeLlmCooldownPatch
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeLlmError(w, http.StatusBadRequest, err.Error())
		return
	}
	note, ok := fakeLlmChangeNote(w, body.ChangeNote)
	if !ok {
		return
	}

	updated := *m
	updated.LastChangeNote = note
	if body.ModelAlias != nil {
		updated.ModelAlias = *body.ModelAlias
	}
	if body.UpstreamModel != nil {
		updated.UpstreamModel = *body.UpstreamModel
	}
	if body.Enabled != nil {
		updated.Enabled = *body.Enabled
	}
	if body.Priority != nil {
		updated.Priority = *body.Priority
	}
	if body.RetryOn429Count != nil {
		updated.RetryOn429Count = *body.RetryOn429Count
	}
	if body.RetryOn429MaxWaitSecs != nil {
		updated.RetryOn429MaxWaitSecs = *body.RetryOn429MaxWaitSecs
	}
	if body.BareAlias != nil {
		updated.BareAlias = *body.BareAlias
	}
	if body.StreamIdleTimeoutSecs != nil {
		updated.StreamIdleTimeoutSecs = *body.StreamIdleTimeoutSecs
	}
	if body.RequestTimeoutSecs != nil {
		updated.RequestTimeoutSecs = *body.RequestTimeoutSecs
	}
	retryCount := updated.RetryOn429Count
	retryMaxWait := updated.RetryOn429MaxWaitSecs
	if !validLlmMappingRanges(w, retryCount, retryMaxWait,
		&updated.StreamIdleTimeoutSecs, &updated.RequestTimeoutSecs) {
		return
	}
	updated.Cooldown = updated.Cooldown.apply(body.fakeLlmCooldownPatch)
	if !validLlmCooldown(w, updated.Cooldown) {
		return
	}

	previousAlias := m.ModelAlias
	*m = updated
	f.renameRouteGroupAlias(previousAlias, m.ModelAlias)
	_ = json.NewEncoder(w).Encode(mappingJSON(f, m, false))
}

// markMappingDeleted removes a stored mapping out-of-band.
func (f *fakeLlmGatewayServer) markMappingDeleted(t *testing.T, id string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, m := range f.mappings {
		if m.ID == id {
			f.mappings = append(f.mappings[:i], f.mappings[i+1:]...)
			f.forgetRouteGroupAliasIfGone(m.ModelAlias)
			return
		}
	}
	t.Fatalf("fake has no model mapping %q to delete", id)
}

// checkAllLlmMappingsDeleted is the CheckDestroy for model-mapping tests.
func checkAllLlmMappingsDeleted(fake *fakeLlmGatewayServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, m := range fake.mappings {
			return fmt.Errorf("model mapping %s (%s → %s) was not deleted on destroy",
				m.ID, m.ModelAlias, m.UpstreamModel)
		}
		return nil
	}
}

// --- model access ----------------------------------------------------------------

var fakeLlmModelAccessScopeTypes = []string{
	"org", "team", "user", "project", "api_key", "mcp_server", "agent", "role", "group",
}

func (f *fakeLlmGatewayServer) findPolicy(id string) *fakeLlmModelAccessPolicy {
	for _, p := range f.policies {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func modelAccessJSON(p *fakeLlmModelAccessPolicy) map[string]any {
	targets := p.Targets
	if targets == nil {
		targets = []json.RawMessage{}
	}
	return map[string]any{
		"id":               p.ID,
		"org_id":           fakeLlmOrgID,
		"name":             p.Name,
		"scope_type":       p.ScopeType,
		"scope_id":         p.ScopeID,
		"scope_value":      p.ScopeValue,
		"policy_type":      p.PolicyType,
		"targets":          targets,
		"traffic_type":     p.TrafficType,
		"enabled":          p.Enabled,
		"last_change_note": p.LastChangeNote,
	}
}

// fakeLlmUUIDRe loosely matches the canonical UUID text form.
var fakeLlmUUIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// validLlmModelAccessTargets mirrors production's serde of the tagged
// ModelAccessTarget enum: a known kind with its required fields. A malformed
// provider_id fails UUID deserialization, which axum's Json extractor
// answers with a plain-text 422 (no OpenAI envelope) — mirror that shape.
func validLlmModelAccessTargets(w http.ResponseWriter, targets []json.RawMessage) bool {
	for i, raw := range targets {
		var t struct {
			Kind       string  `json:"kind"`
			Alias      *string `json:"alias"`
			Model      *string `json:"model"`
			ProviderID *string `json:"provider_id"`
			GroupID    *string `json:"group_id"`
		}
		if err := json.Unmarshal(raw, &t); err != nil {
			writeLlmError(w, http.StatusBadRequest, "malformed target: "+err.Error())
			return false
		}
		if t.ProviderID != nil && !fakeLlmUUIDRe.MatchString(*t.ProviderID) {
			http.Error(w, fmt.Sprintf(
				"Failed to deserialize the JSON body into the target type: targets[%d].provider_id: UUID parsing failed", i),
				http.StatusUnprocessableEntity)
			return false
		}
		// group_id is a Uuid too; like provider_id, existence is not checked.
		if t.GroupID != nil && !fakeLlmUUIDRe.MatchString(*t.GroupID) {
			http.Error(w, fmt.Sprintf(
				"Failed to deserialize the JSON body into the target type: targets[%d].group_id: UUID parsing failed", i),
				http.StatusUnprocessableEntity)
			return false
		}
		ok := false
		switch t.Kind {
		case "model_alias":
			ok = t.Alias != nil
		case "model":
			ok = t.Model != nil
		case "provider":
			ok = t.ProviderID != nil
		case "provider_model":
			ok = t.ProviderID != nil && t.Model != nil
		case "route_group":
			ok = t.GroupID != nil
		}
		if !ok {
			writeLlmError(w, http.StatusBadRequest,
				fmt.Sprintf("malformed target of kind '%s': missing required fields", t.Kind))
			return false
		}
	}
	return true
}

func (f *fakeLlmGatewayServer) handleModelAccess(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/llm-gateway/admin/model-access"), "/")

	switch {
	case id == "" && r.Method == http.MethodPost:
		f.createModelAccess(w, r)
	case id == "" && r.Method == http.MethodGet:
		items := make([]map[string]any, 0, len(f.policies))
		for _, p := range f.policies {
			items = append(items, modelAccessJSON(p))
		}
		_ = json.NewEncoder(w).Encode(items)
	case id != "" && r.Method == http.MethodPut:
		f.updateModelAccess(w, r, id)
	case id != "" && r.Method == http.MethodDelete:
		for i, p := range f.policies {
			if p.ID == id {
				f.policies = append(f.policies[:i], f.policies[i+1:]...)
				_ = json.NewEncoder(w).Encode(map[string]any{"deleted": true})
				return
			}
		}
		writeLlmError(w, http.StatusNotFound, fmt.Sprintf("model access policy {id: %s} not found", id))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeLlmGatewayServer) createModelAccess(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string            `json:"name"`
		ScopeType   string            `json:"scope_type"`
		ScopeID     *string           `json:"scope_id"`
		ScopeValue  *string           `json:"scope_value"`
		PolicyType  string            `json:"policy_type"`
		Targets     []json.RawMessage `json:"targets"`
		TrafficType *string           `json:"traffic_type"`
		ChangeNote  *string           `json:"change_note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeLlmError(w, http.StatusBadRequest, err.Error())
		return
	}
	note, ok := fakeLlmChangeNote(w, body.ChangeNote)
	if !ok {
		return
	}
	if len(body.Targets) == 0 {
		writeLlmError(w, http.StatusBadRequest, "at least one target is required")
		return
	}
	if !slices.Contains(fakeLlmModelAccessScopeTypes, body.ScopeType) {
		writeLlmError(w, http.StatusBadRequest, fmt.Sprintf(
			"invalid scope_type '%s'. Must be one of: %s",
			body.ScopeType, strings.Join(fakeLlmModelAccessScopeTypes, ", ")))
		return
	}
	if body.PolicyType != "allowlist" && body.PolicyType != "denylist" {
		writeLlmError(w, http.StatusBadRequest, "policy_type must be 'allowlist' or 'denylist'")
		return
	}
	if !validLlmModelAccessTargets(w, body.Targets) {
		return
	}

	trafficType := "llm"
	if body.TrafficType != nil {
		trafficType = *body.TrafficType
	}

	p := &fakeLlmModelAccessPolicy{
		ID:             f.newID("cccc"),
		Name:           body.Name,
		ScopeType:      body.ScopeType,
		ScopeID:        body.ScopeID,
		ScopeValue:     body.ScopeValue,
		PolicyType:     body.PolicyType,
		Targets:        body.Targets,
		TrafficType:    trafficType,
		Enabled:        true, // create has no enabled field
		LastChangeNote: note,
	}
	f.policies = append(f.policies, p)
	_ = json.NewEncoder(w).Encode(modelAccessJSON(p))
}

func (f *fakeLlmGatewayServer) updateModelAccess(w http.ResponseWriter, r *http.Request, id string) {
	p := f.findPolicy(id)
	if p == nil {
		writeLlmError(w, http.StatusNotFound, fmt.Sprintf("model access policy {id: %s} not found", id))
		return
	}

	var body struct {
		Name        *string            `json:"name"`
		ScopeType   *string            `json:"scope_type"`
		ScopeID     *string            `json:"scope_id"`
		ScopeValue  *string            `json:"scope_value"`
		PolicyType  *string            `json:"policy_type"`
		Targets     *[]json.RawMessage `json:"targets"`
		TrafficType *string            `json:"traffic_type"`
		Enabled     *bool              `json:"enabled"`
		ChangeNote  *string            `json:"change_note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeLlmError(w, http.StatusBadRequest, err.Error())
		return
	}
	note, ok := fakeLlmChangeNote(w, body.ChangeNote)
	if !ok {
		return
	}
	if body.ScopeType != nil && !slices.Contains(fakeLlmModelAccessScopeTypes, *body.ScopeType) {
		writeLlmError(w, http.StatusBadRequest, fmt.Sprintf(
			"invalid scope_type '%s'. Must be one of: %s",
			*body.ScopeType, strings.Join(fakeLlmModelAccessScopeTypes, ", ")))
		return
	}
	if body.PolicyType != nil && *body.PolicyType != "allowlist" && *body.PolicyType != "denylist" {
		writeLlmError(w, http.StatusBadRequest, "policy_type must be 'allowlist' or 'denylist'")
		return
	}
	if body.Targets != nil {
		if len(*body.Targets) == 0 {
			writeLlmError(w, http.StatusBadRequest, "targets cannot be empty when provided")
			return
		}
		if !validLlmModelAccessTargets(w, *body.Targets) {
			return
		}
	}

	// COALESCE semantics: present keys overwrite, absent keys keep.
	if body.Name != nil {
		p.Name = *body.Name
	}
	if body.ScopeType != nil {
		p.ScopeType = *body.ScopeType
	}
	if body.ScopeID != nil {
		p.ScopeID = body.ScopeID
	}
	if body.ScopeValue != nil {
		p.ScopeValue = body.ScopeValue
	}
	if body.PolicyType != nil {
		p.PolicyType = *body.PolicyType
	}
	if body.Targets != nil {
		p.Targets = *body.Targets
	}
	if body.TrafficType != nil {
		p.TrafficType = *body.TrafficType
	}
	if body.Enabled != nil {
		p.Enabled = *body.Enabled
	}

	p.LastChangeNote = note
	_ = json.NewEncoder(w).Encode(modelAccessJSON(p))
}

// markPolicyDeletedLlm removes a stored model-access policy out-of-band.
func (f *fakeLlmGatewayServer) markPolicyDeletedLlm(t *testing.T, id string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, p := range f.policies {
		if p.ID == id {
			f.policies = append(f.policies[:i], f.policies[i+1:]...)
			return
		}
	}
	t.Fatalf("fake has no model access policy %q to delete", id)
}

// checkAllLlmModelAccessDeleted is the CheckDestroy for model-access tests.
func checkAllLlmModelAccessDeleted(fake *fakeLlmGatewayServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, p := range fake.policies {
			return fmt.Errorf("model access policy %s (%s) was not deleted on destroy", p.ID, p.Name)
		}
		return nil
	}
}

// --- rate limits -----------------------------------------------------------------

func (f *fakeLlmGatewayServer) findRateLimit(id string) *fakeLlmRateLimit {
	for _, p := range f.rateLimits {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func rateLimitJSON(p *fakeLlmRateLimit) map[string]any {
	return map[string]any{
		"id":                    p.ID,
		"org_id":                fakeLlmOrgID,
		"name":                  p.Name,
		"scope_type":            p.ScopeType,
		"scope_id":              p.ScopeID,
		"scope_value":           p.ScopeValue,
		"requests_per_minute":   p.RequestsPerMinute,
		"tokens_per_minute":     p.TokensPerMinute,
		"member_of_group":       p.MemberOfGroup,
		"traffic_type":          p.TrafficType,
		"enabled":               p.Enabled,
		"target_provider_id":    p.Targets.ProviderID,
		"target_upstream_model": p.Targets.UpstreamModel,
		"target_model_alias":    p.Targets.ModelAlias,
		"target_mcp_server_id":  p.Targets.McpServerID,
		"last_change_note":      p.LastChangeNote,
	}
}

// rateLimitScopeTaken mirrors the rate_limit_policies_scope_unique_idx
// (org, scope_type, scope_id, scope_value, traffic_type, member_of_group, the
// four targets — V74).
func (f *fakeLlmGatewayServer) rateLimitScopeTaken(scopeType string, scopeID, scopeValue *string, trafficType string, memberOfGroup *string, targets fakeLlmTargets, excludeID string) bool {
	for _, p := range f.rateLimits {
		if p.ID == excludeID {
			continue
		}
		if p.ScopeType == scopeType && strPtrEq(p.ScopeID, scopeID) &&
			strPtrEq(p.ScopeValue, scopeValue) && p.TrafficType == trafficType &&
			strPtrEq(p.MemberOfGroup, memberOfGroup) && p.Targets.eq(targets) {
			return true
		}
	}
	return false
}

func strPtrEq(a, b *string) bool {
	av, bv := "", ""
	if a != nil {
		av = *a
	}
	if b != nil {
		bv = *b
	}
	return av == bv
}

// fakeMemberOfGroupShape mirrors production's normalize_member_of_group (trim,
// blank means absent) and validate_member_of_group_shape (the filter is only
// valid on a broad user-scoped rule). It writes the 400 and returns false on a
// rejected shape.
func fakeMemberOfGroupShape(w http.ResponseWriter, raw *string, scopeType string, scopeID, scopeValue *string) (*string, bool) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, true
	}
	group := strings.TrimSpace(*raw)
	switch {
	case scopeType != "user":
		writeLlmError(w, http.StatusBadRequest, "member_of_group requires scope_type=user")
		return nil, false
	case scopeID != nil:
		writeLlmError(w, http.StatusBadRequest, "member_of_group cannot be combined with scope_id")
		return nil, false
	case scopeValue != nil:
		writeLlmError(w, http.StatusBadRequest, "member_of_group cannot be combined with scope_value")
		return nil, false
	}
	return &group, true
}

func (f *fakeLlmGatewayServer) handleRateLimits(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/llm-gateway/admin/rate-limits"), "/")

	switch {
	case id == "" && r.Method == http.MethodPost:
		f.createRateLimit(w, r)
	case id == "" && r.Method == http.MethodGet:
		items := make([]map[string]any, 0, len(f.rateLimits))
		for _, p := range f.rateLimits {
			items = append(items, rateLimitJSON(p))
		}
		_ = json.NewEncoder(w).Encode(items)
	case id != "" && id != "status" && r.Method == http.MethodPut:
		f.updateRateLimit(w, r, id)
	case id != "" && r.Method == http.MethodDelete:
		for i, p := range f.rateLimits {
			if p.ID == id {
				f.rateLimits = append(f.rateLimits[:i], f.rateLimits[i+1:]...)
				_ = json.NewEncoder(w).Encode(map[string]any{"deleted": true})
				return
			}
		}
		writeLlmError(w, http.StatusNotFound, fmt.Sprintf("rate limit policy {id: %s} not found", id))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeLlmGatewayServer) createRateLimit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name              string  `json:"name"`
		ScopeType         string  `json:"scope_type"`
		ScopeID           *string `json:"scope_id"`
		ScopeValue        *string `json:"scope_value"`
		RequestsPerMinute *int64  `json:"requests_per_minute"`
		TokensPerMinute   *int64  `json:"tokens_per_minute"`
		MemberOfGroup     *string `json:"member_of_group"`
		TrafficType       *string `json:"traffic_type"`
		ChangeNote        *string `json:"change_note"`
		fakeLlmTargets
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeLlmError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.RequestsPerMinute == nil && body.TokensPerMinute == nil {
		writeLlmError(w, http.StatusBadRequest,
			"at least one of requests_per_minute or tokens_per_minute is required")
		return
	}
	group, ok := fakeMemberOfGroupShape(w, body.MemberOfGroup, body.ScopeType, body.ScopeID, body.ScopeValue)
	if !ok {
		return
	}

	trafficType := "all"
	if body.TrafficType != nil {
		trafficType = *body.TrafficType
	}
	if !validLlmTargetShape(w, body.fakeLlmTargets, trafficType) {
		return
	}
	note, ok := fakeLlmChangeNote(w, body.ChangeNote)
	if !ok {
		return
	}
	if f.rateLimitScopeTaken(body.ScopeType, body.ScopeID, body.ScopeValue, trafficType, group, body.fakeLlmTargets, "") {
		writeLlmError(w, http.StatusConflict,
			"A policy with this scope and traffic type already exists.")
		return
	}

	p := &fakeLlmRateLimit{
		ID:                f.newID("dddd"),
		Name:              body.Name,
		ScopeType:         body.ScopeType,
		ScopeID:           body.ScopeID,
		ScopeValue:        body.ScopeValue,
		RequestsPerMinute: body.RequestsPerMinute,
		TokensPerMinute:   body.TokensPerMinute,
		MemberOfGroup:     group,
		TrafficType:       trafficType,
		Enabled:           true, // create has no enabled field
		Targets:           body.fakeLlmTargets,
		LastChangeNote:    note,
	}
	f.rateLimits = append(f.rateLimits, p)
	_ = json.NewEncoder(w).Encode(rateLimitJSON(p))
}

// updateRateLimit applies production's tri-state PATCH semantics on the two
// metrics: an absent key keeps the current value, an explicit null clears it,
// and a number sets it. Everything else is COALESCE. member_of_group is not
// part of the update contract (production drops the key), and a scope change
// on a filtered policy is rejected (BCP-3896).
func (f *fakeLlmGatewayServer) updateRateLimit(w http.ResponseWriter, r *http.Request, id string) {
	p := f.findRateLimit(id)
	if p == nil {
		writeLlmError(w, http.StatusNotFound, "rate limit policy not found")
		return
	}

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeLlmError(w, http.StatusBadRequest, err.Error())
		return
	}

	updated := *p
	if v, ok := raw["name"]; ok {
		_ = json.Unmarshal(v, &updated.Name)
	}
	if v, ok := raw["scope_type"]; ok {
		_ = json.Unmarshal(v, &updated.ScopeType)
	}
	if v, ok := raw["scope_id"]; ok && string(v) != "null" {
		_ = json.Unmarshal(v, &updated.ScopeID)
	}
	if v, ok := raw["scope_value"]; ok && string(v) != "null" {
		_ = json.Unmarshal(v, &updated.ScopeValue)
	}
	if v, ok := raw["requests_per_minute"]; ok {
		if string(v) == "null" {
			updated.RequestsPerMinute = nil
		} else {
			_ = json.Unmarshal(v, &updated.RequestsPerMinute)
		}
	}
	if v, ok := raw["tokens_per_minute"]; ok {
		if string(v) == "null" {
			updated.TokensPerMinute = nil
		} else {
			_ = json.Unmarshal(v, &updated.TokensPerMinute)
		}
	}
	if v, ok := raw["traffic_type"]; ok && string(v) != "null" {
		_ = json.Unmarshal(v, &updated.TrafficType)
	}
	if v, ok := raw["enabled"]; ok && string(v) != "null" {
		_ = json.Unmarshal(v, &updated.Enabled)
	}
	// The targets are tri-state like the metrics (deserialize_explicit_null).
	for key, dst := range map[string]**string{
		"target_provider_id":    &updated.Targets.ProviderID,
		"target_upstream_model": &updated.Targets.UpstreamModel,
		"target_model_alias":    &updated.Targets.ModelAlias,
		"target_mcp_server_id":  &updated.Targets.McpServerID,
	} {
		if v, ok := raw[key]; ok {
			*dst = nil
			if string(v) != "null" {
				_ = json.Unmarshal(v, dst)
			}
		}
	}
	// change_note is a plain Option: an omitted note clears the last one.
	var notePtr *string
	if v, ok := raw["change_note"]; ok && string(v) != "null" {
		_ = json.Unmarshal(v, &notePtr)
	}
	note, ok := fakeLlmChangeNote(w, notePtr)
	if !ok {
		return
	}
	updated.LastChangeNote = note

	if updated.RequestsPerMinute != nil && *updated.RequestsPerMinute < 0 {
		writeLlmError(w, http.StatusBadRequest, "requests_per_minute cannot be negative")
		return
	}
	if updated.TokensPerMinute != nil && *updated.TokensPerMinute < 0 {
		writeLlmError(w, http.StatusBadRequest, "tokens_per_minute cannot be negative")
		return
	}
	if updated.RequestsPerMinute == nil && updated.TokensPerMinute == nil {
		writeLlmError(w, http.StatusBadRequest,
			"at least one of requests_per_minute or tokens_per_minute must be set")
		return
	}
	if updated.MemberOfGroup != nil &&
		(updated.ScopeType != "user" || updated.ScopeID != nil || updated.ScopeValue != nil) {
		writeLlmError(w, http.StatusBadRequest,
			"this rate limit policy gives each member of '"+*updated.MemberOfGroup+
				"' their own allowance, so its scope cannot be changed")
		return
	}
	if !validLlmTargetShape(w, updated.Targets, updated.TrafficType) {
		return
	}
	if f.rateLimitScopeTaken(updated.ScopeType, updated.ScopeID, updated.ScopeValue, updated.TrafficType,
		updated.MemberOfGroup, updated.Targets, p.ID) {
		writeLlmError(w, http.StatusConflict,
			"A policy with this scope and traffic type already exists.")
		return
	}

	*p = updated
	_ = json.NewEncoder(w).Encode(rateLimitJSON(p))
}

// markRateLimitDeleted removes a stored rate-limit policy out-of-band.
func (f *fakeLlmGatewayServer) markRateLimitDeleted(t *testing.T, id string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, p := range f.rateLimits {
		if p.ID == id {
			f.rateLimits = append(f.rateLimits[:i], f.rateLimits[i+1:]...)
			return
		}
	}
	t.Fatalf("fake has no rate limit policy %q to delete", id)
}

// checkAllLlmRateLimitsDeleted is the CheckDestroy for rate-limit tests.
func checkAllLlmRateLimitsDeleted(fake *fakeLlmGatewayServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, p := range fake.rateLimits {
			return fmt.Errorf("rate limit policy %s (%s) was not deleted on destroy", p.ID, p.Name)
		}
		return nil
	}
}

// --- token budgets ---------------------------------------------------------------

func (f *fakeLlmGatewayServer) findBudget(id string) *fakeLlmTokenBudget {
	for _, b := range f.budgets {
		if b.ID == id {
			return b
		}
	}
	return nil
}

func budgetJSON(b *fakeLlmTokenBudget) map[string]any {
	thresholds := b.AlertThresholds
	if thresholds == nil {
		thresholds = []int64{}
	}
	return map[string]any{
		"id":                    b.ID,
		"org_id":                fakeLlmOrgID,
		"name":                  b.Name,
		"scope_type":            b.ScopeType,
		"scope_id":              b.ScopeID,
		"scope_value":           b.ScopeValue,
		"member_of_group":       b.MemberOfGroup,
		"period":                b.Period,
		"token_limit":           b.TokenLimit,
		"alert_thresholds":      thresholds,
		"action_on_exhaust":     b.ActionOnExhaust,
		"traffic_type":          b.TrafficType,
		"enabled":               b.Enabled,
		"cost_limit":            b.CostLimit,
		"currency":              b.Currency,
		"target_provider_id":    b.Targets.ProviderID,
		"target_upstream_model": b.Targets.UpstreamModel,
		"target_model_alias":    b.Targets.ModelAlias,
		"target_mcp_server_id":  b.Targets.McpServerID,
		"last_change_note":      b.LastChangeNote,
		"created_at":            fakeLlmTime,
	}
}

// budgetScopeTaken mirrors the token_budgets_scope_unique_idx
// (org, scope_type, scope_id, scope_value, traffic_type, period,
// member_of_group, the four targets — V73).
func (f *fakeLlmGatewayServer) budgetScopeTaken(scopeType string, scopeID, scopeValue *string, trafficType, period string, memberOfGroup *string, targets fakeLlmTargets, excludeID string) bool {
	for _, b := range f.budgets {
		if b.ID == excludeID {
			continue
		}
		if b.ScopeType == scopeType && strPtrEq(b.ScopeID, scopeID) &&
			strPtrEq(b.ScopeValue, scopeValue) && b.TrafficType == trafficType && b.Period == period &&
			strPtrEq(b.MemberOfGroup, memberOfGroup) && b.Targets.eq(targets) {
			return true
		}
	}
	return false
}

func (f *fakeLlmGatewayServer) handleBudgets(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/llm-gateway/admin/budgets"), "/")

	switch {
	case id == "" && r.Method == http.MethodPost:
		f.createBudget(w, r)
	case id == "" && r.Method == http.MethodGet:
		items := make([]map[string]any, 0, len(f.budgets))
		for _, b := range f.budgets {
			items = append(items, budgetJSON(b))
		}
		_ = json.NewEncoder(w).Encode(items)
	case id != "" && id != "status" && r.Method == http.MethodPut:
		f.updateBudget(w, r, id)
	case id != "" && r.Method == http.MethodDelete:
		for i, b := range f.budgets {
			if b.ID == id {
				f.budgets = append(f.budgets[:i], f.budgets[i+1:]...)
				_ = json.NewEncoder(w).Encode(map[string]any{"deleted": true})
				return
			}
		}
		writeLlmError(w, http.StatusNotFound, fmt.Sprintf("token budget {id: %s} not found", id))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeLlmGatewayServer) createBudget(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name            string   `json:"name"`
		ScopeType       string   `json:"scope_type"`
		ScopeID         *string  `json:"scope_id"`
		ScopeValue      *string  `json:"scope_value"`
		MemberOfGroup   *string  `json:"member_of_group"`
		Period          string   `json:"period"`
		TokenLimit      *int64   `json:"token_limit"`
		CostLimit       *float64 `json:"cost_limit"`
		Currency        *string  `json:"currency"`
		AlertThresholds []int64  `json:"alert_thresholds"`
		ActionOnExhaust *string  `json:"action_on_exhaust"`
		TrafficType     *string  `json:"traffic_type"`
		ChangeNote      *string  `json:"change_note"`
		fakeLlmTargets
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeLlmError(w, http.StatusBadRequest, err.Error())
		return
	}
	// token_limit is a required i64 in CreateBudgetRequest: a missing key is
	// an axum Json rejection (plain-text 422), not a defaulted 0.
	if body.TokenLimit == nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte("Failed to deserialize the JSON body into the target type: missing field `token_limit`"))
		return
	}
	tokenLimit := *body.TokenLimit
	if tokenLimit < 0 {
		writeLlmError(w, http.StatusBadRequest, "token_limit must be non-negative")
		return
	}
	if tokenLimit == 0 && (body.CostLimit == nil || *body.CostLimit <= 0) {
		writeLlmError(w, http.StatusBadRequest,
			"At least one limit is required: token_limit or cost_limit")
		return
	}
	if !slices.Contains([]string{"daily", "weekly", "monthly"}, body.Period) {
		writeLlmError(w, http.StatusBadRequest, "invalid period: "+body.Period)
		return
	}
	group, ok := fakeMemberOfGroupShape(w, body.MemberOfGroup, body.ScopeType, body.ScopeID, body.ScopeValue)
	if !ok {
		return
	}

	thresholds := body.AlertThresholds
	if thresholds == nil {
		thresholds = []int64{80, 90}
	}
	action := "block"
	if body.ActionOnExhaust != nil {
		action = *body.ActionOnExhaust
	}
	trafficType := "all"
	if body.TrafficType != nil {
		trafficType = *body.TrafficType
	}
	if !validLlmTargetShape(w, body.fakeLlmTargets, trafficType) {
		return
	}
	note, ok := fakeLlmChangeNote(w, body.ChangeNote)
	if !ok {
		return
	}
	currency := "USD"
	if body.Currency != nil {
		currency = *body.Currency
	}
	var costLimit *float64
	if body.CostLimit != nil {
		c := fakeLlmDecimal4(*body.CostLimit)
		costLimit = &c
	}

	if f.budgetScopeTaken(body.ScopeType, body.ScopeID, body.ScopeValue, trafficType, body.Period, group,
		body.fakeLlmTargets, "") {
		writeLlmError(w, http.StatusConflict,
			"A budget with this scope, target, traffic type, and period already exists. "+
				"Edit the existing one, or vary the scope, target, traffic type, or period.")
		return
	}

	b := &fakeLlmTokenBudget{
		ID:              f.newID("eeee"),
		Name:            body.Name,
		ScopeType:       body.ScopeType,
		ScopeID:         body.ScopeID,
		ScopeValue:      body.ScopeValue,
		MemberOfGroup:   group,
		Period:          body.Period,
		TokenLimit:      tokenLimit,
		CostLimit:       costLimit,
		Currency:        currency,
		AlertThresholds: thresholds,
		ActionOnExhaust: action,
		TrafficType:     trafficType,
		Enabled:         true, // create has no enabled field
		Targets:         body.fakeLlmTargets,
		LastChangeNote:  note,
	}
	f.budgets = append(f.budgets, b)
	_ = json.NewEncoder(w).Encode(budgetJSON(b))
}

// updateBudget applies production's COALESCE semantics: present non-null
// keys overwrite, everything else keeps, and cost_limit 0 clears. The scope,
// currency and target columns are not part of the update contract at all,
// and — like production — nothing but the change note is validated: a
// traffic_type that contradicts a stored target trips the V54/V73 CHECK,
// which surfaces as a 500.
func (f *fakeLlmGatewayServer) updateBudget(w http.ResponseWriter, r *http.Request, id string) {
	b := f.findBudget(id)
	if b == nil {
		writeLlmError(w, http.StatusNotFound, fmt.Sprintf("token budget {id: %s} not found", id))
		return
	}

	var body struct {
		Name            *string  `json:"name"`
		TokenLimit      *int64   `json:"token_limit"`
		AlertThresholds []int64  `json:"alert_thresholds"`
		Enabled         *bool    `json:"enabled"`
		Period          *string  `json:"period"`
		ActionOnExhaust *string  `json:"action_on_exhaust"`
		TrafficType     *string  `json:"traffic_type"`
		CostLimit       *float64 `json:"cost_limit"`
		ChangeNote      *string  `json:"change_note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeLlmError(w, http.StatusBadRequest, err.Error())
		return
	}
	note, ok := fakeLlmChangeNote(w, body.ChangeNote)
	if !ok {
		return
	}

	updated := *b
	updated.LastChangeNote = note
	if body.CostLimit != nil {
		if *body.CostLimit == 0 {
			updated.CostLimit = nil
		} else {
			c := fakeLlmDecimal4(*body.CostLimit)
			updated.CostLimit = &c
		}
	}
	if body.Name != nil {
		updated.Name = *body.Name
	}
	if body.TokenLimit != nil {
		updated.TokenLimit = *body.TokenLimit
	}
	if body.AlertThresholds != nil {
		updated.AlertThresholds = body.AlertThresholds
	}
	if body.Enabled != nil {
		updated.Enabled = *body.Enabled
	}
	if body.Period != nil {
		updated.Period = *body.Period
	}
	if body.ActionOnExhaust != nil {
		updated.ActionOnExhaust = *body.ActionOnExhaust
	}
	if body.TrafficType != nil {
		updated.TrafficType = *body.TrafficType
	}
	if !validLlmTargetShape(httptest.NewRecorder(), updated.Targets, updated.TrafficType) {
		writeLlmError(w, http.StatusInternalServerError,
			"error returned from database: new row for relation \"token_budgets\" violates check constraint")
		return
	}

	if f.budgetScopeTaken(updated.ScopeType, updated.ScopeID, updated.ScopeValue,
		updated.TrafficType, updated.Period, updated.MemberOfGroup, updated.Targets, b.ID) {
		writeLlmError(w, http.StatusConflict,
			"A budget with this scope, target, traffic type, and period already exists. "+
				"Edit the existing one, or vary the scope, target, traffic type, or period.")
		return
	}

	*b = updated
	_ = json.NewEncoder(w).Encode(budgetJSON(b))
}

// markBudgetDeleted removes a stored budget out-of-band.
func (f *fakeLlmGatewayServer) markBudgetDeleted(t *testing.T, id string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, b := range f.budgets {
		if b.ID == id {
			f.budgets = append(f.budgets[:i], f.budgets[i+1:]...)
			return
		}
	}
	t.Fatalf("fake has no token budget %q to delete", id)
}

// checkAllLlmBudgetsDeleted is the CheckDestroy for token-budget tests.
func checkAllLlmBudgetsDeleted(fake *fakeLlmGatewayServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, b := range fake.budgets {
			return fmt.Errorf("token budget %s (%s) was not deleted on destroy", b.ID, b.Name)
		}
		return nil
	}
}

// --- governance config -----------------------------------------------------------

// handleGovernanceConfig emulates the governance-config singleton: GET
// reports the stored row or the column defaults when no row exists, PUT
// upserts the full body (the endpoint has no partial-update semantics — a
// missing field is an axum Json rejection, answered as plain-text 422).
func (f *fakeLlmGatewayServer) handleGovernanceConfig(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.forbidden {
		writeLlmError(w, http.StatusForbidden, "principal is not permitted to perform this action")
		return
	}

	switch r.Method {
	case http.MethodGet:
		_ = json.NewEncoder(w).Encode(f.governanceJSON())
	case http.MethodPut:
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			http.Error(w, "Failed to parse the request body as JSON: "+err.Error(),
				http.StatusUnprocessableEntity)
			return
		}
		var val bool
		field, ok := raw["require_pricing_for_mappings"]
		if !ok || json.Unmarshal(field, &val) != nil {
			http.Error(w, "Failed to deserialize the JSON body into the target type: "+
				"missing field `require_pricing_for_mappings`", http.StatusUnprocessableEntity)
			return
		}
		// The later fields are #[serde(default)]: an omitted key takes the
		// default rather than keeping the stored value, and the row is
		// upserted whole.
		posture, routing := "allow", false
		if v, ok := raw["default_model_access"]; ok {
			_ = json.Unmarshal(v, &posture)
		}
		if v, ok := raw["require_routing_policy"]; ok {
			_ = json.Unmarshal(v, &routing)
		}
		if !slices.Contains([]string{"allow", "deny"}, posture) {
			http.Error(w, "Failed to deserialize the JSON body into the target type: "+
				"default_model_access: unknown variant `"+posture+"`", http.StatusUnprocessableEntity)
			return
		}
		f.governance = &val
		f.defaultModelAccess = posture
		f.requireRoutingPolicy = routing
		_ = json.NewEncoder(w).Encode(f.governanceJSON())
	default:
		http.NotFound(w, r)
	}
}

// governanceJSON renders the stored row, or the column defaults when none
// exists. Callers hold f.mu.
func (f *fakeLlmGatewayServer) governanceJSON() map[string]any {
	pricing := false
	if f.governance != nil {
		pricing = *f.governance
	}
	posture := f.defaultModelAccess
	if posture == "" {
		posture = "allow"
	}
	return map[string]any{
		"require_pricing_for_mappings": pricing,
		"default_model_access":         posture,
		"require_routing_policy":       f.requireRoutingPolicy,
	}
}

// setGovernanceValue flips the stored governance flag out-of-band, as if an
// admin changed it in the app.
func (f *fakeLlmGatewayServer) setGovernanceValue(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.governance = &v
}

// setForbidden toggles the simulated Cerbos authorization failure for the
// governance-config and model-pricing families.
func (f *fakeLlmGatewayServer) setForbidden(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forbidden = v
}

// checkLlmGovernanceReset is the CheckDestroy for governance-config tests:
// destroy must have written the platform defaults back.
func checkLlmGovernanceReset(fake *fakeLlmGatewayServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if fake.governance == nil {
			return fmt.Errorf("governance config was never written back on destroy")
		}
		if *fake.governance != llmGovernanceConfigDefaultRequirePricing {
			return fmt.Errorf("governance require_pricing_for_mappings = %t after destroy, want the platform default %t",
				*fake.governance, llmGovernanceConfigDefaultRequirePricing)
		}
		return nil
	}
}

// --- model pricing (versioned) -----------------------------------------------------

// handleModelPricing emulates the endpoints of the versioned pricing store
// that the provider binds: POST (append version, with the production
// change_source computation, skip-on-no-op, model_provider←catalog_slug
// fallback, and the (rule, effective_from) uniqueness), GET history, GET
// version/{id}, PUT {id} (scheduled-version in-place edit, 400 on effective
// rows), and DELETE {id} (cancel-scheduled vs archive-tombstone dispatch).
// The org-bulk endpoints (import-defaults, sync-defaults) and the restore
// path are not bound by the provider and are not emulated.
func (f *fakeLlmGatewayServer) handleModelPricing(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.forbidden {
		writeLlmError(w, http.StatusForbidden, "principal is not permitted to perform this action")
		return
	}

	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/llm-gateway/admin/model-pricing"), "/")

	switch {
	case rest == "" && r.Method == http.MethodPost:
		f.createPricing(w, r)
	case rest == "history" && r.Method == http.MethodGet:
		f.listPricingHistory(w, r)
	case strings.HasPrefix(rest, "version/") && r.Method == http.MethodGet:
		f.getPricingVersion(w, strings.TrimPrefix(rest, "version/"))
	case rest != "" && !strings.Contains(rest, "/") && r.Method == http.MethodPut:
		f.updatePricing(w, r, rest)
	case rest != "" && !strings.Contains(rest, "/") && r.Method == http.MethodDelete:
		f.deletePricing(w, rest)
	default:
		http.NotFound(w, r)
	}
}

// pricingProviderKey mirrors the store's COALESCE(model_provider, ”) group
// key.
func pricingProviderKey(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// pricingGroup returns the versions of one logical rule, unordered. Callers
// hold f.mu.
func (f *fakeLlmGatewayServer) pricingGroup(pattern, providerKey string) []*fakeLlmPricingVersion {
	var group []*fakeLlmPricingVersion
	for _, v := range f.pricing {
		if v.ModelPattern == pattern && pricingProviderKey(v.ModelProvider) == providerKey {
			group = append(group, v)
		}
	}
	return group
}

// pricingCurrentOf picks the latest version with effective_from <= now out
// of a group; nil when every version is future-dated.
func pricingCurrentOf(group []*fakeLlmPricingVersion, now time.Time) *fakeLlmPricingVersion {
	var current *fakeLlmPricingVersion
	for _, v := range group {
		if v.EffectiveFrom.After(now) {
			continue
		}
		if current == nil || v.EffectiveFrom.After(current.EffectiveFrom) {
			current = v
		}
	}
	return current
}

func pricingJSON(v *fakeLlmPricingVersion) map[string]any {
	return map[string]any{
		"id":                                  v.ID,
		"org_id":                              fakeLlmOrgID,
		"model_provider":                      v.ModelProvider,
		"model_pattern":                       v.ModelPattern,
		"input_cost_per_million_tokens":       v.InputCost,
		"output_cost_per_million_tokens":      v.OutputCost,
		"effective_from":                      v.EffectiveFromRaw,
		"provider_id":                         v.ProviderID,
		"provider_name":                       nil,
		"catalog_slug":                        v.CatalogSlug,
		"cache_read_cost_per_million_tokens":  v.CacheRead,
		"cache_write_cost_per_million_tokens": v.CacheWrite,
		"long_context":                        v.LongContext,
		"sync_mode":                           v.SyncMode,
		"change_source":                       v.ChangeSource,
		"change_reason":                       v.ChangeReason,
		"created_by_user_id":                  nil,
		"created_by_email":                    "admin@example.com",
		"is_archived":                         v.IsArchived,
	}
}

// fakeLlmLongContext is a version's long-context tier (BCP-3904): five
// columns that are all set or all null, rendered as one nested object.
type fakeLlmLongContext struct {
	ThresholdPromptTokens int64    `json:"threshold_prompt_tokens"`
	InputCost             float64  `json:"input_cost_per_million_tokens"`
	OutputCost            float64  `json:"output_cost_per_million_tokens"`
	CacheRead             *float64 `json:"cache_read_cost_per_million_tokens"`
	CacheWrite            *float64 `json:"cache_write_cost_per_million_tokens"`
}

func (t *fakeLlmLongContext) eq(o *fakeLlmLongContext) bool {
	if (t == nil) != (o == nil) {
		return false
	}
	return t == nil || (t.ThresholdPromptTokens == o.ThresholdPromptTokens && t.InputCost == o.InputCost &&
		t.OutputCost == o.OutputCost && floatPtrEq(t.CacheRead, o.CacheRead) && floatPtrEq(t.CacheWrite, o.CacheWrite))
}

// validLlmLongContext mirrors LongContextTierRequest::validate.
func validLlmLongContext(w http.ResponseWriter, t *fakeLlmLongContext) bool {
	if t == nil {
		return true
	}
	if t.ThresholdPromptTokens <= 0 {
		writeLlmError(w, http.StatusBadRequest, fmt.Sprintf(
			"long_context.threshold_prompt_tokens must be greater than 0, got %d", t.ThresholdPromptTokens))
		return false
	}
	for name, v := range map[string]*float64{
		"input_cost_per_million_tokens": &t.InputCost, "output_cost_per_million_tokens": &t.OutputCost,
		"cache_read_cost_per_million_tokens": t.CacheRead, "cache_write_cost_per_million_tokens": t.CacheWrite,
	} {
		if v != nil && (math.IsNaN(*v) || *v < 0) {
			writeLlmError(w, http.StatusBadRequest, fmt.Sprintf(
				"long_context.%s must be a number greater than or equal to 0, got %v", name, *v))
			return false
		}
	}
	return true
}

func floatPtrEq(a, b *float64) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

func (f *fakeLlmGatewayServer) createPricing(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ModelProvider *string             `json:"model_provider"`
		ModelPattern  string              `json:"model_pattern"`
		InputCost     float64             `json:"input_cost_per_million_tokens"`
		OutputCost    float64             `json:"output_cost_per_million_tokens"`
		EffectiveFrom *string             `json:"effective_from"`
		ProviderID    *string             `json:"provider_id"`
		CatalogSlug   *string             `json:"catalog_slug"`
		SyncMode      *string             `json:"sync_mode"`
		ChangeReason  *string             `json:"change_reason"`
		CacheRead     *float64            `json:"cache_read_cost_per_million_tokens"`
		CacheWrite    *float64            `json:"cache_write_cost_per_million_tokens"`
		LongContext   *fakeLlmLongContext `json:"long_context"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Failed to parse the request body as JSON: "+err.Error(),
			http.StatusUnprocessableEntity)
		return
	}
	if !validLlmLongContext(w, body.LongContext) {
		return
	}

	// serde default: rows the admin types in by hand are pinned; an invalid
	// value is a deserialization failure.
	syncMode := "pinned"
	if body.SyncMode != nil {
		if !slices.Contains([]string{"tracking", "pinned", "auto"}, *body.SyncMode) {
			http.Error(w, "Failed to deserialize the JSON body into the target type: sync_mode: "+
				"unknown variant `"+*body.SyncMode+"`", http.StatusUnprocessableEntity)
			return
		}
		syncMode = *body.SyncMode
	}

	// Production: model_provider falls back to catalog_slug so manual rules
	// share the import path's group-key semantics.
	modelProvider := body.ModelProvider
	if modelProvider == nil {
		modelProvider = body.CatalogSlug
	}

	now := time.Now().UTC()
	eff := now
	raw := now.Format(time.RFC3339Nano)
	if body.EffectiveFrom != nil {
		t, err := time.Parse(time.RFC3339Nano, *body.EffectiveFrom)
		if err != nil {
			http.Error(w, "Failed to deserialize the JSON body into the target type: effective_from: "+err.Error(),
				http.StatusUnprocessableEntity)
			return
		}
		eff, raw = t, *body.EffectiveFrom
	}
	isFuture := eff.After(now)

	group := f.pricingGroup(body.ModelPattern, pricingProviderKey(modelProvider))
	current := pricingCurrentOf(group, now)

	// Skip-on-no-op: a non-future write matching the current effective
	// version (costs, cache rates, long-context tier, sync mode; not
	// archived) returns the current row without inserting.
	if !isFuture && current != nil && !current.IsArchived &&
		current.InputCost == body.InputCost && current.OutputCost == body.OutputCost &&
		floatPtrEq(current.CacheRead, body.CacheRead) && floatPtrEq(current.CacheWrite, body.CacheWrite) &&
		current.LongContext.eq(body.LongContext) && current.SyncMode == syncMode {
		_ = json.NewEncoder(w).Encode(pricingJSON(current))
		return
	}

	// V34 unique key: no two versions of a rule at the same instant. The
	// production DB error surfaces as a 500.
	for _, v := range group {
		if v.EffectiveFrom.Equal(eff) {
			writeLlmError(w, http.StatusInternalServerError,
				`duplicate key value violates unique constraint "model_pricing_org_pattern_provider_effective_idx"`)
			return
		}
	}

	changeSource := "admin_edit"
	if isFuture {
		changeSource = "admin_schedule"
	} else if len(group) == 0 {
		changeSource = "admin_create"
	}

	v := &fakeLlmPricingVersion{
		ID:            f.newID("ffff"),
		ModelProvider: modelProvider,
		ModelPattern:  body.ModelPattern,
		InputCost:     body.InputCost,
		OutputCost:    body.OutputCost,
		CacheRead:     body.CacheRead,
		CacheWrite:    body.CacheWrite,
		// Like production, a new version takes the tier from the request
		// alone: nothing is inherited from the version it supersedes.
		LongContext:      body.LongContext,
		EffectiveFrom:    eff,
		EffectiveFromRaw: raw,
		ProviderID:       body.ProviderID,
		CatalogSlug:      body.CatalogSlug,
		SyncMode:         syncMode,
		ChangeSource:     changeSource,
		ChangeReason:     body.ChangeReason,
	}
	f.pricing = append(f.pricing, v)
	_ = json.NewEncoder(w).Encode(pricingJSON(v))
}

// listPricingHistory answers GET /model-pricing/history: the rule's full
// version stack, newest effective_from first, including future-dated and
// tombstone rows. An absent/empty model_provider matches provider-unscoped
// rules only (the COALESCE key).
func (f *fakeLlmGatewayServer) listPricingHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pattern := q.Get("model_pattern")
	if pattern == "" {
		// axum Query rejection for the required field.
		http.Error(w, "Failed to deserialize query string: missing field `model_pattern`",
			http.StatusBadRequest)
		return
	}

	group := f.pricingGroup(pattern, q.Get("model_provider"))
	sort.Slice(group, func(i, j int) bool { return group[i].EffectiveFrom.After(group[j].EffectiveFrom) })
	items := make([]map[string]any, 0, len(group))
	for _, v := range group {
		items = append(items, pricingJSON(v))
	}
	_ = json.NewEncoder(w).Encode(items)
}

func (f *fakeLlmGatewayServer) findPricingVersion(id string) *fakeLlmPricingVersion {
	for _, v := range f.pricing {
		if v.ID == id {
			return v
		}
	}
	return nil
}

func (f *fakeLlmGatewayServer) getPricingVersion(w http.ResponseWriter, id string) {
	v := f.findPricingVersion(id)
	if v == nil {
		writeLlmError(w, http.StatusNotFound, fmt.Sprintf("pricing version %s not found in this org", id))
		return
	}
	_ = json.NewEncoder(w).Encode(pricingJSON(v))
}

// updatePricing emulates PUT /model-pricing/{id}: an in-place edit of a
// not-yet-effective scheduled version. Past versions are immutable (400).
// The cache-cost keys carry the double-Option semantics: absent keeps,
// explicit null clears.
func (f *fakeLlmGatewayServer) updatePricing(w http.ResponseWriter, r *http.Request, id string) {
	v := f.findPricingVersion(id)
	if v == nil {
		writeLlmError(w, http.StatusNotFound, fmt.Sprintf("model pricing version %s not found", id))
		return
	}
	if !v.EffectiveFrom.After(time.Now().UTC()) {
		writeLlmError(w, http.StatusBadRequest,
			"Past pricing versions are immutable. Create a new version (POST /admin/model-pricing) to change the current price.")
		return
	}

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		http.Error(w, "Failed to parse the request body as JSON: "+err.Error(),
			http.StatusUnprocessableEntity)
		return
	}

	updated := *v
	if field, ok := raw["input_cost_per_million_tokens"]; ok && string(field) != "null" {
		_ = json.Unmarshal(field, &updated.InputCost)
	}
	if field, ok := raw["output_cost_per_million_tokens"]; ok && string(field) != "null" {
		_ = json.Unmarshal(field, &updated.OutputCost)
	}
	if field, ok := raw["effective_from"]; ok && string(field) != "null" {
		var s string
		if err := json.Unmarshal(field, &s); err == nil {
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				updated.EffectiveFrom, updated.EffectiveFromRaw = t, s
			}
		}
	}
	if field, ok := raw["sync_mode"]; ok && string(field) != "null" {
		var s string
		_ = json.Unmarshal(field, &s)
		if !slices.Contains([]string{"tracking", "pinned", "auto"}, s) {
			http.Error(w, "Failed to deserialize the JSON body into the target type: sync_mode: "+
				"unknown variant `"+s+"`", http.StatusUnprocessableEntity)
			return
		}
		updated.SyncMode = s
	}
	if field, ok := raw["change_reason"]; ok && string(field) != "null" {
		_ = json.Unmarshal(field, &updated.ChangeReason)
	}
	if field, ok := raw["cache_read_cost_per_million_tokens"]; ok {
		if string(field) == "null" {
			updated.CacheRead = nil
		} else {
			_ = json.Unmarshal(field, &updated.CacheRead)
		}
	}
	if field, ok := raw["cache_write_cost_per_million_tokens"]; ok {
		if string(field) == "null" {
			updated.CacheWrite = nil
		} else {
			_ = json.Unmarshal(field, &updated.CacheWrite)
		}
	}
	if field, ok := raw["long_context"]; ok {
		updated.LongContext = nil
		if string(field) != "null" {
			_ = json.Unmarshal(field, &updated.LongContext)
			if !validLlmLongContext(w, updated.LongContext) {
				return
			}
		}
	}

	*v = updated
	_ = json.NewEncoder(w).Encode(pricingJSON(v))
}

// deletePricing emulates the DELETE dispatch: a future-dated version is a
// scheduled change to cancel (that one row is removed); an effective version
// archives the whole rule (pending scheduled versions are cancelled and an
// is_archived tombstone is appended, carrying the current prices forward).
func (f *fakeLlmGatewayServer) deletePricing(w http.ResponseWriter, id string) {
	v := f.findPricingVersion(id)
	if v == nil {
		writeLlmError(w, http.StatusNotFound, fmt.Sprintf("model pricing version %s not found", id))
		return
	}

	now := time.Now().UTC()
	if v.EffectiveFrom.After(now) {
		f.removePricingVersion(id)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"deleted": true, "mode": "cancelled_scheduled", "cancelled_scheduled": 0, "archived": false,
		})
		return
	}

	cancelled, archived := f.archivePricingGroup(v.ModelPattern, pricingProviderKey(v.ModelProvider), now)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"deleted": true, "mode": "archived_rule", "cancelled_scheduled": cancelled, "archived": archived,
	})
}

func (f *fakeLlmGatewayServer) removePricingVersion(id string) {
	for i, v := range f.pricing {
		if v.ID == id {
			f.pricing = append(f.pricing[:i], f.pricing[i+1:]...)
			return
		}
	}
}

// archivePricingGroup cancels pending scheduled versions of a rule and
// appends the tombstone. Archiving an already-archived rule is an idempotent
// no-op (archived = false). Callers hold f.mu.
func (f *fakeLlmGatewayServer) archivePricingGroup(pattern, providerKey string, now time.Time) (cancelled int, archived bool) {
	for _, v := range f.pricingGroup(pattern, providerKey) {
		if v.EffectiveFrom.After(now) {
			f.removePricingVersion(v.ID)
			cancelled++
		}
	}
	current := pricingCurrentOf(f.pricingGroup(pattern, providerKey), now)
	if current == nil || current.IsArchived {
		return cancelled, false
	}
	eff := now
	if !eff.After(current.EffectiveFrom) {
		eff = current.EffectiveFrom.Add(time.Microsecond)
	}
	reason := "Archived by admin"
	tombstone := *current
	tombstone.ID = f.newID("ffff")
	tombstone.EffectiveFrom = eff
	tombstone.EffectiveFromRaw = eff.Format(time.RFC3339Nano)
	tombstone.ChangeSource = "admin_archive"
	tombstone.ChangeReason = &reason
	tombstone.IsArchived = true
	f.pricing = append(f.pricing, &tombstone)
	return cancelled, true
}

// markPricingArchived archives a rule out-of-band, as if an admin clicked
// Archive in the app. providerKey uses the COALESCE semantics ("" for a
// provider-unscoped rule).
func (f *fakeLlmGatewayServer) markPricingArchived(t *testing.T, pattern, providerKey string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pricingGroup(pattern, providerKey)) == 0 {
		t.Fatalf("fake has no pricing rule %q (provider %q) to archive", pattern, providerKey)
	}
	f.archivePricingGroup(pattern, providerKey, time.Now().UTC())
}

// pricingGroupSnapshot returns copies of a rule's versions (newest
// effective_from first) for test assertions.
func (f *fakeLlmGatewayServer) pricingGroupSnapshot(pattern, providerKey string) []fakeLlmPricingVersion {
	f.mu.Lock()
	defer f.mu.Unlock()
	group := f.pricingGroup(pattern, providerKey)
	sort.Slice(group, func(i, j int) bool { return group[i].EffectiveFrom.After(group[j].EffectiveFrom) })
	out := make([]fakeLlmPricingVersion, 0, len(group))
	for _, v := range group {
		out = append(out, *v)
	}
	return out
}

// checkAllLlmPricingArchived is the CheckDestroy for model-pricing tests:
// destroy never hard-deletes, so every rule left in the store must be
// archived — its latest effective version a tombstone, with no pending
// scheduled versions.
func checkAllLlmPricingArchived(fake *fakeLlmGatewayServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		now := time.Now().UTC()
		seen := map[string]bool{}
		for _, v := range fake.pricing {
			key := v.ModelPattern + "\x00" + pricingProviderKey(v.ModelProvider)
			if seen[key] {
				continue
			}
			seen[key] = true
			group := fake.pricingGroup(v.ModelPattern, pricingProviderKey(v.ModelProvider))
			for _, g := range group {
				if g.EffectiveFrom.After(now) {
					return fmt.Errorf("pricing rule %q still has a scheduled version after destroy", v.ModelPattern)
				}
			}
			current := pricingCurrentOf(group, now)
			if current == nil || !current.IsArchived {
				return fmt.Errorf("pricing rule %q was not archived on destroy", v.ModelPattern)
			}
		}
		return nil
	}
}
