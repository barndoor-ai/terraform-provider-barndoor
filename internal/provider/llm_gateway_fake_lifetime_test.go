// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// --- API key lifetime policies (fake llm-gateway) -------------------------------
//
// Mirrors `routes/admin/api_key_lifetime_policies.rs` and
// `db/api_key_lifetime_policy.rs`:
//
//   - GET returns {default, groups, users}; PUT /default answers the same
//     shape. Writing no limit (no max, require_expiry false) to the default
//     removes its row.
//   - LifetimeLimitInput::validate: max_lifetime_days in 1..=3650, and
//     fixed_lifetime needs a maximum (400 each).
//   - POST /groups trims group_name (400 when empty) and validates; one
//     override per group (409). POST /users validates, then requires the user
//     to belong to the org (400); one override per user (409).
//   - PUT and DELETE {groups,users}/{id} are org-scoped and 404 on an unknown
//     id (a group id never matches the default row).

// fakeLlmOutsiderUserID is a user the fake treats as outside the org.
const fakeLlmOutsiderUserID = "99999999-9999-9999-9999-999999999999"

type fakeLlmLifetimeRow struct {
	ID              string
	GroupName       *string // nil = the org default (in lifetimeDefault)
	UserID          string  // set on user overrides
	MaxLifetimeDays *int32
	RequireExpiry   bool
	FixedLifetime   bool
}

type fakeLlmLifetimeInput struct {
	MaxLifetimeDays *int32 `json:"max_lifetime_days"`
	RequireExpiry   bool   `json:"require_expiry"`
	FixedLifetime   bool   `json:"fixed_lifetime"`
}

func (in fakeLlmLifetimeInput) validate(w http.ResponseWriter) bool {
	if in.FixedLifetime && in.MaxLifetimeDays == nil {
		writeLlmError(w, http.StatusBadRequest,
			"fixed_lifetime needs max_lifetime_days: the number of days every key gets.")
		return false
	}
	if d := in.MaxLifetimeDays; d != nil && (*d < 1 || *d > 3650) {
		writeLlmError(w, http.StatusBadRequest, "max_lifetime_days must be between 1 and 3650.")
		return false
	}
	return true
}

func (row *fakeLlmLifetimeRow) apply(in fakeLlmLifetimeInput) {
	row.MaxLifetimeDays = in.MaxLifetimeDays
	row.RequireExpiry = in.RequireExpiry
	row.FixedLifetime = in.FixedLifetime
}

func (row *fakeLlmLifetimeRow) policyJSON() map[string]any {
	return map[string]any{
		"id": row.ID, "group_name": row.GroupName,
		"max_lifetime_days": row.MaxLifetimeDays, "require_expiry": row.RequireExpiry,
		"fixed_lifetime": row.FixedLifetime, "updated_at": fakeLlmTime, "updated_by_user_id": nil,
	}
}

func (row *fakeLlmLifetimeRow) userJSON() map[string]any {
	return map[string]any{
		"id": row.ID, "user_id": row.UserID,
		"max_lifetime_days": row.MaxLifetimeDays, "require_expiry": row.RequireExpiry,
		"fixed_lifetime": row.FixedLifetime, "updated_at": fakeLlmTime, "updated_by_user_id": nil,
	}
}

// lifetimePoliciesJSON renders the GET body. Callers hold f.mu.
func (f *fakeLlmGatewayServer) lifetimePoliciesJSON() map[string]any {
	var def any
	if f.lifetimeDefault != nil {
		def = f.lifetimeDefault.policyJSON()
	}
	groups := []any{}
	for _, g := range f.lifetimeGroups {
		groups = append(groups, g.policyJSON())
	}
	users := []any{}
	for _, u := range f.lifetimeUsers {
		users = append(users, u.userJSON())
	}
	return map[string]any{"default": def, "groups": groups, "users": users}
}

