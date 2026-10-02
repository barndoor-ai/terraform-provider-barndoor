// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// --- routing policies and rules (fake llm-gateway) ------------------------------
//
// Mirrors `routes/admin/routing_policies.rs`, `routes/admin/routing_rules.rs`
// and their stores in `db/routing_policies.rs` / `db/routing_rules.rs`:
//
//   - policies: listing-only reads (no get-by-id), a keep-if-omitted PUT merge
//     where JSON null also keeps (no deserialize_with on the
//     Option<Option<String>> fields), the handler's slot-target and
//     alias-shadow reference checks (400) ahead of validate_draft (400), the
//     case-insensitive (org_id, lower(model_alias)) unique index (409), and a
//     hard delete that cascades the policy's rules;
//   - rules: scoped by a required `policy_id` query parameter (400 when
//     missing, 404 for an unknown policy), normalize-then-validate with a flat
//     422 `{"error": "..."}` body, the case-sensitive UNIQUE(policy_id, name)
//     mapped to 422, a full-replace PUT using the draft defaults, advisory
//     conflicts on every write and listing, and a 204 delete.

// fakeLlmRoutingSlot mirrors RoutingSlot. label/description serialize as
// null when absent (no skip_serializing_if).
type fakeLlmRoutingSlot struct {
	Label       *string `json:"label"`
	ModelAlias  string  `json:"model_alias"`
	Description *string `json:"description"`
}

type fakeLlmRoutingPolicy struct {
	ID                   string
	ModelAlias           string
	Description          *string
	Enabled              bool
	DeterminerModelAlias *string
	DeterminerPrompt     *string
	Posture              string
	Slots                []fakeLlmRoutingSlot
	ContextBreakpoints   []int64
	RouterInputMaxChars  int64
	DefaultSlotOnFailure int64
	CreatedAt            string
	UpdatedAt            string
}

type fakeLlmRoutingRule struct {
	ID          string
	PolicyID    string
	Name        string
	Description string
	FloorSlot   *int64
	DenySlots   []int64
	Enabled     bool
}

// fakeLlmUUID is the shape axum's Uuid extractors accept.
var fakeLlmUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// fakeLlmStamp mints a strictly later updated_at per write, like now().
// Callers hold f.mu.
func (f *fakeLlmGatewayServer) fakeLlmStamp() string {
	f.nextID++
	base, _ := time.Parse(time.RFC3339, fakeLlmTime)
	return base.Add(time.Duration(f.nextID) * time.Second).UTC().Format(time.RFC3339)
}

func routingPolicyJSON(p *fakeLlmRoutingPolicy) map[string]any {
	slots := p.Slots
	if slots == nil {
		slots = []fakeLlmRoutingSlot{}
	}
	breakpoints := p.ContextBreakpoints
	if breakpoints == nil {
		breakpoints = []int64{}
	}
	return map[string]any{
		"id":                      p.ID,
		"org_id":                  fakeLlmOrgID,
		"model_alias":             p.ModelAlias,
		"description":             p.Description,
		"enabled":                 p.Enabled,
		"determiner_model_alias":  p.DeterminerModelAlias,
		"determiner_prompt":       p.DeterminerPrompt,
		"posture":                 p.Posture,
		"slots":                   slots,
		"context_breakpoints":     breakpoints,
		"router_input_max_chars":  p.RouterInputMaxChars,
		"default_slot_on_failure": p.DefaultSlotOnFailure,
		"created_at":              p.CreatedAt,
		"updated_at":              p.UpdatedAt,
	}
}

