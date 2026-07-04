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
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// --- in-process fake dlp-service ----------------------------------------------
//
// fakeDlpServer emulates the dlp-service tenant admin REST surface the
// provider binds (`/api/dlp/admin/v1/config|enforcement-policies|allow-list|
// custom-detection-types|field-control-policies`) faithfully enough to drive
// real plan/apply cycles: the COALESCE-upsert org config (GET auto-creates),
// presence-based partial updates with deny_unknown_fields on enforcement and
// field-control policies, server-assigned priorities, target-shape
// validation, the offset-token pagination envelope of the allow list, the
// generated detection-type wire names and value trimming of custom detection
// types, and the POST-as-upsert (always 201, version bump) plus 409
// rules-shape validation of field control policies.

// fakeDlpOrgID matches the BARNDOOR_ORGANIZATION_ID set by setupDlpTest.
const fakeDlpOrgID = "org-123"

// fakeDlpDetectionEngineID is the one detection engine the fake org owns;
// enforcement policies referencing any other engine id get the production
// 404.
const fakeDlpDetectionEngineID = "dddd0000-0000-0000-0000-000000000001"

// fakeDlpAllowListPageCap clamps the page size the allow-list listing serves,
// so tests with a handful of entries still exercise the provider's
// multi-page walk (the provider must treat page tokens as opaque).
const fakeDlpAllowListPageCap = 2

const fakeDlpTime = "2026-07-02T00:00:00Z"

type fakeDlpOrgConfig struct {
	Enabled      bool
	GlobalDryRun bool
}

type fakeDlpEnforcementPolicy struct {
	ID                 string
	Name               string
	TargetKind         string
	ProviderIDs        []string
	ModelAlias         *string
	RuntimeStage       string
	Action             string
	Priority           int64
	DryRun             bool
	McpTargets         []dlpMcpTargetPayload
	Principals         []dlpPrincipalPayload
	DetectionEngineIDs []string
}

type fakeDlpAllowListEntry struct {
	ID             string
	Pattern        string
	PatternType    string
	DetectionTypes []string
	Reason         string
}

type fakeDlpCustomDetectionType struct {
	ID                string
	DetectionType     string
	Name              string
	Description       string
	Patterns          []dlpCustomDetectionPatternPayload
	DefaultSeverity   string
	DefaultConfidence string
}

type fakeDlpFieldControlPolicy struct {
	ID          string
	McpServerID string
	Name        string
	Enabled     bool
	Version     int64
	Rules       json.RawMessage
}

type fakeDlpServer struct {
	mu sync.Mutex

	// orgConfig is nil until first touched, mirroring the lazily-created row.
	orgConfig *fakeDlpOrgConfig

	nextPolicyID int
	policies     map[string]*fakeDlpEnforcementPolicy

	nextEntryID int
	// allowList keeps insertion order for stable offset pagination.
	allowList []*fakeDlpAllowListEntry

	nextDetectionTypeID int
	detectionTypes      map[string]*fakeDlpCustomDetectionType

	nextFieldPolicyID int
	// fieldPolicies keeps insertion order for a stable listing.
	fieldPolicies []*fakeDlpFieldControlPolicy
}

func newFakeDlpServer() *fakeDlpServer {
	return &fakeDlpServer{
		policies:       map[string]*fakeDlpEnforcementPolicy{},
		detectionTypes: map[string]*fakeDlpCustomDetectionType{},
	}
}

