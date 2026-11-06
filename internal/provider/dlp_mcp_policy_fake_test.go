// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// --- in-process fake of the dlp-service MCP policy routes -------------------------
//
// fakeDlpMcpServer emulates `/api/dlp/admin/v1/mcp-policies` and
// `/mcp-baseline-policy` faithfully enough to drive real plan/apply cycles:
// deny_unknown_fields request bodies, the LEGACY policy model's 409 on writes,
// one-policy-per-server 409s with their stable `code`, unique and reserved
// names, rule validation (boundaries, scope pairing, mask_format only with
// MASK, known detection engines), rule ids kept when sent back and minted
// otherwise, sorted server lists, and a Baseline policy that is null until
// its first PUT and has no DELETE.

const (
	fakeMcpPolicyIDPrefix = "c0c0c0c0-0000-0000-0000-"
	fakeMcpRuleIDPrefix   = "aaaa0000-0000-0000-0000-"
	fakeMcpBaselineID     = "bbbb0000-0000-0000-0000-000000000001"
)

type fakeMcpPolicy struct {
	ID         string
	Name       string
	IsBaseline bool
	Servers    []string
	Rules      []dlpMcpRulePayload
}

type fakeDlpMcpServer struct {
	mu sync.Mutex

	// policyModel is SERVER_POLICIES unless a test sets LEGACY.
	policyModel string

	nextPolicyID int
	nextRuleID   int
	policies     map[string]*fakeMcpPolicy
	// baseline is nil until its first save.
	baseline *fakeMcpPolicy

	// createdRuleIDs records every rule id the fake minted, so tests can tell
	// a kept rule from a recreated one.
	createdRuleIDs []string
}

func newFakeDlpMcpServer() *fakeDlpMcpServer {
	return &fakeDlpMcpServer{
		policyModel: "SERVER_POLICIES",
		policies:    map[string]*fakeMcpPolicy{},
	}
}

// setupDlpMcpTest starts the fake and points the provider's BARNDOOR_*
// environment at it.
func setupDlpMcpTest(t *testing.T) *fakeDlpMcpServer {
	t.Helper()

	fake := newFakeDlpMcpServer()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	t.Setenv("BARNDOOR_BASE_URL", srv.URL)
	t.Setenv("BARNDOOR_TOKEN_URL", srv.URL+"/token")
	t.Setenv("BARNDOOR_CLIENT_ID", "test-client")
	t.Setenv("BARNDOOR_CLIENT_SECRET", "test-secret")
	t.Setenv("BARNDOOR_ORGANIZATION_ID", fakeDlpOrgID)

	return fake
}

