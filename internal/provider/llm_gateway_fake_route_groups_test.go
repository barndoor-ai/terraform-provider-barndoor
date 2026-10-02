// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// --- model route groups (fakeLlmGatewayServer) ----------------------------------
//
// Mirrors bdai-platform's `routes/admin/model_route_groups.rs` and
// `db/model_route_groups.rs` (V71): listing-only reads sorted by lower(name),
// trimmed name/description, (org_id, lower(name)) uniqueness answered with a
// 409, aliases trimmed with blanks dropped and duplicates collapsed (no check
// against existing mappings), a 1000-alias request cap, a 500-character change
// note that an update omitting it clears, COALESCE name/description and
// wholesale model_aliases replacement on PUT, and a DELETE that never refuses
// (policies keep a dangling target). Mapping renames and last-mapping deletes
// sweep memberships through renameRouteGroupAlias / forgetRouteGroupAliasIfGone.

// fakeLlmRouteGroupMaxMembers mirrors MAX_MEMBERS_PER_REQUEST.
const fakeLlmRouteGroupMaxMembers = 1000

// fakeLlmChangeNoteMaxChars mirrors normalize_change_note's cap.
const fakeLlmChangeNoteMaxChars = 500

type fakeLlmRouteGroup struct {
	ID             string
	Name           string
	Description    string
	ModelAliases   []string // sorted, unique
	LastChangeNote *string
}

func (f *fakeLlmGatewayServer) findRouteGroup(id string) *fakeLlmRouteGroup {
	for _, g := range f.routeGroups {
		if g.ID == id {
			return g
		}
	}
	return nil
}

func routeGroupJSON(g *fakeLlmRouteGroup) map[string]any {
	aliases := g.ModelAliases
	if aliases == nil {
		aliases = []string{}
	}
	return map[string]any{
		"id":            g.ID,
		"org_id":        fakeLlmOrgID,
		"name":          g.Name,
		"description":   g.Description,
		"model_aliases": aliases,
		// Flattened ChangeAttribution; only the note is modelled.
		"created_by_user_id":       nil,
		"created_by_name":          nil,
		"created_by_email":         nil,
		"last_modified_by_user_id": nil,
		"last_modified_by_name":    nil,
		"last_modified_by_email":   nil,
		"last_change_note":         g.LastChangeNote,
		"last_modified_at":         fakeLlmTime,
	}
}