func (f *fakeLlmGatewayServer) findRoutingPolicy(id string) *fakeLlmRoutingPolicy {
	for _, p := range f.routingPolicies {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// fakeLlmResolvesBare mirrors ProviderDb::resolve_model_routes: a bare alias
// resolves through enabled `bare_alias` rows on enabled providers whose 1:1
// anchor for the same (provider, upstream) is enabled. Callers hold f.mu.
func (f *fakeLlmGatewayServer) fakeLlmResolvesBare(alias string) bool {
	for _, m := range f.mappings {
		if m.ModelAlias != alias || !m.BareAlias || !m.Enabled {
			continue
		}
		if p := f.findProvider(m.ProviderID); p == nil || !p.Enabled {
			continue
		}
		for _, anchor := range f.mappings {
			if anchor.ProviderID == m.ProviderID && anchor.UpstreamModel == m.UpstreamModel &&
				anchor.ModelAlias == anchor.UpstreamModel && anchor.Enabled {
				return true
			}
		}
	}
	return false
}

// fakeLlmResolvesTarget mirrors ProviderDb::resolve_routes_for_target: a
// `<provider>/<model>` target reaches only that provider's enabled 1:1
// enablement row (BCP-4086; provider matched by name, case-insensitively);
// anything else resolves as a bare alias. Callers hold f.mu.
func (f *fakeLlmGatewayServer) fakeLlmResolvesTarget(target string) bool {
	providerName, model, ok := strings.Cut(target, "/")
	if !ok || providerName == "" || model == "" {
		return f.fakeLlmResolvesBare(target)
	}
	for _, m := range f.mappings {
		if m.ModelAlias != model || m.ModelAlias != m.UpstreamModel || !m.Enabled {
			continue
		}
		if p := f.findProvider(m.ProviderID); p != nil && p.Enabled && strings.EqualFold(p.Name, providerName) {
			return true
		}
	}
	return false
}

// fakeLlmTargetAlias mirrors target_alias.
func fakeLlmTargetAlias(target string) string {
	if provider, model, ok := strings.Cut(target, "/"); ok && provider != "" && model != "" {
		return model
	}
	return target
}

// validRoutingPolicyRefs mirrors the handler's validate_policy_targets and
// ensure_alias_does_not_shadow_mapping, which run before the store's
// validate_draft. Callers hold f.mu.
func (f *fakeLlmGatewayServer) validRoutingPolicyRefs(w http.ResponseWriter, p *fakeLlmRoutingPolicy) bool {
	for _, slot := range p.Slots {
		if !f.fakeLlmResolvesTarget(slot.ModelAlias) {
			writeLlmError(w, http.StatusBadRequest, fmt.Sprintf("target '%s' has no enabled routes", slot.ModelAlias))
			return false
		}
	}
	if f.fakeLlmResolvesBare(p.ModelAlias) {
		writeLlmError(w, http.StatusBadRequest, fmt.Sprintf(
			"model alias '%s' already resolves via model routes; smart aliases must be distinct", p.ModelAlias))
		return false
	}
	return true
}

// validRoutingPolicyDraft mirrors validate_draft, in its order.
func validRoutingPolicyDraft(w http.ResponseWriter, p *fakeLlmRoutingPolicy) bool {
	fail := func(msg string) bool {
		writeLlmError(w, http.StatusBadRequest, msg)
		return false
	}
	if strings.TrimSpace(p.ModelAlias) == "" {
		return fail("model_alias is required")
	}
	determiner := ""
	if p.DeterminerModelAlias != nil {
		determiner = strings.TrimSpace(*p.DeterminerModelAlias)
	}
	if determiner == "" {
		return fail("determiner_model_alias is required")
	}
	if len(p.Slots) < 2 {
		return fail("at least two model slots are required")
	}
	if len(p.ContextBreakpoints) != len(p.Slots)-1 {
		return fail(fmt.Sprintf("context_breakpoints must have exactly %d entries for %d slots",
			len(p.Slots)-1, len(p.Slots)))
	}
	for i, slot := range p.Slots {
		if strings.TrimSpace(slot.ModelAlias) == "" {
			return fail(fmt.Sprintf("slot %d model_alias is required", i))
		}
		if strings.EqualFold(fakeLlmTargetAlias(slot.ModelAlias), p.ModelAlias) {
			return fail("routing policy alias cannot appear as a slot target")
		}
	}
	for _, b := range p.ContextBreakpoints {
		if b <= 0 {
			return fail("context breakpoints must be positive")
		}
	}
	for i := 1; i < len(p.ContextBreakpoints); i++ {
		if p.ContextBreakpoints[i-1] >= p.ContextBreakpoints[i] {
			return fail("context breakpoints must be strictly increasing")
		}
	}
	if p.RouterInputMaxChars <= 0 {
		return fail("router_input_max_chars must be positive")
	}
	if p.DefaultSlotOnFailure < 0 || p.DefaultSlotOnFailure >= int64(len(p.Slots)) {
		return fail("default_slot_on_failure must be a valid slot index")
	}
	if strings.EqualFold(fakeLlmTargetAlias(determiner), p.ModelAlias) {
		return fail("routing policy alias cannot equal determiner model alias")
	}
	if p.DeterminerPrompt != nil && len([]rune(*p.DeterminerPrompt)) > 8000 {
		return fail("determiner_prompt must be at most 8000 characters")
	}
	for i, slot := range p.Slots {
		if slot.Description != nil && len([]rune(*slot.Description)) > 2000 {
			return fail(fmt.Sprintf("slot %d description must be at most 2000 characters", i))
		}
	}
	if !slices.Contains([]string{"savings", "balanced", "quality"}, p.Posture) {
		return fail("unknown posture: " + p.Posture)
	}
	return true
}

// routingPolicyAliasTaken mirrors the (org_id, lower(model_alias)) unique
// index. Callers hold f.mu.
func (f *fakeLlmGatewayServer) routingPolicyAliasTaken(alias, excludeID string) bool {
	for _, p := range f.routingPolicies {
		if p.ID != excludeID && strings.EqualFold(p.ModelAlias, alias) {
			return true
		}
	}
	return false
}

// fakeLlmNormalizeDeterminer mirrors normalize_determiner_alias: trimmed,
// empty stored as NULL.
func fakeLlmNormalizeDeterminer(v *string) *string {
	if v == nil || strings.TrimSpace(*v) == "" {
		return nil
	}
	trimmed := strings.TrimSpace(*v)
	return &trimmed
}

func (f *fakeLlmGatewayServer) handleRoutingPolicies(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/llm-gateway/admin/routing-policies"), "/")

	switch {
	case id == "" && r.Method == http.MethodGet:
		sorted := slices.Clone(f.routingPolicies)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ModelAlias < sorted[j].ModelAlias })
		items := make([]map[string]any, 0, len(sorted))
		for _, p := range sorted {
			items = append(items, routingPolicyJSON(p))
		}
		_ = json.NewEncoder(w).Encode(items)
	case id == "" && r.Method == http.MethodPost:
		f.createRoutingPolicy(w, r)
	case id != "" && r.Method == http.MethodPut:
		f.updateRoutingPolicy(w, r, id)
	case id != "" && r.Method == http.MethodDelete:
		if f.findRoutingPolicy(id) == nil {
			writeLlmError(w, http.StatusNotFound, fmt.Sprintf("routing policy '%s' not found", id))
			return
		}
		f.removeRoutingPolicy(id)
		_ = json.NewEncoder(w).Encode(map[string]any{"deleted": true})
	default:
		http.NotFound(w, r)
	}
}

