// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"encoding/json"
	"fmt"
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
// provider binds (`/api/llm-gateway/admin/providers|model-mappings|
// model-access|rate-limits|budgets|model-pricing|governance-config`)
// faithfully enough to drive real plan/apply cycles: write-only provider
// credentials (stored, never echoed), per-model-provider auth_type
// defaulting, settings-must-be-object validation, the model-mapping
// orphan-alias guard and 1:1 PATCH-or-create upsert with timeout
// materialization, listing-only reads (no get-by-id) for mappings/policies/
// budgets, the rate-limit tri-state metric PATCH, the scope-uniqueness 409s
// of rate limits and budgets, the append-only versioned pricing store
// (scheduled changes, skip-on-no-op, archive tombstones, per-instant
// uniqueness), and the governance-config singleton upsert. Errors use the
// service's OpenAI envelope (`{"error": {"message": ...}}`).

// fakeLlmOrgID matches the BARNDOOR_ORGANIZATION_ID set by setupLlmGatewayTest.
const fakeLlmOrgID = "11111111-2222-3333-4444-555555555555"

const fakeLlmTime = "2026-07-02T00:00:00Z"

// Platform timeout defaults materialized at mapping insert
// (`STREAM_IDLE_TIMEOUT_DEFAULT_SECS` / `TOTAL_REQUEST_TIMEOUT_DEFAULT_SECS`).
const (
	fakeLlmStreamIdleDefault = 30
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
	// Credential is the last api_key written. Stored to let tests assert the
	// write-only round trip; never rendered into a response.
	Credential string
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
}

type fakeLlmModelAccessPolicy struct {
	ID          string
	Name        string
	ScopeType   string
	ScopeID     *string
	ScopeValue  *string
	PolicyType  string
	Targets     []json.RawMessage
	TrafficType string
	Enabled     bool
}

type fakeLlmRateLimit struct {
	ID                string
	Name              string
	ScopeType         string
	ScopeID           *string
	ScopeValue        *string
	RequestsPerMinute *int64
	TokensPerMinute   *int64
	TrafficType       string
	Enabled           bool
}

type fakeLlmTokenBudget struct {
	ID              string
	Name            string
	ScopeType       string
	ScopeID         *string
	ScopeValue      *string
	Period          string
	TokenLimit      int64
	AlertThresholds []int64
	ActionOnExhaust string
	TrafficType     string
	Enabled         bool
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
	providers  []*fakeLlmProvider
	mappings   []*fakeLlmModelMapping
	policies   []*fakeLlmModelAccessPolicy
	rateLimits []*fakeLlmRateLimit
	budgets    []*fakeLlmTokenBudget
	pricing    []*fakeLlmPricingVersion

	// governance is the org's singleton governance_config row; nil means no
	// row yet (the API then reports the column defaults).
	governance *bool

	// forbidden simulates a credential that fails the Cerbos authorize()
	// check every admin handler runs; when set, the governance-config and
	// model-pricing families answer 403.
	forbidden bool
}