// fakeLlmCleanAliases mirrors clean_aliases plus the set semantics of the
// UNIQUE (group_id, model_alias) membership rows and the sorted array_agg.
func fakeLlmCleanAliases(w http.ResponseWriter, aliases []string) ([]string, bool) {
	if len(aliases) > fakeLlmRouteGroupMaxMembers {
		writeLlmError(w, http.StatusBadRequest, fmt.Sprintf(
			"at most %d routes can be sent in one request", fakeLlmRouteGroupMaxMembers))
		return nil, false
	}
	out := []string{}
	for _, a := range aliases {
		if a = strings.TrimSpace(a); a != "" && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out, true
}

// fakeLlmNormalizeChangeNote mirrors normalize_change_note: trimmed, empty
// means none, over-long is a 400.
func fakeLlmNormalizeChangeNote(w http.ResponseWriter, note *string) (*string, bool) {
	if note == nil {
		return nil, true
	}
	trimmed := strings.TrimSpace(*note)
	if trimmed == "" {
		return nil, true
	}
	if utf8.RuneCountInString(trimmed) > fakeLlmChangeNoteMaxChars {
		writeLlmError(w, http.StatusBadRequest, fmt.Sprintf(
			"change_note must be at most %d characters", fakeLlmChangeNoteMaxChars))
		return nil, false
	}
	return &trimmed, true
}

// routeGroupNameTaken mirrors the (org_id, lower(name)) unique index.
func (f *fakeLlmGatewayServer) routeGroupNameTaken(name, excludeID string) bool {
	for _, g := range f.routeGroups {
		if g.ID != excludeID && strings.EqualFold(g.Name, name) {
			return true
		}
	}
	return false
}

func writeRouteGroupNameConflict(w http.ResponseWriter, name string) {
	writeLlmError(w, http.StatusConflict, fmt.Sprintf(
		"a route group named '%s' already exists for this organization", name))
}

func (f *fakeLlmGatewayServer) handleRouteGroups(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/llm-gateway/admin/model-route-groups"), "/")

	switch {
	case id == "" && r.Method == http.MethodPost:
		f.createRouteGroup(w, r)
	case id == "" && r.Method == http.MethodGet:
		groups := slices.Clone(f.routeGroups)
		sort.SliceStable(groups, func(i, j int) bool {
			return strings.ToLower(groups[i].Name) < strings.ToLower(groups[j].Name)
		})
		items := make([]map[string]any, 0, len(groups))
		for _, g := range groups {
			items = append(items, routeGroupJSON(g))
		}
		_ = json.NewEncoder(w).Encode(items)
	case id != "" && !strings.Contains(id, "/") && r.Method == http.MethodPut:
		f.updateRouteGroup(w, r, id)
	case id != "" && !strings.Contains(id, "/") && r.Method == http.MethodDelete:
		for i, g := range f.routeGroups {
			if g.ID == id {
				f.routeGroups = append(f.routeGroups[:i], f.routeGroups[i+1:]...)
				_ = json.NewEncoder(w).Encode(map[string]any{"deleted": true})
				return
			}
		}
		writeLlmError(w, http.StatusNotFound, fmt.Sprintf("route group %s not found", id))
	default:
		// The /{id}/members endpoints are not bound by the provider (PUT
		// replaces the membership), so the fake does not serve them.
		http.NotFound(w, r)
	}
}

func (f *fakeLlmGatewayServer) createRouteGroup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name         string   `json:"name"`
		Description  string   `json:"description"`
		ModelAliases []string `json:"model_aliases"`
		ChangeNote   *string  `json:"change_note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeLlmError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeLlmError(w, http.StatusBadRequest, "group name is required")
		return
	}
	note, ok := fakeLlmNormalizeChangeNote(w, body.ChangeNote)
	if !ok {
		return
	}
	aliases, ok := fakeLlmCleanAliases(w, body.ModelAliases)
	if !ok {
		return
	}
	if f.routeGroupNameTaken(name, "") {
		writeRouteGroupNameConflict(w, name)
		return
	}

	g := &fakeLlmRouteGroup{
		ID:             f.newID("abab"),
		Name:           name,
		Description:    strings.TrimSpace(body.Description),
		ModelAliases:   aliases,
		LastChangeNote: note,
	}
	f.routeGroups = append(f.routeGroups, g)
	_ = json.NewEncoder(w).Encode(routeGroupJSON(g))
}

func (f *fakeLlmGatewayServer) updateRouteGroup(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Name         *string   `json:"name"`
		Description  *string   `json:"description"`
		ModelAliases *[]string `json:"model_aliases"`
		ChangeNote   *string   `json:"change_note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeLlmError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Every validation runs before the first write, like production.
	if body.Name != nil && strings.TrimSpace(*body.Name) == "" {
		writeLlmError(w, http.StatusBadRequest, "group name is required")
		return
	}
	var aliases []string
	if body.ModelAliases != nil {
		var ok bool
		if aliases, ok = fakeLlmCleanAliases(w, *body.ModelAliases); !ok {
			return
		}
	}
	note, ok := fakeLlmNormalizeChangeNote(w, body.ChangeNote)
	if !ok {
		return
	}

	g := f.findRouteGroup(id)
	if g == nil {
		writeLlmError(w, http.StatusNotFound, fmt.Sprintf("route group %s not found", id))
		return
	}
	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		if f.routeGroupNameTaken(name, id) {
			writeRouteGroupNameConflict(w, name)
			return
		}
		g.Name = name
	}
	if body.Description != nil {
		g.Description = strings.TrimSpace(*body.Description)
	}
	if body.ModelAliases != nil {
		g.ModelAliases = aliases
	}
	// The note describes this edit only: omitting it clears the last one.
	g.LastChangeNote = note

	_ = json.NewEncoder(w).Encode(routeGroupJSON(g))
}