// removeRoutingPolicy hard-deletes a policy and, like the ON DELETE CASCADE,
// its rules. Callers hold f.mu.
func (f *fakeLlmGatewayServer) removeRoutingPolicy(id string) {
	f.routingPolicies = slices.DeleteFunc(f.routingPolicies, func(p *fakeLlmRoutingPolicy) bool { return p.ID == id })
	f.routingRules = slices.DeleteFunc(f.routingRules, func(r *fakeLlmRoutingRule) bool { return r.PolicyID == id })
}

func (f *fakeLlmGatewayServer) createRoutingPolicy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ModelAlias           string               `json:"model_alias"`
		Description          *string              `json:"description"`
		Enabled              *bool                `json:"enabled"`
		DeterminerModelAlias *string              `json:"determiner_model_alias"`
		DeterminerPrompt     *string              `json:"determiner_prompt"`
		Posture              *string              `json:"posture"`
		Slots                []fakeLlmRoutingSlot `json:"slots"`
		ContextBreakpoints   *[]int64             `json:"context_breakpoints"`
		RouterInputMaxChars  *int64               `json:"router_input_max_chars"`
		DefaultSlotOnFailure *int64               `json:"default_slot_on_failure"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeLlmError(w, http.StatusBadRequest, err.Error())
		return
	}

	// RoutingPolicyDraft's serde defaults.
	stamp := f.fakeLlmStamp()
	p := &fakeLlmRoutingPolicy{
		ModelAlias:           body.ModelAlias,
		Description:          body.Description,
		Enabled:              true,
		DeterminerModelAlias: body.DeterminerModelAlias,
		DeterminerPrompt:     body.DeterminerPrompt,
		Posture:              "balanced",
		Slots:                body.Slots,
		ContextBreakpoints:   []int64{128000, 512000},
		RouterInputMaxChars:  12000,
		DefaultSlotOnFailure: 1,
		CreatedAt:            stamp,
		UpdatedAt:            stamp,
	}
	if body.Enabled != nil {
		p.Enabled = *body.Enabled
	}
	if body.Posture != nil {
		p.Posture = *body.Posture
	}
	if body.ContextBreakpoints != nil {
		p.ContextBreakpoints = *body.ContextBreakpoints
	}
	if body.RouterInputMaxChars != nil {
		p.RouterInputMaxChars = *body.RouterInputMaxChars
	}
	if body.DefaultSlotOnFailure != nil {
		p.DefaultSlotOnFailure = *body.DefaultSlotOnFailure
	}

	if !f.validRoutingPolicyRefs(w, p) || !validRoutingPolicyDraft(w, p) {
		return
	}
	if f.routingPolicyAliasTaken(p.ModelAlias, "") {
		writeLlmError(w, http.StatusConflict, "A resource with that name already exists.")
		return
	}
	p.DeterminerModelAlias = fakeLlmNormalizeDeterminer(p.DeterminerModelAlias)
	p.ID = f.newID("cccc")
	f.routingPolicies = append(f.routingPolicies, p)
	_ = json.NewEncoder(w).Encode(routingPolicyJSON(p))
}

func (f *fakeLlmGatewayServer) updateRoutingPolicy(w http.ResponseWriter, r *http.Request, id string) {
	existing := f.findRoutingPolicy(id)
	if existing == nil {
		writeLlmError(w, http.StatusNotFound, fmt.Sprintf("routing policy '%s' not found", id))
		return
	}

	// RoutingPolicyUpdate: every key optional, and a JSON null decodes to
	// None just like an absent key, so both keep the stored value.
	var body struct {
		ModelAlias           *string               `json:"model_alias"`
		Description          *string               `json:"description"`
		Enabled              *bool                 `json:"enabled"`
		DeterminerModelAlias *string               `json:"determiner_model_alias"`
		DeterminerPrompt     *string               `json:"determiner_prompt"`
		Posture              *string               `json:"posture"`
		Slots                *[]fakeLlmRoutingSlot `json:"slots"`
		ContextBreakpoints   *[]int64              `json:"context_breakpoints"`
		RouterInputMaxChars  *int64                `json:"router_input_max_chars"`
		DefaultSlotOnFailure *int64                `json:"default_slot_on_failure"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeLlmError(w, http.StatusBadRequest, err.Error())
		return
	}

	merged := *existing
	if body.ModelAlias != nil {
		merged.ModelAlias = *body.ModelAlias
	}
	if body.Description != nil {
		merged.Description = body.Description
	}
	if body.Enabled != nil {
		merged.Enabled = *body.Enabled
	}
	if body.DeterminerModelAlias != nil {
		merged.DeterminerModelAlias = body.DeterminerModelAlias
	}
	if body.DeterminerPrompt != nil {
		merged.DeterminerPrompt = body.DeterminerPrompt
	}
	if body.Posture != nil {
		merged.Posture = *body.Posture
	}
	if body.Slots != nil {
		merged.Slots = *body.Slots
	}
	if body.ContextBreakpoints != nil {
		merged.ContextBreakpoints = *body.ContextBreakpoints
	}
	if body.RouterInputMaxChars != nil {
		merged.RouterInputMaxChars = *body.RouterInputMaxChars
	}
	if body.DefaultSlotOnFailure != nil {
		merged.DefaultSlotOnFailure = *body.DefaultSlotOnFailure
	}

	if !f.validRoutingPolicyRefs(w, &merged) || !validRoutingPolicyDraft(w, &merged) {
		return
	}
	if f.routingPolicyAliasTaken(merged.ModelAlias, id) {
		writeLlmError(w, http.StatusConflict, "A resource with that name already exists.")
		return
	}
	merged.DeterminerModelAlias = fakeLlmNormalizeDeterminer(merged.DeterminerModelAlias)
	merged.UpdatedAt = f.fakeLlmStamp()
	*existing = merged
	_ = json.NewEncoder(w).Encode(routingPolicyJSON(existing))
}