func (f *fakeDlpMcpServer) handler() http.HandlerFunc {
	const prefix = "/api/dlp/admin/v1"
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		switch p := r.URL.Path; {
		case p == "/token":
			writeToken(w)
		case p == prefix+"/mcp-baseline-policy":
			f.handleBaseline(w, r)
		case p == prefix+"/mcp-policies":
			switch r.Method {
			case http.MethodGet:
				f.listPolicies(w)
			case http.MethodPost:
				f.createPolicy(w, r)
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
		case strings.HasPrefix(p, prefix+"/mcp-policies/"):
			id := strings.TrimPrefix(p, prefix+"/mcp-policies/")
			switch r.Method {
			case http.MethodGet:
				f.getPolicy(w, id)
			case http.MethodPut:
				f.updatePolicy(w, r, id)
			case http.MethodDelete:
				f.deletePolicy(w, id)
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
		default:
			http.NotFound(w, r)
		}
	}
}

// --- wire helpers ---------------------------------------------------------------------

// fakeMcpRuleRequest is the request rule shape with optional scopes, so the
// fake can tell an omitted scope (ALL) from an explicit one.
type fakeMcpRuleRequest struct {
	ID                 *string                      `json:"id"`
	Label              *string                      `json:"label"`
	RuntimeBoundary    string                       `json:"runtime_boundary"`
	Action             string                       `json:"action"`
	MaskFormat         *fakeMcpMaskFormatRequest    `json:"mask_format"`
	DryRun             bool                         `json:"dry_run"`
	AgentScope         *dlpMcpAgentScopePayload     `json:"agent_scope"`
	PrincipalScope     *dlpMcpPrincipalScopePayload `json:"principal_scope"`
	DetectionEngineIDs []string                     `json:"detection_engine_ids"`
}

type fakeMcpMaskFormatRequest struct {
	Keep               string  `json:"keep"`
	KeepChars          int64   `json:"keep_chars"`
	MaskChar           *string `json:"mask_char"`
	PreserveSeparators *bool   `json:"preserve_separators"`
}

func decodeStrict(w http.ResponseWriter, r *http.Request, out any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		writeJSONMessage(w, http.StatusUnprocessableEntity, "invalid body: "+err.Error())
		return false
	}
	return true
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fakeMcpPolicyJSON(p *fakeMcpPolicy) map[string]any {
	servers := slices.Clone(p.Servers)
	if servers == nil {
		servers = []string{}
	}
	rules := slices.Clone(p.Rules)
	if rules == nil {
		rules = []dlpMcpRulePayload{}
	}
	return map[string]any{
		"id":             p.ID,
		"name":           p.Name,
		"is_baseline":    p.IsBaseline,
		"mcp_server_ids": servers,
		"rules":          rules,
		"created_by":     "svc-terraform",
		"updated_by":     "svc-terraform",
		"created_at":     fakeDlpTime,
		"updated_at":     fakeDlpTime,
	}
}

// requireServerPolicies refuses writes on a LEGACY org with the production
// 409 message.
func (f *fakeDlpMcpServer) requireServerPolicies(w http.ResponseWriter) bool {
	if f.policyModel != "SERVER_POLICIES" {
		writeJSONMessage(w, http.StatusConflict,
			"Your organization doesn't use MCP policies yet. Keep editing its Data Protection policies.")
		return false
	}
	return true
}

// convertRules validates request rules against the policy's stored ones and
// returns the rules to store, keeping sent ids and minting the rest.
func (f *fakeDlpMcpServer) convertRules(w http.ResponseWriter, in []fakeMcpRuleRequest, stored []dlpMcpRulePayload, allowIDs bool) ([]dlpMcpRulePayload, bool) {
	if len(in) > dlpMcpMaxRules {
		writeJSONMessage(w, http.StatusUnprocessableEntity, "a policy can hold at most 50 rules")
		return nil, false
	}
	out := make([]dlpMcpRulePayload, 0, len(in))
	seen := map[string]bool{}
	for i, rr := range in {
		n := i + 1
		if !slices.Contains(dlpMcpRuntimeBoundaries, rr.RuntimeBoundary) {
			writeJSONMessage(w, http.StatusUnprocessableEntity, fmt.Sprintf(
				"rule %d: runtime_boundary must be one of: TOOL_INPUT, TOOL_OUTPUT, BOTH; got '%s'", n, rr.RuntimeBoundary))
			return nil, false
		}
		if !validDlpAction(rr.Action) {
			writeJSONMessage(w, http.StatusUnprocessableEntity, fmt.Sprintf("rule %d: unknown action %q", n, rr.Action))
			return nil, false
		}
		if len(rr.DetectionEngineIDs) == 0 {
			writeJSONMessage(w, http.StatusUnprocessableEntity, fmt.Sprintf("rule %d: choose at least one protection profile", n))
			return nil, false
		}
		for _, id := range rr.DetectionEngineIDs {
			if id != fakeDlpDetectionEngineID {
				writeJSONMessage(w, http.StatusNotFound, fmt.Sprintf("detection engine %s not found in org", id))
				return nil, false
			}
		}

		rule := dlpMcpRulePayload{
			Label:              rr.Label,
			RuntimeBoundary:    rr.RuntimeBoundary,
			Action:             rr.Action,
			DryRun:             rr.DryRun,
			AgentScope:         dlpMcpAgentScopePayload{Mode: dlpMcpScopeAll, AgentIDs: []string{}},
			PrincipalScope:     dlpMcpPrincipalScopePayload{Mode: dlpMcpScopeAll, Principals: []dlpPrincipalPayload{}},
			DetectionEngineIDs: rr.DetectionEngineIDs,
		}
		if mf := rr.MaskFormat; mf != nil {
			if rr.Action != dlpMcpMaskAction {
				writeJSONMessage(w, http.StatusUnprocessableEntity,
					fmt.Sprintf("rule %d: mask_format is only valid with POLICY_ACTION_MASK", n))
				return nil, false
			}
			out := &dlpMcpMaskFormatPayload{Keep: mf.Keep, KeepChars: mf.KeepChars, MaskChar: "X", PreserveSeparators: true}
			if mf.MaskChar != nil {
				out.MaskChar = *mf.MaskChar
			}
			if mf.PreserveSeparators != nil {
				out.PreserveSeparators = *mf.PreserveSeparators
			}
			rule.MaskFormat = out
		}
		if s := rr.AgentScope; s != nil {
			if !fakeMcpScopePairingOK(w, n, "agent_scope", s.Mode, len(s.AgentIDs)) {
				return nil, false
			}
			ids := make([]string, 0, len(s.AgentIDs))
			for _, id := range s.AgentIDs {
				ids = append(ids, strings.ToLower(id))
			}
			rule.AgentScope = dlpMcpAgentScopePayload{Mode: s.Mode, AgentIDs: ids}
		}
		if s := rr.PrincipalScope; s != nil {
			if !fakeMcpScopePairingOK(w, n, "principal_scope", s.Mode, len(s.Principals)) {
				return nil, false
			}
			principals := make([]dlpPrincipalPayload, 0, len(s.Principals))
			for _, p := range s.Principals {
				principals = append(principals, dlpPrincipalPayload{
					PrincipalType: p.PrincipalType,
					PrincipalID:   strings.TrimSpace(p.PrincipalID),
				})
			}
			rule.PrincipalScope = dlpMcpPrincipalScopePayload{Mode: s.Mode, Principals: principals}
		}

		if rr.ID != nil {
			if !allowIDs {
				writeJSONMessage(w, http.StatusUnprocessableEntity, "a new policy's rules can't carry ids")
				return nil, false
			}
			if seen[*rr.ID] {
				writeJSONMessage(w, http.StatusUnprocessableEntity, fmt.Sprintf("rule %d: rule id %s is listed twice", n, *rr.ID))
				return nil, false
			}
			if !slices.ContainsFunc(stored, func(s dlpMcpRulePayload) bool { return s.ID != nil && *s.ID == *rr.ID }) {
				writeJSONMessage(w, http.StatusUnprocessableEntity, fmt.Sprintf("rule %d: rule %s is not part of this policy", n, *rr.ID))
				return nil, false
			}
			seen[*rr.ID] = true
			id := *rr.ID
			rule.ID = &id
		} else {
			f.nextRuleID++
			id := fmt.Sprintf("%s%012d", fakeMcpRuleIDPrefix, f.nextRuleID)
			f.createdRuleIDs = append(f.createdRuleIDs, id)
			rule.ID = &id
		}
		out = append(out, rule)
	}
	return out, true
}

func fakeMcpScopePairingOK(w http.ResponseWriter, n int, block, mode string, listed int) bool {
	if !slices.Contains(dlpMcpScopeModes, mode) {
		writeJSONMessage(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("rule %d: %s.mode must be one of: ALL, ONLY, EXCEPT; got '%s'", n, block, mode))
		return false
	}
	if (mode == dlpMcpScopeAll) != (listed == 0) {
		writeJSONMessage(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("rule %d: %s list doesn't fit mode %s", n, block, mode))
		return false
	}
	return true
}

func (f *fakeDlpMcpServer) normalizeServers(w http.ResponseWriter, in []string) ([]string, bool) {
	out := []string{}
	for _, raw := range in {
		id := strings.TrimSpace(raw)
		if id == "" || id == "*" {
			writeJSONMessage(w, http.StatusUnprocessableEntity, "mcp_server_ids lists specific servers")
			return nil, false
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		writeJSONMessage(w, http.StatusUnprocessableEntity, "A policy must cover at least one MCP server.")
		return nil, false
	}
	slices.Sort(out)
	return out, true
}

// refuseConflicts writes the production 409 when another policy owns one of
// servers, or another policy has name.
func (f *fakeDlpMcpServer) refuseConflicts(w http.ResponseWriter, name string, servers []string, exceptID string) bool {
	for _, p := range f.policies {
		if p.ID == exceptID {
			continue
		}
		for _, s := range servers {
			if slices.Contains(p.Servers, s) {
				writeJSONStatus(w, http.StatusConflict, map[string]string{
					"message": fmt.Sprintf("Each MCP server can belong to only one policy. Some of the servers "+
						"you selected already belong to '%s'. Remove them from those policies first.", p.Name),
					"code": dlpMcpServerAlreadyAssignedCode,
				})
				return false
			}
		}
		if strings.EqualFold(p.Name, name) {
			writeJSONMessage(w, http.StatusConflict, fmt.Sprintf("an MCP policy named '%s' already exists", name))
			return false
		}
	}
	return true
}

func fakeMcpValidName(w http.ResponseWriter, name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		writeJSONMessage(w, http.StatusUnprocessableEntity, "name must not be empty")
		return "", false
	}
	if strings.EqualFold(name, dlpMcpBaselineName) {
		writeJSONMessage(w, http.StatusUnprocessableEntity, "'Baseline policy' is reserved for the Baseline policy")
		return "", false
	}
	return name, true
}

// --- handlers ---------------------------------------------------------------------------

func (f *fakeDlpMcpServer) listPolicies(w http.ResponseWriter) {
	items := []map[string]any{}
	if f.baseline != nil {
		items = append(items, fakeMcpPolicyJSON(f.baseline))
	}
	for _, p := range f.policies {
		items = append(items, fakeMcpPolicyJSON(p))
	}
	writeJSONStatus(w, http.StatusOK, map[string]any{"policy_model": f.policyModel, "items": items})
}

func (f *fakeDlpMcpServer) findPolicy(id string) *fakeMcpPolicy {
	if f.baseline != nil && f.baseline.ID == id {
		return f.baseline
	}
	return f.policies[id]
}

func (f *fakeDlpMcpServer) getPolicy(w http.ResponseWriter, id string) {
	p := f.findPolicy(id)
	if p == nil {
		writeJSONMessage(w, http.StatusNotFound, fmt.Sprintf("MCP policy %s not found", id))
		return
	}
	writeJSONStatus(w, http.StatusOK, fakeMcpPolicyJSON(p))
}

func (f *fakeDlpMcpServer) createPolicy(w http.ResponseWriter, r *http.Request) {
	if !f.requireServerPolicies(w) {
		return
	}
	var body struct {
		Name         string               `json:"name"`
		McpServerIDs []string             `json:"mcp_server_ids"`
		Rules        []fakeMcpRuleRequest `json:"rules"`
	}
	if !decodeStrict(w, r, &body) {
		return
	}
	name, ok := fakeMcpValidName(w, body.Name)
	if !ok {
		return
	}
	servers, ok := f.normalizeServers(w, body.McpServerIDs)
	if !ok {
		return
	}
	rules, ok := f.convertRules(w, body.Rules, nil, false)
	if !ok {
		return
	}
	if !f.refuseConflicts(w, name, servers, "") {
		return
	}
	f.nextPolicyID++
	p := &fakeMcpPolicy{
		ID:      fmt.Sprintf("%s%012d", fakeMcpPolicyIDPrefix, f.nextPolicyID),
		Name:    name,
		Servers: servers,
		Rules:   rules,
	}
	f.policies[p.ID] = p
	writeJSONStatus(w, http.StatusCreated, fakeMcpPolicyJSON(p))
}

func (f *fakeDlpMcpServer) updatePolicy(w http.ResponseWriter, r *http.Request, id string) {
	if !f.requireServerPolicies(w) {
		return
	}
	var body struct {
		Name         *string               `json:"name"`
		McpServerIDs *[]string             `json:"mcp_server_ids"`
		Rules        *[]fakeMcpRuleRequest `json:"rules"`
	}
	if !decodeStrict(w, r, &body) {
		return
	}
	p := f.findPolicy(id)
	if p == nil {
		writeJSONMessage(w, http.StatusNotFound, fmt.Sprintf("MCP policy %s not found", id))
		return
	}
	if p.IsBaseline {
		writeJSONMessage(w, http.StatusUnprocessableEntity, "Edit the Baseline policy through /mcp-baseline-policy")
		return
	}
	name := p.Name
	if body.Name != nil {
		var ok bool
		if name, ok = fakeMcpValidName(w, *body.Name); !ok {
			return
		}
	}
	servers := p.Servers
	if body.McpServerIDs != nil {
		var ok bool
		if servers, ok = f.normalizeServers(w, *body.McpServerIDs); !ok {
			return
		}
	}
	rules := p.Rules
	if body.Rules != nil {
		var ok bool
		if rules, ok = f.convertRules(w, *body.Rules, p.Rules, true); !ok {
			return
		}
	}
	if !f.refuseConflicts(w, name, servers, id) {
		return
	}
	p.Name, p.Servers, p.Rules = name, servers, rules
	writeJSONStatus(w, http.StatusOK, fakeMcpPolicyJSON(p))
}

func (f *fakeDlpMcpServer) deletePolicy(w http.ResponseWriter, id string) {
	if !f.requireServerPolicies(w) {
		return
	}
	if f.baseline != nil && f.baseline.ID == id {
		writeJSONMessage(w, http.StatusConflict, "The Baseline policy can't be deleted")
		return
	}
	if _, ok := f.policies[id]; !ok {
		writeJSONMessage(w, http.StatusNotFound, fmt.Sprintf("MCP policy %s not found", id))
		return
	}
	delete(f.policies, id)
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeDlpMcpServer) handleBaseline(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		var policy any
		if f.baseline != nil {
			policy = fakeMcpPolicyJSON(f.baseline)
		}
		writeJSONStatus(w, http.StatusOK, map[string]any{"policy_model": f.policyModel, "policy": policy})
	case http.MethodPut:
		if !f.requireServerPolicies(w) {
			return
		}
		var body struct {
			Rules *[]fakeMcpRuleRequest `json:"rules"`
		}
		if !decodeStrict(w, r, &body) {
			return
		}
		if body.Rules == nil {
			writeJSONMessage(w, http.StatusUnprocessableEntity, "missing field `rules`")
			return
		}
		var stored []dlpMcpRulePayload
		if f.baseline != nil {
			stored = f.baseline.Rules
		}
		rules, ok := f.convertRules(w, *body.Rules, stored, true)
		if !ok {
			return
		}
		if f.baseline == nil {
			f.baseline = &fakeMcpPolicy{ID: fakeMcpBaselineID, Name: dlpMcpBaselineName, IsBaseline: true}
		}
		f.baseline.Rules = rules
		writeJSONStatus(w, http.StatusOK, fakeMcpPolicyJSON(f.baseline))
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// --- test helpers -----------------------------------------------------------------------

// seedBaseline stores a Baseline policy as if it had been saved in the app.
func (f *fakeDlpMcpServer) seedBaseline(rules ...dlpMcpRulePayload) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range rules {
		f.nextRuleID++
		id := fmt.Sprintf("%s%012d", fakeMcpRuleIDPrefix, f.nextRuleID)
		rules[i].ID = &id
	}
	f.baseline = &fakeMcpPolicy{ID: fakeMcpBaselineID, Name: dlpMcpBaselineName, IsBaseline: true, Rules: rules}
}

func (f *fakeDlpMcpServer) setPolicyModel(model string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.policyModel = model
}

func (f *fakeDlpMcpServer) markPolicyDeleted(t *testing.T, id string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.policies[id]; !ok {
		t.Fatalf("fake: no MCP policy %s to delete", id)
	}
	delete(f.policies, id)
}

func (f *fakeDlpMcpServer) ruleIDs(policyID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.findPolicy(policyID)
	if p == nil {
		return nil
	}
	ids := make([]string, 0, len(p.Rules))
	for _, r := range p.Rules {
		ids = append(ids, *r.ID)
	}
	return ids
}

func (f *fakeDlpMcpServer) mintedRuleCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.createdRuleIDs)
}

func checkAllDlpMcpPoliciesDeleted(fake *fakeDlpMcpServer) func(*terraform.State) error {
	return func(*terraform.State) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if n := len(fake.policies); n != 0 {
			return fmt.Errorf("%d MCP policies remain after destroy", n)
		}
		return nil
	}
}

func checkDlpMcpBaselineCleared(fake *fakeDlpMcpServer) func(*terraform.State) error {
	return func(*terraform.State) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if fake.baseline == nil {
			return fmt.Errorf("the Baseline policy should still exist after destroy (there is no DELETE)")
		}
		if n := len(fake.baseline.Rules); n != 0 {
			return fmt.Errorf("the Baseline policy still has %d rules after destroy", n)
		}
		return nil
	}
}