// renameRouteGroupAlias mirrors ModelRouteGroupDb::rename_alias, which the
// mapping update handler runs whenever a mapping's alias changes: every
// membership moves to the new alias, collapsing into an existing membership
// of the destination. Callers hold f.mu.
func (f *fakeLlmGatewayServer) renameRouteGroupAlias(oldAlias, newAlias string) {
	if oldAlias == newAlias {
		return
	}
	for _, g := range f.routeGroups {
		i := slices.Index(g.ModelAliases, oldAlias)
		if i < 0 {
			continue
		}
		g.ModelAliases = slices.Delete(g.ModelAliases, i, i+1)
		if !slices.Contains(g.ModelAliases, newAlias) {
			g.ModelAliases = append(g.ModelAliases, newAlias)
			sort.Strings(g.ModelAliases)
		}
	}
}

// forgetRouteGroupAliasIfGone mirrors the mapping delete handler: once a
// route's last mapping is gone, ModelRouteGroupDb::forget_alias drops the
// alias from every group. Callers hold f.mu.
func (f *fakeLlmGatewayServer) forgetRouteGroupAliasIfGone(alias string) {
	for _, m := range f.mappings {
		if m.ModelAlias == alias {
			return
		}
	}
	for _, g := range f.routeGroups {
		g.ModelAliases = slices.DeleteFunc(g.ModelAliases, func(a string) bool { return a == alias })
	}
}

// seedRouteGroupMapping stores a bare mapping row for alias, standing in for
// a route created outside Terraform so a test can rename or delete it.
func (f *fakeLlmGatewayServer) seedRouteGroupMapping(alias string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := &fakeLlmModelMapping{
		ID:            f.newID("cdcd"),
		ProviderID:    f.newID("aaaa"),
		ModelAlias:    alias,
		UpstreamModel: alias,
		Enabled:       true,
	}
	f.mappings = append(f.mappings, m)
	return m.ID
}

// renameMappingOutOfBand renames a stored mapping's alias as an app edit
// would, running the platform's membership sweep.
func (f *fakeLlmGatewayServer) renameMappingOutOfBand(t *testing.T, id, newAlias string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.findMapping(id)
	if m == nil {
		t.Fatalf("fake has no model mapping %q to rename", id)
	}
	old := m.ModelAlias
	m.ModelAlias = newAlias
	f.renameRouteGroupAlias(old, newAlias)
}

// routeGroupSnapshot returns a copy of the stored group, or nil.
func (f *fakeLlmGatewayServer) routeGroupSnapshot(id string) *fakeLlmRouteGroup {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.findRouteGroup(id)
	if g == nil {
		return nil
	}
	cp := *g
	cp.ModelAliases = slices.Clone(g.ModelAliases)
	return &cp
}

// markRouteGroupDeleted removes a stored route group out-of-band.
func (f *fakeLlmGatewayServer) markRouteGroupDeleted(t *testing.T, id string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, g := range f.routeGroups {
		if g.ID == id {
			f.routeGroups = append(f.routeGroups[:i], f.routeGroups[i+1:]...)
			return
		}
	}
	t.Fatalf("fake has no route group %q to delete", id)
}

// checkAllLlmRouteGroupsDeleted is the CheckDestroy for route-group tests.
func checkAllLlmRouteGroupsDeleted(fake *fakeLlmGatewayServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, g := range fake.routeGroups {
			return fmt.Errorf("route group %s (%s) was not deleted on destroy", g.ID, g.Name)
		}
		return nil
	}
}