// seedRoutingPolicy stores a policy directly, as if created in the app.
// Reference checks are skipped.
func (f *fakeLlmGatewayServer) seedRoutingPolicy(alias string, slots ...string) *fakeLlmRoutingPolicy {
	f.mu.Lock()
	defer f.mu.Unlock()
	determiner := slots[0]
	p := &fakeLlmRoutingPolicy{
		ID:                   f.newID("cccc"),
		ModelAlias:           alias,
		Enabled:              true,
		DeterminerModelAlias: &determiner,
		Posture:              "balanced",
		ContextBreakpoints:   []int64{128000, 512000}[:len(slots)-1],
		RouterInputMaxChars:  12000,
		DefaultSlotOnFailure: 1,
		CreatedAt:            fakeLlmTime,
		UpdatedAt:            fakeLlmTime,
	}
	for _, s := range slots {
		p.Slots = append(p.Slots, fakeLlmRoutingSlot{ModelAlias: s})
	}
	f.routingPolicies = append(f.routingPolicies, p)
	return p
}

// routingPolicySnapshot returns a copy of a stored policy (nil when absent).
func (f *fakeLlmGatewayServer) routingPolicySnapshot(id string) *fakeLlmRoutingPolicy {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.findRoutingPolicy(id); p != nil {
		c := *p
		return &c
	}
	return nil
}