func (f *fakeDlpServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			writeToken(w)
		case r.URL.Path == "/api/dlp/admin/v1/config":
			f.handleConfig(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/dlp/admin/v1/enforcement-policies"):
			f.handleEnforcementPolicies(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/dlp/admin/v1/allow-list"):
			f.handleAllowList(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/dlp/admin/v1/custom-detection-types"):
			f.handleCustomDetectionTypes(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/dlp/admin/v1/field-control-policies"):
			f.handleFieldControlPolicies(w, r)
		default:
			http.NotFound(w, r)
		}
	}
}

// setupDlpTest starts the fake dlp-service and points the provider's
// BARNDOOR_* environment at it. REST traffic and token minting ride the same
// httptest server, exactly like production shares one platform host.
func setupDlpTest(t *testing.T) *fakeDlpServer {
	t.Helper()

	fake := newFakeDlpServer()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	t.Setenv("BARNDOOR_BASE_URL", srv.URL)
	t.Setenv("BARNDOOR_TOKEN_URL", srv.URL+"/token")
	t.Setenv("BARNDOOR_CLIENT_ID", "test-client")
	t.Setenv("BARNDOOR_CLIENT_SECRET", "test-secret")
	t.Setenv("BARNDOOR_ORGANIZATION_ID", fakeDlpOrgID)

	return fake
}

// --- org config ----------------------------------------------------------------

// currentOrgConfig returns the config row, lazily creating it with the
// platform defaults like production's GET does. Callers hold f.mu.
func (f *fakeDlpServer) currentOrgConfig() *fakeDlpOrgConfig {
	if f.orgConfig == nil {
		f.orgConfig = &fakeDlpOrgConfig{Enabled: true, GlobalDryRun: false}
	}
	return f.orgConfig
}

func (f *fakeDlpServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.Method {
	case http.MethodGet:
		f.writeOrgConfig(w)
	case http.MethodPut:
		var body struct {
			Enabled      *bool `json:"enabled"`
			GlobalDryRun *bool `json:"global_dry_run"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONMessage(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		cfg := f.currentOrgConfig()
		if body.Enabled != nil {
			cfg.Enabled = *body.Enabled
		}
		if body.GlobalDryRun != nil {
			cfg.GlobalDryRun = *body.GlobalDryRun
		}
		f.writeOrgConfig(w)
	default:
		http.NotFound(w, r)
	}
}

// writeOrgConfig renders the OrgConfigResponse shape (id == org_id). Callers
// hold f.mu.
func (f *fakeDlpServer) writeOrgConfig(w http.ResponseWriter) {
	cfg := f.currentOrgConfig()
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":             fakeDlpOrgID,
		"org_id":         fakeDlpOrgID,
		"enabled":        cfg.Enabled,
		"global_dry_run": cfg.GlobalDryRun,
		"created_at":     fakeDlpTime,
		"updated_at":     fakeDlpTime,
	})
}

// checkDlpOrgConfigReset is the CheckDestroy for org config tests: destroy
// must have written the platform defaults back.
func checkDlpOrgConfigReset(fake *fakeDlpServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		cfg := fake.currentOrgConfig()
		if !cfg.Enabled || cfg.GlobalDryRun {
			return fmt.Errorf("org config was not reset to the platform defaults on destroy: enabled=%t global_dry_run=%t",
				cfg.Enabled, cfg.GlobalDryRun)
		}
		return nil
	}
}

// --- enforcement policies --------------------------------------------------------

// writeJSONMessage renders the dlp-service error shape ({"message": "..."}).
func writeJSONMessage(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
}

var fakeDlpUpdatePolicyKeys = []string{
	"name", "target_kind", "provider_ids", "model_alias", "runtime_stage",
	"action", "priority", "dry_run", "mcp_targets", "principals", "detection_engine_ids",
}

func (f *fakeDlpServer) handleEnforcementPolicies(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/dlp/admin/v1/enforcement-policies"), "/")

	switch {
	case id == "" && r.Method == http.MethodPost:
		f.createPolicy(w, r)
	case id == "" && r.Method == http.MethodGet:
		items := make([]map[string]any, 0, len(f.policies))
		for _, p := range f.policies {
			items = append(items, policyJSON(p))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	case id != "" && r.Method == http.MethodGet:
		p, ok := f.policies[id]
		if !ok {
			writeJSONMessage(w, http.StatusNotFound, fmt.Sprintf("enforcement policy %s not found", id))
			return
		}
		_ = json.NewEncoder(w).Encode(policyJSON(p))
	case id != "" && r.Method == http.MethodPut:
		f.updatePolicy(w, r, id)
	case id != "" && r.Method == http.MethodDelete:
		if _, ok := f.policies[id]; !ok {
			writeJSONMessage(w, http.StatusNotFound, fmt.Sprintf("enforcement policy %s not found", id))
			return
		}
		delete(f.policies, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func policyJSON(p *fakeDlpEnforcementPolicy) map[string]any {
	mcpTargets := make([]map[string]any, 0, len(p.McpTargets))
	for i, t := range p.McpTargets {
		mcpTargets = append(mcpTargets, map[string]any{
			// Row ids regenerate on every update, like production's
			// delete-and-reinsert; the provider must not track them.
			"id":            fmt.Sprintf("aaaa0000-0000-0000-0000-%012d", i+1),
			"mcp_server_id": t.McpServerID,
			"direction":     t.Direction,
		})
	}
	principals := make([]map[string]any, 0, len(p.Principals))
	for i, pr := range p.Principals {
		principals = append(principals, map[string]any{
			"id":             fmt.Sprintf("bbbb0000-0000-0000-0000-%012d", i+1),
			"principal_type": pr.PrincipalType,
			"principal_id":   pr.PrincipalID,
		})
	}
	return map[string]any{
		"id":                   p.ID,
		"org_id":               fakeDlpOrgID,
		"name":                 p.Name,
		"target_kind":          p.TargetKind,
		"provider_ids":         p.ProviderIDs,
		"model_alias":          p.ModelAlias,
		"runtime_stage":        p.RuntimeStage,
		"action":               p.Action,
		"priority":             p.Priority,
		"dry_run":              p.DryRun,
		"mcp_targets":          mcpTargets,
		"principals":           principals,
		"detection_engine_ids": p.DetectionEngineIDs,
		"created_by":           "svc-test",
		"updated_by":           "svc-test",
		"created_at":           fakeDlpTime,
		"updated_at":           fakeDlpTime,
	}
}

func validDlpAction(action string) bool {
	return slices.Contains([]string{
		"POLICY_ACTION_UNSPECIFIED", "POLICY_ACTION_TOKENIZE", "POLICY_ACTION_REDACT",
		"POLICY_ACTION_PASSTHROUGH", "POLICY_ACTION_ALERT_ONLY", "POLICY_ACTION_BLOCK",
		"POLICY_ACTION_MASK", "POLICY_ACTION_OMIT",
	}, action)
}

func validDlpRuntimeStage(stage string) bool {
	return slices.Contains([]string{
		"RUNTIME_STAGE_UNSPECIFIED", "RUNTIME_STAGE_PROMPT", "RUNTIME_STAGE_RESPONSE",
		"RUNTIME_STAGE_TOOL_INPUT", "RUNTIME_STAGE_TOOL_OUTPUT",
	}, stage)
}

// validatePolicyShape mirrors production's validate_enforcement_target_shape
// and companion checks. Callers pass the final (post-merge) field values.
func validatePolicyShape(w http.ResponseWriter, p *fakeDlpEnforcementPolicy) bool {
	if strings.TrimSpace(p.Name) == "" {
		writeJSONMessage(w, http.StatusUnprocessableEntity, "name must not be empty")
		return false
	}
	if !validDlpAction(p.Action) {
		writeJSONMessage(w, http.StatusUnprocessableEntity, "unknown action: "+p.Action)
		return false
	}
	if !validDlpRuntimeStage(p.RuntimeStage) {
		writeJSONMessage(w, http.StatusUnprocessableEntity, "unknown runtime_stage: "+p.RuntimeStage)
		return false
	}
	for _, t := range p.McpTargets {
		if t.McpServerID == "" {
			writeJSONMessage(w, http.StatusUnprocessableEntity, "mcp_server_id must not be empty")
			return false
		}
		if !slices.Contains([]string{"REQUEST", "RESPONSE", "BOTH"}, t.Direction) {
			writeJSONMessage(w, http.StatusUnprocessableEntity,
				fmt.Sprintf("direction must be one of: REQUEST, RESPONSE, BOTH; got '%s'", t.Direction))
			return false
		}
	}
	for _, pr := range p.Principals {
		if !slices.Contains([]string{"GROUP", "ROLE"}, pr.PrincipalType) {
			writeJSONMessage(w, http.StatusUnprocessableEntity,
				fmt.Sprintf("principal_type must be one of: GROUP, ROLE; got '%s'", pr.PrincipalType))
			return false
		}
	}
	switch p.TargetKind {
	case "MODEL_PROVIDER":
		if len(p.McpTargets) > 0 {
			writeJSONMessage(w, http.StatusUnprocessableEntity,
				"MODEL_PROVIDER policies may not include mcp_targets")
			return false
		}
	case "MCP_SERVER":
		if len(p.ProviderIDs) > 0 || p.ModelAlias != nil {
			writeJSONMessage(w, http.StatusUnprocessableEntity,
				"MCP_SERVER policies may not include provider_ids or model_alias")
			return false
		}
	default:
		writeJSONMessage(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("target_kind must be one of: MCP_SERVER, MODEL_PROVIDER; got '%s'", p.TargetKind))
		return false
	}
	if len(p.DetectionEngineIDs) == 0 {
		writeJSONMessage(w, http.StatusUnprocessableEntity, "at least one detection_engine_id is required")
		return false
	}
	for _, engineID := range p.DetectionEngineIDs {
		if engineID != fakeDlpDetectionEngineID {
			writeJSONMessage(w, http.StatusNotFound,
				fmt.Sprintf("detection engine %s not found in org", engineID))
			return false
		}
	}
	return true
}

func (f *fakeDlpServer) policyNameTaken(name, excludeID string) bool {
	for _, p := range f.policies {
		if p.ID != excludeID && p.Name == name {
			return true
		}
	}
	return false
}

// nextPriority mirrors production's COALESCE(MAX(priority), -1) + 1 per
// target kind. Callers hold f.mu.
func (f *fakeDlpServer) nextPriority(targetKind string) int64 {
	next := int64(0)
	for _, p := range f.policies {
		if p.TargetKind == targetKind && p.Priority >= next {
			next = p.Priority + 1
		}
	}
	return next
}

func (f *fakeDlpServer) createPolicy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name               string                `json:"name"`
		TargetKind         *string               `json:"target_kind"`
		ProviderIDs        []string              `json:"provider_ids"`
		ModelAlias         *string               `json:"model_alias"`
		RuntimeStage       *string               `json:"runtime_stage"`
		Action             string                `json:"action"`
		Priority           *int64                `json:"priority"`
		DryRun             bool                  `json:"dry_run"`
		McpTargets         []dlpMcpTargetPayload `json:"mcp_targets"`
		Principals         []dlpPrincipalPayload `json:"principals"`
		DetectionEngineIDs []string              `json:"detection_engine_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONMessage(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	runtimeStage := "RUNTIME_STAGE_UNSPECIFIED"
	if body.RuntimeStage != nil {
		runtimeStage = *body.RuntimeStage
	}
	// Mirror production's target-kind inference when the request omits it.
	targetKind := "MCP_SERVER"
	switch {
	case body.TargetKind != nil:
		targetKind = *body.TargetKind
	case len(body.ProviderIDs) > 0 || body.ModelAlias != nil ||
		runtimeStage == "RUNTIME_STAGE_PROMPT" || runtimeStage == "RUNTIME_STAGE_RESPONSE":
		targetKind = "MODEL_PROVIDER"
	}

	if f.policyNameTaken(body.Name, "") {
		writeJSONMessage(w, http.StatusConflict,
			"enforcement policy with this name or target-kind priority already exists for this org")
		return
	}

	priority := f.nextPriority(targetKind)
	if body.Priority != nil {
		priority = *body.Priority
	}

	f.nextPolicyID++
	p := &fakeDlpEnforcementPolicy{
		ID:                 fmt.Sprintf("eeee0000-0000-0000-0000-%012d", f.nextPolicyID),
		Name:               body.Name,
		TargetKind:         targetKind,
		ProviderIDs:        body.ProviderIDs,
		ModelAlias:         body.ModelAlias,
		RuntimeStage:       runtimeStage,
		Action:             body.Action,
		Priority:           priority,
		DryRun:             body.DryRun,
		McpTargets:         body.McpTargets,
		Principals:         body.Principals,
		DetectionEngineIDs: body.DetectionEngineIDs,
	}
	if p.ProviderIDs == nil {
		p.ProviderIDs = []string{}
	}
	if !validatePolicyShape(w, p) {
		return
	}

	f.policies[p.ID] = p
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(policyJSON(p))
}

// updatePolicy applies presence-based partial-update semantics: only keys in
// the payload change (with model_alias's "" = clear, null = no change quirk),
// and unknown keys are rejected like production's deny_unknown_fields.
func (f *fakeDlpServer) updatePolicy(w http.ResponseWriter, r *http.Request, id string) {
	p, ok := f.policies[id]
	if !ok {
		writeJSONMessage(w, http.StatusNotFound, fmt.Sprintf("enforcement policy %s not found", id))
		return
	}

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeJSONMessage(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	for key := range raw {
		if !slices.Contains(fakeDlpUpdatePolicyKeys, key) {
			writeJSONMessage(w, http.StatusUnprocessableEntity, "unknown field `"+key+"`")
			return
		}
	}

	updated := *p
	updated.ProviderIDs = slices.Clone(p.ProviderIDs)
	updated.McpTargets = slices.Clone(p.McpTargets)
	updated.Principals = slices.Clone(p.Principals)
	updated.DetectionEngineIDs = slices.Clone(p.DetectionEngineIDs)

	if v, ok := raw["name"]; ok {
		_ = json.Unmarshal(v, &updated.Name)
		if f.policyNameTaken(updated.Name, p.ID) {
			writeJSONMessage(w, http.StatusConflict,
				"enforcement policy with this name or target-kind priority already exists for this org")
			return
		}
	}
	if v, ok := raw["target_kind"]; ok {
		_ = json.Unmarshal(v, &updated.TargetKind)
	}
	if v, ok := raw["provider_ids"]; ok {
		_ = json.Unmarshal(v, &updated.ProviderIDs)
		if updated.ProviderIDs == nil {
			updated.ProviderIDs = []string{}
		}
	}
	if v, ok := raw["model_alias"]; ok {
		// Production: JSON null is "no change" (serde's outer Option), and an
		// empty string clears the alias (empty_to_none).
		var alias *string
		_ = json.Unmarshal(v, &alias)
		if alias != nil {
			if *alias == "" {
				updated.ModelAlias = nil
			} else {
				updated.ModelAlias = alias
			}
		}
	}
	if v, ok := raw["runtime_stage"]; ok {
		_ = json.Unmarshal(v, &updated.RuntimeStage)
	}
	if v, ok := raw["action"]; ok {
		_ = json.Unmarshal(v, &updated.Action)
	}
	if v, ok := raw["priority"]; ok {
		_ = json.Unmarshal(v, &updated.Priority)
	}
	if v, ok := raw["dry_run"]; ok {
		_ = json.Unmarshal(v, &updated.DryRun)
	}
	if v, ok := raw["mcp_targets"]; ok {
		updated.McpTargets = nil
		_ = json.Unmarshal(v, &updated.McpTargets)
	}
	if v, ok := raw["principals"]; ok {
		updated.Principals = nil
		_ = json.Unmarshal(v, &updated.Principals)
	}
	if v, ok := raw["detection_engine_ids"]; ok {
		updated.DetectionEngineIDs = nil
		_ = json.Unmarshal(v, &updated.DetectionEngineIDs)
	}

	if !validatePolicyShape(w, &updated) {
		return
	}

	*p = updated
	_ = json.NewEncoder(w).Encode(policyJSON(p))
}

// markPolicyDeleted removes a stored policy out-of-band.
func (f *fakeDlpServer) markPolicyDeleted(t *testing.T, id string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.policies[id]; !ok {
		t.Fatalf("fake has no enforcement policy %q to delete", id)
	}
	delete(f.policies, id)
}

// checkAllDlpPoliciesDeleted is the CheckDestroy for enforcement policy
// tests: destroy must remove every policy the test created.
func checkAllDlpPoliciesDeleted(fake *fakeDlpServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for id, p := range fake.policies {
			return fmt.Errorf("enforcement policy %s (%s) was not deleted on destroy", id, p.Name)
		}
		return nil
	}
}

// --- allow list ------------------------------------------------------------------

func (f *fakeDlpServer) handleAllowList(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/dlp/admin/v1/allow-list"), "/")

	switch {
	case id == "" && r.Method == http.MethodGet:
		f.listAllowList(w, r)
	case id == "" && r.Method == http.MethodPost:
		f.createAllowListEntry(w, r)
	case id != "" && r.Method == http.MethodDelete:
		for i, e := range f.allowList {
			if e.ID == id {
				f.allowList = append(f.allowList[:i], f.allowList[i+1:]...)
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		writeJSONMessage(w, http.StatusNotFound, fmt.Sprintf("allow list entry %s not found", id))
	default:
		http.NotFound(w, r)
	}
}

// listAllowList serves the PaginatedResponse envelope. The page token is an
// opaque offset string; the served page size is capped at
// fakeDlpAllowListPageCap so small tests still cross page boundaries.
func (f *fakeDlpServer) listAllowList(w http.ResponseWriter, r *http.Request) {
	offset := 0
	if tok := r.URL.Query().Get("page_token"); tok != "" {
		offset, _ = strconv.Atoi(strings.TrimPrefix(tok, "off:"))
	}
	limit := fakeDlpAllowListPageCap
	if v := r.URL.Query().Get("page_size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < limit {
			limit = n
		}
	}

	total := len(f.allowList)
	start := min(offset, total)
	end := min(start+limit, total)

	items := make([]map[string]any, 0, end-start)
	for _, e := range f.allowList[start:end] {
		items = append(items, allowListEntryJSON(e))
	}
	nextPageToken := ""
	if end < total {
		nextPageToken = "off:" + strconv.Itoa(end)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"items":           items,
		"next_page_token": nextPageToken,
		"total_count":     total,
	})
}

func allowListEntryJSON(e *fakeDlpAllowListEntry) map[string]any {
	detectionTypes := e.DetectionTypes
	if detectionTypes == nil {
		detectionTypes = []string{}
	}
	return map[string]any{
		"id":              e.ID,
		"org_id":          fakeDlpOrgID,
		"pattern":         e.Pattern,
		"pattern_type":    e.PatternType,
		"detection_types": detectionTypes,
		"reason":          e.Reason,
		"created_by":      "svc-test",
		"created_at":      fakeDlpTime,
	}
}

func (f *fakeDlpServer) createAllowListEntry(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Pattern        string   `json:"pattern"`
		PatternType    string   `json:"pattern_type"`
		DetectionTypes []string `json:"detection_types"`
		Reason         *string  `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONMessage(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if !slices.Contains([]string{"PATTERN_TYPE_LITERAL", "PATTERN_TYPE_REGEX"}, body.PatternType) {
		writeJSONMessage(w, http.StatusUnprocessableEntity, "unknown pattern_type: "+body.PatternType)
		return
	}
	for _, dt := range body.DetectionTypes {
		// The fake accepts the built-in namespace only; production also
		// accepts the org's custom detection type names.
		if !strings.HasPrefix(dt, "DETECTION_TYPE_") {
			writeJSONMessage(w, http.StatusUnprocessableEntity, "unknown detection_type: "+dt)
			return
		}
	}

	reason := ""
	if body.Reason != nil {
		reason = *body.Reason
	}
	f.nextEntryID++
	e := &fakeDlpAllowListEntry{
		ID:             fmt.Sprintf("ffff0000-0000-0000-0000-%012d", f.nextEntryID),
		Pattern:        body.Pattern,
		PatternType:    body.PatternType,
		DetectionTypes: body.DetectionTypes,
		Reason:         reason,
	}
	f.allowList = append(f.allowList, e)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(allowListEntryJSON(e))
}

// markAllowListEntryDeleted removes a stored entry out-of-band.
func (f *fakeDlpServer) markAllowListEntryDeleted(t *testing.T, id string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, e := range f.allowList {
		if e.ID == id {
			f.allowList = append(f.allowList[:i], f.allowList[i+1:]...)
			return
		}
	}
	t.Fatalf("fake has no allow-list entry %q to delete", id)
}

// checkAllDlpAllowListEntriesDeleted is the CheckDestroy for allow-list
// tests: destroy must remove every entry the test created.
func checkAllDlpAllowListEntriesDeleted(fake *fakeDlpServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, e := range fake.allowList {
			return fmt.Errorf("allow-list entry %s (%s) was not deleted on destroy", e.ID, e.Pattern)
		}
		return nil
	}
}

// --- custom detection types --------------------------------------------------------

var fakeDlpUpdateDetectionTypeKeys = []string{
	"name", "description", "patterns", "default_severity", "default_confidence",
}

func (f *fakeDlpServer) handleCustomDetectionTypes(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/dlp/admin/v1/custom-detection-types"), "/")

	switch {
	case id == "" && r.Method == http.MethodPost:
		f.createCustomDetectionType(w, r)
	case id == "" && r.Method == http.MethodGet:
		items := make([]map[string]any, 0, len(f.detectionTypes))
		for _, dt := range f.detectionTypes {
			items = append(items, detectionTypeJSON(dt))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	case id != "" && r.Method == http.MethodGet:
		dt, ok := f.detectionTypes[id]
		if !ok {
			writeJSONMessage(w, http.StatusNotFound, fmt.Sprintf("custom detection type %s not found", id))
			return
		}
		_ = json.NewEncoder(w).Encode(detectionTypeJSON(dt))
	case id != "" && r.Method == http.MethodPut:
		f.updateCustomDetectionType(w, r, id)
	case id != "" && r.Method == http.MethodDelete:
		if _, ok := f.detectionTypes[id]; !ok {
			writeJSONMessage(w, http.StatusNotFound, fmt.Sprintf("custom detection type %s not found", id))
			return
		}
		delete(f.detectionTypes, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func detectionTypeJSON(dt *fakeDlpCustomDetectionType) map[string]any {
	patterns := make([]map[string]any, 0, len(dt.Patterns))
	for _, p := range dt.Patterns {
		patterns = append(patterns, map[string]any{
			"pattern":      p.Pattern,
			"pattern_type": p.PatternType,
		})
	}
	return map[string]any{
		"id":             dt.ID,
		"org_id":         fakeDlpOrgID,
		"detection_type": dt.DetectionType,
		"name":           dt.Name,
		"description":    dt.Description,
		"patterns":       patterns,
		// Production serializes the group under the key `group`.
		"group":              "DETECTION_GROUP_CUSTOM",
		"default_severity":   dt.DefaultSeverity,
		"default_confidence": dt.DefaultConfidence,
		"created_at":         fakeDlpTime,
		"updated_at":         fakeDlpTime,
	}
}

// validateDetectionPatterns mirrors production's
// validate_custom_detection_patterns: at least one pattern, non-empty after
// trimming, a known pattern_type, and compiling regexes.
func validateDetectionPatterns(w http.ResponseWriter, patterns []dlpCustomDetectionPatternPayload) bool {
	if len(patterns) == 0 {
		writeJSONMessage(w, http.StatusUnprocessableEntity, "at least one pattern is required")
		return false
	}
	for _, p := range patterns {
		if strings.TrimSpace(p.Pattern) == "" {
			writeJSONMessage(w, http.StatusUnprocessableEntity, "pattern must not be empty")
			return false
		}
		if !slices.Contains([]string{"PATTERN_TYPE_LITERAL", "PATTERN_TYPE_REGEX"}, p.PatternType) {
			writeJSONMessage(w, http.StatusUnprocessableEntity, "unknown pattern_type: "+p.PatternType)
			return false
		}
		if p.PatternType == "PATTERN_TYPE_REGEX" {
			if _, err := regexp.Compile(strings.TrimSpace(p.Pattern)); err != nil {
				writeJSONMessage(w, http.StatusUnprocessableEntity,
					fmt.Sprintf("invalid regex pattern '%s': %v", p.Pattern, err))
				return false
			}
		}
	}
	return true
}

// validateDetectionConfidence mirrors production's validate_confidence
// (including its quirky "min_confidence" message). Severities are stored
// verbatim without validation, like production.
func validateDetectionConfidence(w http.ResponseWriter, confidence string) bool {
	if !slices.Contains([]string{
		"DETECTION_CONFIDENCE_UNSPECIFIED", "DETECTION_CONFIDENCE_LOW",
		"DETECTION_CONFIDENCE_MEDIUM", "DETECTION_CONFIDENCE_HIGH",
	}, confidence) {
		writeJSONMessage(w, http.StatusUnprocessableEntity, "unknown min_confidence: "+confidence)
		return false
	}
	return true
}

// trimDetectionPatterns mirrors production's per-pattern trimming.
func trimDetectionPatterns(patterns []dlpCustomDetectionPatternPayload) []dlpCustomDetectionPatternPayload {
	out := make([]dlpCustomDetectionPatternPayload, 0, len(patterns))
	for _, p := range patterns {
		out = append(out, dlpCustomDetectionPatternPayload{
			Pattern:     strings.TrimSpace(p.Pattern),
			PatternType: p.PatternType,
		})
	}
	return out
}

func (f *fakeDlpServer) createCustomDetectionType(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name              string                             `json:"name"`
		Description       *string                            `json:"description"`
		Patterns          []dlpCustomDetectionPatternPayload `json:"patterns"`
		DefaultSeverity   *string                            `json:"default_severity"`
		DefaultConfidence *string                            `json:"default_confidence"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONMessage(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeJSONMessage(w, http.StatusUnprocessableEntity, "name must not be empty")
		return
	}
	if !validateDetectionPatterns(w, body.Patterns) {
		return
	}
	if body.DefaultConfidence != nil && !validateDetectionConfidence(w, *body.DefaultConfidence) {
		return
	}

	severity := "DETECTION_SEVERITY_MEDIUM"
	if body.DefaultSeverity != nil {
		severity = *body.DefaultSeverity
	}
	confidence := "DETECTION_CONFIDENCE_MEDIUM"
	if body.DefaultConfidence != nil {
		confidence = *body.DefaultConfidence
	}
	description := ""
	if body.Description != nil {
		description = strings.TrimSpace(*body.Description)
	}

	f.nextDetectionTypeID++
	dt := &fakeDlpCustomDetectionType{
		ID: fmt.Sprintf("cccc0000-0000-0000-0000-%012d", f.nextDetectionTypeID),
		// Production appends a simple-format UUID; a counter is just as opaque.
		DetectionType:     fmt.Sprintf("DETECTION_TYPE_CUSTOM_%032d", f.nextDetectionTypeID),
		Name:              strings.TrimSpace(body.Name),
		Description:       description,
		Patterns:          trimDetectionPatterns(body.Patterns),
		DefaultSeverity:   severity,
		DefaultConfidence: confidence,
	}
	f.detectionTypes[dt.ID] = dt
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(detectionTypeJSON(dt))
}

// updateCustomDetectionType applies presence-based partial-update semantics:
// only keys in the payload change, with description's "" = clear behavior.
func (f *fakeDlpServer) updateCustomDetectionType(w http.ResponseWriter, r *http.Request, id string) {
	dt, ok := f.detectionTypes[id]
	if !ok {
		writeJSONMessage(w, http.StatusNotFound, fmt.Sprintf("custom detection type %s not found", id))
		return
	}

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeJSONMessage(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	// Production has no deny_unknown_fields here, but the provider must only
	// ever send the documented keys — fail the test if it drifts.
	for key := range raw {
		if !slices.Contains(fakeDlpUpdateDetectionTypeKeys, key) {
			writeJSONMessage(w, http.StatusUnprocessableEntity, "unexpected field `"+key+"`")
			return
		}
	}

	updated := *dt
	updated.Patterns = slices.Clone(dt.Patterns)

	if v, ok := raw["name"]; ok {
		_ = json.Unmarshal(v, &updated.Name)
		if strings.TrimSpace(updated.Name) == "" {
			writeJSONMessage(w, http.StatusUnprocessableEntity, "name must not be empty")
			return
		}
		updated.Name = strings.TrimSpace(updated.Name)
	}
	if v, ok := raw["description"]; ok {
		var description string
		_ = json.Unmarshal(v, &description)
		updated.Description = strings.TrimSpace(description)
	}
	if v, ok := raw["patterns"]; ok {
		var patterns []dlpCustomDetectionPatternPayload
		_ = json.Unmarshal(v, &patterns)
		if !validateDetectionPatterns(w, patterns) {
			return
		}
		updated.Patterns = trimDetectionPatterns(patterns)
	}
	if v, ok := raw["default_severity"]; ok {
		_ = json.Unmarshal(v, &updated.DefaultSeverity)
	}
	if v, ok := raw["default_confidence"]; ok {
		_ = json.Unmarshal(v, &updated.DefaultConfidence)
		if !validateDetectionConfidence(w, updated.DefaultConfidence) {
			return
		}
	}

	*dt = updated
	_ = json.NewEncoder(w).Encode(detectionTypeJSON(dt))
}

// markDetectionTypeDeleted removes a stored custom detection type out-of-band.
func (f *fakeDlpServer) markDetectionTypeDeleted(t *testing.T, id string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.detectionTypes[id]; !ok {
		t.Fatalf("fake has no custom detection type %q to delete", id)
	}
	delete(f.detectionTypes, id)
}

// checkAllDlpDetectionTypesDeleted is the CheckDestroy for custom detection
// type tests: destroy must remove every detection type the test created.
func checkAllDlpDetectionTypesDeleted(fake *fakeDlpServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for id, dt := range fake.detectionTypes {
			return fmt.Errorf("custom detection type %s (%s) was not deleted on destroy", id, dt.Name)
		}
		return nil
	}
}

// --- field control policies --------------------------------------------------------

var fakeDlpUpdateFieldPolicyKeys = []string{"name", "enabled", "rules"}

func (f *fakeDlpServer) handleFieldControlPolicies(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/dlp/admin/v1/field-control-policies"), "/")

	switch {
	case id == "" && r.Method == http.MethodGet:
		items := make([]map[string]any, 0, len(f.fieldPolicies))
		for _, p := range f.fieldPolicies {
			items = append(items, fieldPolicyJSON(p))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	case id == "" && r.Method == http.MethodPost:
		f.upsertFieldControlPolicy(w, r)
	case id != "" && r.Method == http.MethodGet:
		p := f.findFieldPolicy(id)
		if p == nil {
			writeJSONMessage(w, http.StatusNotFound, fmt.Sprintf("field control policy %s not found", id))
			return
		}
		_ = json.NewEncoder(w).Encode(fieldPolicyJSON(p))
	case id != "" && r.Method == http.MethodPut:
		f.updateFieldControlPolicy(w, r, id)
	case id != "" && r.Method == http.MethodDelete:
		for i, p := range f.fieldPolicies {
			if p.ID == id {
				f.fieldPolicies = append(f.fieldPolicies[:i], f.fieldPolicies[i+1:]...)
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		writeJSONMessage(w, http.StatusNotFound, fmt.Sprintf("field control policy %s not found", id))
	default:
		http.NotFound(w, r)
	}
}

// findFieldPolicy returns the stored policy with the given id, or nil.
// Callers hold f.mu.
func (f *fakeDlpServer) findFieldPolicy(id string) *fakeDlpFieldControlPolicy {
	for _, p := range f.fieldPolicies {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func fieldPolicyJSON(p *fakeDlpFieldControlPolicy) map[string]any {
	return map[string]any{
		"id":            p.ID,
		"org_id":        fakeDlpOrgID,
		"mcp_server_id": p.McpServerID,
		"name":          p.Name,
		"enabled":       p.Enabled,
		"version":       p.Version,
		"rules":         p.Rules,
		"created_by":    "svc-test",
		"updated_by":    "svc-test",
		"created_at":    fakeDlpTime,
		"updated_at":    fakeDlpTime,
	}
}

// validateFieldControlRules mirrors the leading checks of production's
// store-level validate_field_control_rules, which answers 409 (Conflict) —
// not 422 — for malformed rule contents. The provider treats rule contents as
// opaque JSON, so mirroring the array/object/tool/direction checks is enough
// to exercise its 409 path.
func validateFieldControlRules(w http.ResponseWriter, rules json.RawMessage) bool {
	var items []json.RawMessage
	if err := json.Unmarshal(rules, &items); err != nil || items == nil {
		writeJSONMessage(w, http.StatusConflict, "field control policy rules must be a JSON array")
		return false
	}
	for i, item := range items {
		var rule map[string]json.RawMessage
		if err := json.Unmarshal(item, &rule); err != nil || rule == nil {
			writeJSONMessage(w, http.StatusConflict, fmt.Sprintf("field control rule %d must be an object", i))
			return false
		}
		var tool string
		_ = json.Unmarshal(rule["tool"], &tool)
		if strings.TrimSpace(tool) == "" {
			writeJSONMessage(w, http.StatusConflict, fmt.Sprintf("field control rule %d must specify tool", i))
			return false
		}
		var direction string
		_ = json.Unmarshal(rule["direction"], &direction)
		if direction != "output" {
			writeJSONMessage(w, http.StatusConflict, fmt.Sprintf("field control rule %d must use output direction", i))
			return false
		}
	}
	return true
}

// upsertFieldControlPolicy mirrors production's POST: an INSERT … ON CONFLICT
// (org_id, mcp_server_id) DO UPDATE that bumps version on the update arm and
// answers 201 CREATED either way.
func (f *fakeDlpServer) upsertFieldControlPolicy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		McpServerID string           `json:"mcp_server_id"`
		Name        string           `json:"name"`
		Enabled     *bool            `json:"enabled"`
		Rules       *json.RawMessage `json:"rules"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONMessage(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeJSONMessage(w, http.StatusUnprocessableEntity, "name must not be empty")
		return
	}
	if strings.TrimSpace(body.McpServerID) == "" {
		writeJSONMessage(w, http.StatusUnprocessableEntity, "mcp_server_id must not be empty")
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	rules := json.RawMessage("[]")
	if body.Rules != nil {
		rules = *body.Rules
	}
	if !validateFieldControlRules(w, rules) {
		return
	}

	mcpServerID := strings.TrimSpace(body.McpServerID)
	for _, p := range f.fieldPolicies {
		if p.McpServerID == mcpServerID {
			p.Name = strings.TrimSpace(body.Name)
			p.Enabled = enabled
			p.Rules = rules
			p.Version++
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(fieldPolicyJSON(p))
			return
		}
	}

	f.nextFieldPolicyID++
	p := &fakeDlpFieldControlPolicy{
		ID:          fmt.Sprintf("abab0000-0000-0000-0000-%012d", f.nextFieldPolicyID),
		McpServerID: mcpServerID,
		Name:        strings.TrimSpace(body.Name),
		Enabled:     enabled,
		Version:     1,
		Rules:       rules,
	}
	f.fieldPolicies = append(f.fieldPolicies, p)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(fieldPolicyJSON(p))
}

// updateFieldControlPolicy applies presence-based partial-update semantics
// with deny_unknown_fields, bumping version when any content key is present.
func (f *fakeDlpServer) updateFieldControlPolicy(w http.ResponseWriter, r *http.Request, id string) {
	p := f.findFieldPolicy(id)
	if p == nil {
		writeJSONMessage(w, http.StatusNotFound, fmt.Sprintf("field control policy %s not found", id))
		return
	}

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeJSONMessage(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	for key := range raw {
		if !slices.Contains(fakeDlpUpdateFieldPolicyKeys, key) {
			writeJSONMessage(w, http.StatusUnprocessableEntity, "unknown field `"+key+"`")
			return
		}
	}

	updated := *p
	bump := false
	if v, ok := raw["name"]; ok {
		_ = json.Unmarshal(v, &updated.Name)
		if strings.TrimSpace(updated.Name) == "" {
			writeJSONMessage(w, http.StatusUnprocessableEntity, "name must not be empty")
			return
		}
		updated.Name = strings.TrimSpace(updated.Name)
		bump = true
	}
	if v, ok := raw["enabled"]; ok {
		_ = json.Unmarshal(v, &updated.Enabled)
		bump = true
	}
	if v, ok := raw["rules"]; ok {
		if !validateFieldControlRules(w, v) {
			return
		}
		updated.Rules = v
		bump = true
	}
	if bump {
		updated.Version++
	}

	*p = updated
	_ = json.NewEncoder(w).Encode(fieldPolicyJSON(p))
}

// seedFieldControlPolicy plants a policy out-of-band, as if another actor had
// created it through the API.
func (f *fakeDlpServer) seedFieldControlPolicy(mcpServerID, name string) *fakeDlpFieldControlPolicy {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextFieldPolicyID++
	p := &fakeDlpFieldControlPolicy{
		ID:          fmt.Sprintf("abab0000-0000-0000-0000-%012d", f.nextFieldPolicyID),
		McpServerID: mcpServerID,
		Name:        name,
		Enabled:     true,
		Version:     1,
		Rules:       json.RawMessage("[]"),
	}
	f.fieldPolicies = append(f.fieldPolicies, p)
	return p
}

// markFieldControlPolicyDeleted removes a stored policy out-of-band.
func (f *fakeDlpServer) markFieldControlPolicyDeleted(t *testing.T, id string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, p := range f.fieldPolicies {
		if p.ID == id {
			f.fieldPolicies = append(f.fieldPolicies[:i], f.fieldPolicies[i+1:]...)
			return
		}
	}
	t.Fatalf("fake has no field control policy %q to delete", id)
}

// checkAllDlpFieldControlPoliciesDeleted is the CheckDestroy for field
// control policy tests: destroy must remove every policy the test created.
func checkAllDlpFieldControlPoliciesDeleted(fake *fakeDlpServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, p := range fake.fieldPolicies {
			return fmt.Errorf("field control policy %s (%s) was not deleted on destroy", p.ID, p.Name)
		}
		return nil
	}
}