func newFakeLlmGatewayServer() *fakeLlmGatewayServer {
	return &fakeLlmGatewayServer{}
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
		case strings.HasPrefix(r.URL.Path, "/api/llm-gateway/admin/model-mappings"):
			f.handleModelMappings(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/llm-gateway/admin/model-access"):
			f.handleModelAccess(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/llm-gateway/admin/rate-limits"):
			f.handleRateLimits(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/llm-gateway/admin/budgets"):
			f.handleBudgets(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/llm-gateway/admin/model-pricing"):
			f.handleModelPricing(w, r)
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
	"openai", "anthropic", "azure_openai", "google_ai", "bedrock", "vertex",
	"groq", "together", "mistral", "cohere", "xai", "fireworks", "perplexity",
	"openrouter", "deepseek", "custom",
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
	return map[string]any{
		"id":             p.ID,
		"org_id":         fakeLlmOrgID,
		"catalog_id":     nil,
		"connection_id":  nil,
		"name":           p.Name,
		"model_provider": p.ModelProvider,
		"auth_type":      p.AuthType,
		"base_url":       p.BaseURL,
		// The stored credential is never part of the response — only the
		// opaque secret-store path it was written to.
		"secret_path":          fmt.Sprintf("orgs/%s/providers/%s", fakeLlmOrgID, p.ID),
		"enabled":              p.Enabled,
		"settings":             p.Settings,
		"created_at":           fakeLlmTime,
		"updated_at":           fakeLlmTime,
		"health_status":        "unverified",
		"enforce_health_check": p.EnforceHealthCheck,
	}
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

func (f *fakeLlmGatewayServer) createProvider(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name               string          `json:"name"`
		ModelProvider      string          `json:"model_provider"`
		AuthType           *string         `json:"auth_type"`
		BaseURL            string          `json:"base_url"`
		APIKey             *string         `json:"api_key"`
		Settings           json.RawMessage `json:"settings"`
		EnforceHealthCheck *bool           `json:"enforce_health_check"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeLlmError(w, http.StatusBadRequest, err.Error())
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
	// Production requires a credential for the direct (no-connection) flow of
	// the API-key auth families.
	authType := fakeLlmDefaultAuthType(body.ModelProvider, body.AuthType)
	credential := ""
	if body.APIKey != nil {
		credential = *body.APIKey
	}
	if credential == "" && (authType == "bearer_api_key" || authType == "x_api_key" || authType == "azure_api_key") {
		writeLlmError(w, http.StatusBadRequest, "api_key is required for API-key auth providers")
		return
	}

	enforce := true
	if body.EnforceHealthCheck != nil {
		enforce = *body.EnforceHealthCheck
	}

	p := &fakeLlmProvider{
		ID:                 f.newID("aaaa"),
		Name:               body.Name,
		ModelProvider:      body.ModelProvider,
		AuthType:           authType,
		BaseURL:            body.BaseURL,
		Settings:           settings,
		Enabled:            true, // create has no enabled field
		EnforceHealthCheck: enforce,
		Credential:         credential,
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
		Enabled            *bool           `json:"enabled"`
		Settings           json.RawMessage `json:"settings"`
		EnforceHealthCheck *bool           `json:"enforce_health_check"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeLlmError(w, http.StatusBadRequest, err.Error())
		return
	}

	updated := *p
	if body.Name != nil {
		updated.Name = *body.Name
	}
	if body.BaseURL != nil {
		updated.BaseURL = *body.BaseURL
	}
	if body.AuthType != nil {
		updated.AuthType = *body.AuthType
	}
	if body.APIKey != nil {
		updated.Credential = *body.APIKey
	}
	if body.Enabled != nil {
		updated.Enabled = *body.Enabled
	}
	if len(body.Settings) > 0 && string(body.Settings) != "null" {
		settings, ok := validLlmSettings(w, body.Settings)
		if !ok {
			return
		}
		updated.Settings = settings
	}
	if body.EnforceHealthCheck != nil {
		updated.EnforceHealthCheck = *body.EnforceHealthCheck
	}

	*p = updated
	_ = json.NewEncoder(w).Encode(providerJSON(p))
}

// seedProvider plants an openai provider out-of-band, as if created in the
// app.
func (f *fakeLlmGatewayServer) seedProvider() *fakeLlmProvider {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := &fakeLlmProvider{
		ID:                 f.newID("aaaa"),
		Name:               "Seeded openai",
		ModelProvider:      "openai",
		AuthType:           fakeLlmDefaultAuthType("openai", nil),
		BaseURL:            "https://upstream.example.com/v1",
		Settings:           json.RawMessage("{}"),
		Enabled:            true,
		EnforceHealthCheck: true,
		Credential:         "seeded-key",
	}
	f.providers = append(f.providers, p)
	return p
}

// providerCredential reads the stored (never-echoed) credential.
func (f *fakeLlmGatewayServer) providerCredential(t *testing.T, id string) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.findProvider(id)
	if p == nil {
		t.Fatalf("fake has no provider %q", id)
	}
	return p.Credential
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
	}
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
	if streamIdle != nil && (*streamIdle < 1 || *streamIdle > 120) {
		writeLlmError(w, http.StatusBadRequest, "stream_idle_timeout_secs must be between 1 and 120")
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
		ProviderID            string `json:"provider_id"`
		ModelAlias            string `json:"model_alias"`
		UpstreamModel         string `json:"upstream_model"`
		Enabled               *bool  `json:"enabled"`
		Priority              int64  `json:"priority"`
		RetryOn429Count       int64  `json:"retry_on_429_count"`
		RetryOn429MaxWaitSecs int64  `json:"retry_on_429_max_wait_secs"`
		BareAlias             *bool  `json:"bare_alias"`
		StreamIdleTimeoutSecs *int64 `json:"stream_idle_timeout_secs"`
		RequestTimeoutSecs    *int64 `json:"request_timeout_secs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeLlmError(w, http.StatusBadRequest, err.Error())
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
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeLlmError(w, http.StatusBadRequest, err.Error())
		return
	}

	updated := *m
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

	*m = updated
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
		"id":           p.ID,
		"org_id":       fakeLlmOrgID,
		"name":         p.Name,
		"scope_type":   p.ScopeType,
		"scope_id":     p.ScopeID,
		"scope_value":  p.ScopeValue,
		"policy_type":  p.PolicyType,
		"targets":      targets,
		"traffic_type": p.TrafficType,
		"enabled":      p.Enabled,
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
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeLlmError(w, http.StatusBadRequest, err.Error())
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
		ID:          f.newID("cccc"),
		Name:        body.Name,
		ScopeType:   body.ScopeType,
		ScopeID:     body.ScopeID,
		ScopeValue:  body.ScopeValue,
		PolicyType:  body.PolicyType,
		Targets:     body.Targets,
		TrafficType: trafficType,
		Enabled:     true, // create has no enabled field
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
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeLlmError(w, http.StatusBadRequest, err.Error())
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
		"id":                  p.ID,
		"org_id":              fakeLlmOrgID,
		"name":                p.Name,
		"scope_type":          p.ScopeType,
		"scope_id":            p.ScopeID,
		"scope_value":         p.ScopeValue,
		"requests_per_minute": p.RequestsPerMinute,
		"tokens_per_minute":   p.TokensPerMinute,
		"traffic_type":        p.TrafficType,
		"enabled":             p.Enabled,
	}
}

// rateLimitScopeTaken mirrors the rate_limit_policies_scope_unique_idx
// (org, scope_type, scope_id, scope_value, traffic_type).
func (f *fakeLlmGatewayServer) rateLimitScopeTaken(scopeType string, scopeID, scopeValue *string, trafficType, excludeID string) bool {
	for _, p := range f.rateLimits {
		if p.ID == excludeID {
			continue
		}
		if p.ScopeType == scopeType && strPtrEq(p.ScopeID, scopeID) &&
			strPtrEq(p.ScopeValue, scopeValue) && p.TrafficType == trafficType {
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
		TrafficType       *string `json:"traffic_type"`
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

	trafficType := "all"
	if body.TrafficType != nil {
		trafficType = *body.TrafficType
	}
	if f.rateLimitScopeTaken(body.ScopeType, body.ScopeID, body.ScopeValue, trafficType, "") {
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
		TrafficType:       trafficType,
		Enabled:           true, // create has no enabled field
	}
	f.rateLimits = append(f.rateLimits, p)
	_ = json.NewEncoder(w).Encode(rateLimitJSON(p))
}

// updateRateLimit applies production's tri-state PATCH semantics on the two
// metrics: an absent key keeps the current value, an explicit null clears it,
// and a number sets it. Everything else is COALESCE.
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
	if f.rateLimitScopeTaken(updated.ScopeType, updated.ScopeID, updated.ScopeValue, updated.TrafficType, p.ID) {
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
		"period":                b.Period,
		"token_limit":           b.TokenLimit,
		"alert_thresholds":      thresholds,
		"action_on_exhaust":     b.ActionOnExhaust,
		"traffic_type":          b.TrafficType,
		"enabled":               b.Enabled,
		"cost_limit":            nil,
		"currency":              "USD",
		"target_provider_id":    nil,
		"target_upstream_model": nil,
		"target_model_alias":    nil,
		"created_at":            fakeLlmTime,
	}
}

// budgetScopeTaken mirrors the token_budgets_scope_unique_idx
// (org, scope_type, scope_id, scope_value, traffic_type, period).
func (f *fakeLlmGatewayServer) budgetScopeTaken(scopeType string, scopeID, scopeValue *string, trafficType, period, excludeID string) bool {
	for _, b := range f.budgets {
		if b.ID == excludeID {
			continue
		}
		if b.ScopeType == scopeType && strPtrEq(b.ScopeID, scopeID) &&
			strPtrEq(b.ScopeValue, scopeValue) && b.TrafficType == trafficType && b.Period == period {
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
		Name            string  `json:"name"`
		ScopeType       string  `json:"scope_type"`
		ScopeID         *string `json:"scope_id"`
		ScopeValue      *string `json:"scope_value"`
		Period          string  `json:"period"`
		TokenLimit      int64   `json:"token_limit"`
		AlertThresholds []int64 `json:"alert_thresholds"`
		ActionOnExhaust *string `json:"action_on_exhaust"`
		TrafficType     *string `json:"traffic_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeLlmError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.TokenLimit < 0 {
		writeLlmError(w, http.StatusBadRequest, "token_limit must be non-negative")
		return
	}
	if body.TokenLimit == 0 {
		writeLlmError(w, http.StatusBadRequest,
			"At least one limit is required: token_limit or cost_limit")
		return
	}
	if !slices.Contains([]string{"daily", "weekly", "monthly"}, body.Period) {
		writeLlmError(w, http.StatusBadRequest, "invalid period: "+body.Period)
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

	if f.budgetScopeTaken(body.ScopeType, body.ScopeID, body.ScopeValue, trafficType, body.Period, "") {
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
		Period:          body.Period,
		TokenLimit:      body.TokenLimit,
		AlertThresholds: thresholds,
		ActionOnExhaust: action,
		TrafficType:     trafficType,
		Enabled:         true, // create has no enabled field
	}
	f.budgets = append(f.budgets, b)
	_ = json.NewEncoder(w).Encode(budgetJSON(b))
}

// updateBudget applies production's COALESCE semantics: present non-null
// keys overwrite, everything else keeps. The scope columns are not part of
// the update contract at all.
func (f *fakeLlmGatewayServer) updateBudget(w http.ResponseWriter, r *http.Request, id string) {
	b := f.findBudget(id)
	if b == nil {
		writeLlmError(w, http.StatusNotFound, fmt.Sprintf("token budget {id: %s} not found", id))
		return
	}

	var body struct {
		Name            *string `json:"name"`
		TokenLimit      *int64  `json:"token_limit"`
		AlertThresholds []int64 `json:"alert_thresholds"`
		Enabled         *bool   `json:"enabled"`
		Period          *string `json:"period"`
		ActionOnExhaust *string `json:"action_on_exhaust"`
		TrafficType     *string `json:"traffic_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeLlmError(w, http.StatusBadRequest, err.Error())
		return
	}

	updated := *b
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

	if f.budgetScopeTaken(updated.ScopeType, updated.ScopeID, updated.ScopeValue,
		updated.TrafficType, updated.Period, b.ID) {
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
		val := false
		if f.governance != nil {
			val = *f.governance
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"require_pricing_for_mappings": val})
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
		f.governance = &val
		_ = json.NewEncoder(w).Encode(map[string]any{"require_pricing_for_mappings": val})
	default:
		http.NotFound(w, r)
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
		"sync_mode":                           v.SyncMode,
		"change_source":                       v.ChangeSource,
		"change_reason":                       v.ChangeReason,
		"created_by_user_id":                  nil,
		"created_by_email":                    "admin@example.com",
		"is_archived":                         v.IsArchived,
	}
}

func floatPtrEq(a, b *float64) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

func (f *fakeLlmGatewayServer) createPricing(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ModelProvider *string  `json:"model_provider"`
		ModelPattern  string   `json:"model_pattern"`
		InputCost     float64  `json:"input_cost_per_million_tokens"`
		OutputCost    float64  `json:"output_cost_per_million_tokens"`
		EffectiveFrom *string  `json:"effective_from"`
		ProviderID    *string  `json:"provider_id"`
		CatalogSlug   *string  `json:"catalog_slug"`
		SyncMode      *string  `json:"sync_mode"`
		ChangeReason  *string  `json:"change_reason"`
		CacheRead     *float64 `json:"cache_read_cost_per_million_tokens"`
		CacheWrite    *float64 `json:"cache_write_cost_per_million_tokens"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Failed to parse the request body as JSON: "+err.Error(),
			http.StatusUnprocessableEntity)
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
	// version (costs, cache rates, sync mode; not archived) returns the
	// current row without inserting.
	if !isFuture && current != nil && !current.IsArchived &&
		current.InputCost == body.InputCost && current.OutputCost == body.OutputCost &&
		floatPtrEq(current.CacheRead, body.CacheRead) && floatPtrEq(current.CacheWrite, body.CacheWrite) &&
		current.SyncMode == syncMode {
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
		ID:               f.newID("ffff"),
		ModelProvider:    modelProvider,
		ModelPattern:     body.ModelPattern,
		InputCost:        body.InputCost,
		OutputCost:       body.OutputCost,
		CacheRead:        body.CacheRead,
		CacheWrite:       body.CacheWrite,
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