// markRoutingPolicyDeleted deletes a stored policy (and its rules) out-of-band.
func (f *fakeLlmGatewayServer) markRoutingPolicyDeleted(t *testing.T, id string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.findRoutingPolicy(id) == nil {
		t.Fatalf("fake has no routing policy %q to delete", id)
	}
	f.removeRoutingPolicy(id)
}

// checkAllLlmRoutingDeleted is the CheckDestroy for routing tests.
func checkAllLlmRoutingDeleted(fake *fakeLlmGatewayServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, p := range fake.routingPolicies {
			return fmt.Errorf("routing policy %s (%s) was not deleted on destroy", p.ID, p.ModelAlias)
		}
		for _, r := range fake.routingRules {
			return fmt.Errorf("routing rule %s (%s) was not deleted on destroy", r.ID, r.Name)
		}
		return nil
	}
}

// --- routing rules -----------------------------------------------------------------

func routingRuleJSON(r *fakeLlmRoutingRule) map[string]any {
	deny := r.DenySlots
	if deny == nil {
		deny = []int64{}
	}
	return map[string]any{
		"id":          r.ID,
		"org_id":      fakeLlmOrgID,
		"policy_id":   r.PolicyID,
		"name":        r.Name,
		"description": r.Description,
		"floor_slot":  r.FloorSlot,
		"deny_slots":  deny,
		"enabled":     r.Enabled,
	}
}

// writeLlmUnprocessable renders the routing-rule handlers' flat 422 body.
func writeLlmUnprocessable(w http.ResponseWriter, message string) {
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": message})
}

// fakeLlmRoutingRuleDraft mirrors RoutingRuleDraft; enabled defaults to true.
type fakeLlmRoutingRuleDraft struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	FloorSlot   *int64  `json:"floor_slot"`
	DenySlots   []int64 `json:"deny_slots"`
	Enabled     *bool   `json:"enabled"`
}

// decodeRoutingRuleDraft decodes, normalizes and validates a rule body. A
// missing required key is axum's Json data rejection (422, plain text); an
// authoring-rule failure is the handler's flat 422.
func decodeRoutingRuleDraft(w http.ResponseWriter, r *http.Request) (*fakeLlmRoutingRule, bool) {
	var d fakeLlmRoutingRuleDraft
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		http.Error(w, "Failed to parse the request body as JSON: "+err.Error(), http.StatusBadRequest)
		return nil, false
	}
	if d.Name == nil || d.Description == nil {
		http.Error(w, "Failed to deserialize the JSON body into the target type: missing field",
			http.StatusUnprocessableEntity)
		return nil, false
	}
	deny := slices.Clone(d.DenySlots)
	slices.Sort(deny)
	deny = slices.Compact(deny)
	rule := &fakeLlmRoutingRule{
		Name:        strings.TrimSpace(*d.Name),
		Description: strings.TrimSpace(*d.Description),
		FloorSlot:   d.FloorSlot,
		DenySlots:   deny,
		Enabled:     d.Enabled == nil || *d.Enabled,
	}

	fail := func(msg string) (*fakeLlmRoutingRule, bool) {
		writeLlmUnprocessable(w, msg)
		return nil, false
	}
	switch {
	case rule.Name == "":
		return fail("rule name is required")
	case len([]rune(rule.Name)) > 120:
		return fail("rule name must be 120 characters or fewer")
	case rule.Description == "":
		return fail("rule description is required — it is what the router matches on")
	case len([]rune(rule.Description)) > 2000:
		return fail("rule description must be 2000 characters or fewer")
	case rule.FloorSlot != nil && *rule.FloorSlot < 0:
		return fail("floor_slot must be zero or greater")
	case slices.ContainsFunc(rule.DenySlots, func(s int64) bool { return s < 0 }):
		return fail("deny_slots must be zero or greater")
	case rule.FloorSlot == nil && len(rule.DenySlots) == 0:
		return fail("a rule needs a floor, a banned model, or both — one with neither would never change any request")
	}
	return rule, true
}