func (f *fakeLlmGatewayServer) handleAPIKeyLifetimePolicies(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/llm-gateway/admin/api-key-lifetime-policies"), "/")
	seg, id, _ := strings.Cut(rest, "/")

	switch {
	case rest == "" && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(f.lifetimePoliciesJSON())

	case rest == "default" && r.Method == http.MethodPut:
		var in fakeLlmLifetimeInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "Failed to parse the request body as JSON: "+err.Error(), http.StatusUnprocessableEntity)
			return
		}
		if !in.validate(w) {
			return
		}
		if in.MaxLifetimeDays == nil && !in.RequireExpiry {
			f.lifetimeDefault = nil
		} else {
			if f.lifetimeDefault == nil {
				f.lifetimeDefault = &fakeLlmLifetimeRow{ID: f.newID("1a1a")}
			}
			f.lifetimeDefault.apply(in)
		}
		_ = json.NewEncoder(w).Encode(f.lifetimePoliciesJSON())

	case (seg == "groups" || seg == "users") && id == "" && r.Method == http.MethodPost:
		var body struct {
			fakeLlmLifetimeInput
			GroupName *string `json:"group_name"`
			UserID    *string `json:"user_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "Failed to parse the request body as JSON: "+err.Error(), http.StatusUnprocessableEntity)
			return
		}
		if seg == "groups" {
			f.createLifetimeGroup(w, body.GroupName, body.fakeLlmLifetimeInput)
		} else {
			f.createLifetimeUser(w, body.UserID, body.fakeLlmLifetimeInput)
		}

	case (seg == "groups" || seg == "users") && id != "" && r.Method == http.MethodPut:
		row := f.findLifetimeOverride(seg, id)
		var in fakeLlmLifetimeInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "Failed to parse the request body as JSON: "+err.Error(), http.StatusUnprocessableEntity)
			return
		}
		if !in.validate(w) {
			return
		}
		if row == nil {
			writeLlmError(w, http.StatusNotFound, fmt.Sprintf("key lifetime override %s not found", id))
			return
		}
		row.apply(in)
		if seg == "groups" {
			_ = json.NewEncoder(w).Encode(row.policyJSON())
		} else {
			_ = json.NewEncoder(w).Encode(row.userJSON())
		}

	case (seg == "groups" || seg == "users") && id != "" && r.Method == http.MethodDelete:
		if f.findLifetimeOverride(seg, id) == nil {
			writeLlmError(w, http.StatusNotFound, fmt.Sprintf("key lifetime override %s not found", id))
			return
		}
		if seg == "groups" {
			f.lifetimeGroups = removeLifetimeRow(f.lifetimeGroups, id)
		} else {
			f.lifetimeUsers = removeLifetimeRow(f.lifetimeUsers, id)
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"deleted": true})

	default:
		http.NotFound(w, r)
	}
}

// createLifetimeGroup mirrors create_group_lifetime_policy. Callers hold f.mu.
func (f *fakeLlmGatewayServer) createLifetimeGroup(w http.ResponseWriter, name *string, in fakeLlmLifetimeInput) {
	if name == nil {
		http.Error(w, "Failed to deserialize the JSON body into the target type: missing field `group_name`",
			http.StatusUnprocessableEntity)
		return
	}
	group := strings.TrimSpace(*name)
	if group == "" {
		writeLlmError(w, http.StatusBadRequest, "group_name is required.")
		return
	}
	if !in.validate(w) {
		return
	}
	for _, g := range f.lifetimeGroups {
		if *g.GroupName == group {
			writeLlmError(w, http.StatusConflict,
				fmt.Sprintf("The %s group already has a key lifetime override. Edit it instead.", group))
			return
		}
	}
	row := &fakeLlmLifetimeRow{ID: f.newID("2b2b"), GroupName: &group}
	row.apply(in)
	f.lifetimeGroups = append(f.lifetimeGroups, row)
	_ = json.NewEncoder(w).Encode(row.policyJSON())
}

// createLifetimeUser mirrors create_user_lifetime_policy. Callers hold f.mu.
func (f *fakeLlmGatewayServer) createLifetimeUser(w http.ResponseWriter, userID *string, in fakeLlmLifetimeInput) {
	if userID == nil {
		http.Error(w, "Failed to deserialize the JSON body into the target type: missing field `user_id`",
			http.StatusUnprocessableEntity)
		return
	}
	id := strings.ToLower(*userID)
	if !in.validate(w) {
		return
	}
	if id == fakeLlmOutsiderUserID {
		writeLlmError(w, http.StatusBadRequest, "user_id is not a member of this organization.")
		return
	}
	for _, u := range f.lifetimeUsers {
		if u.UserID == id {
			writeLlmError(w, http.StatusConflict,
				"This user already has a key lifetime override. Edit it instead.")
			return
		}
	}
	row := &fakeLlmLifetimeRow{ID: f.newID("3c3c"), UserID: id}
	row.apply(in)
	f.lifetimeUsers = append(f.lifetimeUsers, row)
	_ = json.NewEncoder(w).Encode(row.userJSON())
}

// findLifetimeOverride finds a group or user override by id. Callers hold f.mu.
func (f *fakeLlmGatewayServer) findLifetimeOverride(seg, id string) *fakeLlmLifetimeRow {
	rows := f.lifetimeGroups
	if seg == "users" {
		rows = f.lifetimeUsers
	}
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	return nil
}

func removeLifetimeRow(rows []*fakeLlmLifetimeRow, id string) []*fakeLlmLifetimeRow {
	out := rows[:0]
	for _, row := range rows {
		if row.ID != id {
			out = append(out, row)
		}
	}
	return out
}

// checkLlmLifetimePoliciesCleared is the CheckDestroy for lifetime policy
// tests: no default, group or user override may survive a destroy.
func checkLlmLifetimePoliciesCleared(fake *fakeLlmGatewayServer) func(*terraform.State) error {
	return func(*terraform.State) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if fake.lifetimeDefault != nil {
			return fmt.Errorf("default lifetime policy still set after destroy")
		}
		if n := len(fake.lifetimeGroups) + len(fake.lifetimeUsers); n > 0 {
			return fmt.Errorf("%d lifetime override(s) still present after destroy", n)
		}
		return nil
	}
}