// routingRuleNameTaken mirrors UNIQUE(policy_id, name) — case-sensitive.
// Callers hold f.mu.
func (f *fakeLlmGatewayServer) routingRuleNameTaken(policyID, name, excludeID string) bool {
	for _, r := range f.routingRules {
		if r.PolicyID == policyID && r.Name == name && r.ID != excludeID {
			return true
		}
	}
	return false
}

// rulesForPolicy lists a policy's rules ordered by lower(name). Callers hold
// f.mu.
func (f *fakeLlmGatewayServer) rulesForPolicy(policyID string) []*fakeLlmRoutingRule {
	var out []*fakeLlmRoutingRule
	for _, r := range f.routingRules {
		if r.PolicyID == policyID {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// fakeLlmRuleEffect mirrors routing::rules::effect_of: slot indices outside
// the policy are ignored.
func fakeLlmRuleEffect(r *fakeLlmRoutingRule, slotCount int) (floor *int, bans map[int]bool, conflict bool) {
	bans = map[int]bool{}
	if r.FloorSlot != nil && *r.FloorSlot < int64(slotCount) {
		v := int(*r.FloorSlot)
		floor = &v
	}
	for _, s := range r.DenySlots {
		if s < int64(slotCount) {
			bans[int(s)] = true
		}
	}
	if floor != nil {
		conflict = true
		for s := *floor; s < slotCount; s++ {
			if !bans[s] {
				conflict = false
			}
		}
	}
	return floor, bans, conflict
}

// fakeLlmDetectConflicts mirrors detect_conflicts over the enabled rules.
func fakeLlmDetectConflicts(rules []*fakeLlmRoutingRule, slotCount int) []map[string]any {
	conflicts := []map[string]any{}
	var enabled []*fakeLlmRoutingRule
	for _, r := range rules {
		if r.Enabled {
			enabled = append(enabled, r)
		}
	}
	for _, r := range enabled {
		if floor, _, conflict := fakeLlmRuleEffect(r, slotCount); conflict {
			conflicts = append(conflicts, map[string]any{
				"rule": r.Name, "conflicts_with": r.Name,
				"detail": fmt.Sprintf("bans every slot at or above its own floor (%d), so it can never apply", *floor),
			})
		}
	}
	for _, r := range enabled {
		floor, _, _ := fakeLlmRuleEffect(r, slotCount)
		if floor == nil {
			continue
		}
		for _, other := range enabled {
			if other.ID == r.ID {
				continue
			}
			_, otherBans, _ := fakeLlmRuleEffect(other, slotCount)
			all := true
			for s := *floor; s < slotCount; s++ {
				if !otherBans[s] {
					all = false
				}
			}
			if all {
				conflicts = append(conflicts, map[string]any{
					"rule": r.Name, "conflicts_with": other.Name,
					"detail": fmt.Sprintf("'%s' requires slot %d or higher, which '%s' bans. "+
						"If both match a request, the ban wins.", r.Name, *floor, other.Name),
				})
			}
		}
	}
	return conflicts
}

// policyScope extracts the required `policy_id` query parameter the way
// axum's Query<PolicyScope> does (400, plain text, when missing or not a
// UUID), then loads the policy (404 when absent). Callers hold f.mu.
func (f *fakeLlmGatewayServer) policyScope(w http.ResponseWriter, r *http.Request) (*fakeLlmRoutingPolicy, bool) {
	raw := r.URL.Query().Get("policy_id")
	if raw == "" {
		http.Error(w, "Failed to deserialize query string: missing field `policy_id`", http.StatusBadRequest)
		return nil, false
	}
	if !fakeLlmUUID.MatchString(raw) {
		http.Error(w, "Failed to deserialize query string: policy_id: UUID parsing failed", http.StatusBadRequest)
		return nil, false
	}
	p := f.findRoutingPolicy(raw)
	if p == nil {
		writeLlmError(w, http.StatusNotFound, fmt.Sprintf("routing policy '%s' not found", raw))
		return nil, false
	}
	return p, true
}

// writeRoutingRule renders the write response: the rule flattened plus the
// policy's current conflicts.
func (f *fakeLlmGatewayServer) writeRoutingRule(w http.ResponseWriter, status int, rule *fakeLlmRoutingRule) {
	out := routingRuleJSON(rule)
	slotCount := 0
	if p := f.findRoutingPolicy(rule.PolicyID); p != nil {
		slotCount = len(p.Slots)
	}
	out["conflicts"] = fakeLlmDetectConflicts(f.rulesForPolicy(rule.PolicyID), slotCount)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(out)
}

func (f *fakeLlmGatewayServer) handleRoutingRules(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/llm-gateway/admin/routing-rules"), "/")

	switch {
	case id == "" && r.Method == http.MethodGet:
		policy, ok := f.policyScope(w, r)
		if !ok {
			return
		}
		rules := f.rulesForPolicy(policy.ID)
		items := make([]map[string]any, 0, len(rules))
		for _, rule := range rules {
			items = append(items, routingRuleJSON(rule))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"rules":     items,
			"conflicts": fakeLlmDetectConflicts(rules, len(policy.Slots)),
		})
	case id == "" && r.Method == http.MethodPost:
		policy, ok := f.policyScope(w, r)
		if !ok {
			return
		}
		rule, ok := decodeRoutingRuleDraft(w, r)
		if !ok {
			return
		}
		if f.routingRuleNameTaken(policy.ID, rule.Name, "") {
			writeLlmUnprocessable(w, "a rule with this name already exists on this routing policy")
			return
		}
		rule.ID = f.newID("dddd")
		rule.PolicyID = policy.ID
		f.routingRules = append(f.routingRules, rule)
		f.writeRoutingRule(w, http.StatusCreated, rule)
	case id != "" && r.Method == http.MethodPut:
		// The store validates before it looks the rule up.
		draft, ok := decodeRoutingRuleDraft(w, r)
		if !ok {
			return
		}
		idx := slices.IndexFunc(f.routingRules, func(rule *fakeLlmRoutingRule) bool { return rule.ID == id })
		if idx < 0 {
			writeLlmError(w, http.StatusNotFound, fmt.Sprintf("routing rule '%s' not found", id))
			return
		}
		existing := f.routingRules[idx]
		if f.routingRuleNameTaken(existing.PolicyID, draft.Name, id) {
			writeLlmUnprocessable(w, "a rule with this name already exists on this routing policy")
			return
		}
		// Full replace: every column comes from the draft.
		draft.ID, draft.PolicyID = existing.ID, existing.PolicyID
		*existing = *draft
		f.writeRoutingRule(w, http.StatusOK, existing)
	case id != "" && r.Method == http.MethodDelete:
		idx := slices.IndexFunc(f.routingRules, func(rule *fakeLlmRoutingRule) bool { return rule.ID == id })
		if idx < 0 {
			writeLlmError(w, http.StatusNotFound, fmt.Sprintf("routing rule '%s' not found", id))
			return
		}
		f.routingRules = slices.Delete(f.routingRules, idx, idx+1)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// routingRuleSnapshot returns a copy of a stored rule (nil when absent).
func (f *fakeLlmGatewayServer) routingRuleSnapshot(id string) *fakeLlmRoutingRule {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.routingRules {
		if r.ID == id {
			c := *r
			return &c
		}
	}
	return nil
}

// markRoutingRuleDeleted deletes a stored rule out-of-band.
func (f *fakeLlmGatewayServer) markRoutingRuleDeleted(t *testing.T, id string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	idx := slices.IndexFunc(f.routingRules, func(rule *fakeLlmRoutingRule) bool { return rule.ID == id })
	if idx < 0 {
		t.Fatalf("fake has no routing rule %q to delete", id)
	}
	f.routingRules = slices.Delete(f.routingRules, idx, idx+1)
}
