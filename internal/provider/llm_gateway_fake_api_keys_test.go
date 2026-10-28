// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// --- API keys (fake llm-gateway) -------------------------------------------------
//
// Mirrors `routes/admin/api_keys.rs` and `db/api_keys.rs`:
//
//   - POST trims group_name (blank = none), refuses user_id together with
//     group_name and a past expires_at (400), and keeps names unique among
//     live keys of the same user, group or organization (409). kind is `user`
//     with a user_id, `machine` otherwise. The secret is returned here and on
//     rotate only.
//   - GET {id} returns revoked keys too, with revoked_at set.
//   - PATCH {id} {disabled}, PUT {id}/expiry {expires_at: <time>|null} (the
//     field is required; a past time is 400) and POST {id}/rotate
//     {grace_period_secs, expires_at?} (grace at most 7 days; a grace period
//     only parks a still-live secret) all 404 on a revoked key, as does a
//     second DELETE.
//   - Timestamps come back as UTC with microseconds, like chrono, whatever
//     offset or precision was sent.

type fakeLlmAPIKey struct {
	ID                   string
	Kind                 string
	UserID               *string
	Name                 string
	Secret               string
	Scopes               []string
	GroupName            *string
	ExpiresAt            *time.Time
	RevokedAt            *time.Time
	DisabledAt           *time.Time
	RotatedAt            *time.Time
	PreviousKeyExpiresAt *time.Time
	PreviousSecret       string
}

// fakeLlmChronoTime renders a time the way chrono serializes DateTime<Utc>.
func fakeLlmChronoTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format("2006-01-02T15:04:05.000000Z")
}

func (k *fakeLlmAPIKey) json() map[string]any {
	return map[string]any{
		"id": k.ID, "org_id": fakeLlmOrgID, "kind": k.Kind, "user_id": k.UserID,
		"created_by_user_id": "00000000-0000-0000-0000-00000000ad01", "name": k.Name,
		"key_prefix": k.Secret[:8], "scopes": k.Scopes, "group_name": k.GroupName,
		"expires_at": fakeLlmChronoTime(k.ExpiresAt), "created_at": fakeLlmTime,
		"revoked_at": fakeLlmChronoTime(k.RevokedAt), "disabled_at": fakeLlmChronoTime(k.DisabledAt),
		"last_used_at": nil, "rotated_at": fakeLlmChronoTime(k.RotatedAt),
		"previous_key_expires_at": fakeLlmChronoTime(k.PreviousKeyExpiresAt),
	}
}

// newSecret mints a deterministic, unique key secret. Callers hold f.mu.
func (f *fakeLlmGatewayServer) newSecret() string {
	f.nextID++
	return fmt.Sprintf("bdk_%04x%032d", f.nextID, f.nextID)
}

func (f *fakeLlmGatewayServer) findAPIKey(id string) *fakeLlmAPIKey {
	for _, k := range f.apiKeys {
		if k.ID == id {
			return k
		}
	}
	return nil
}

// parseFutureTime reads an RFC 3339 expiry and refuses a past one with the
// handler's message.
func parseFutureTime(w http.ResponseWriter, raw string) (*time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		http.Error(w, "Failed to deserialize the JSON body into the target type: expires_at: "+err.Error(),
			http.StatusUnprocessableEntity)
		return nil, false
	}
	if !t.After(time.Now()) {
		writeLlmError(w, http.StatusBadRequest, "expires_at must be in the future.")
		return nil, false
	}
	return &t, true
}

func (f *fakeLlmGatewayServer) handleAPIKeys(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/llm-gateway/admin/api-keys"), "/")
	id, action, _ := strings.Cut(rest, "/")

	if rest == "" && r.Method == http.MethodPost {
		f.createAPIKey(w, r)
		return
	}

	key := f.findAPIKey(id)
	live := key != nil && key.RevokedAt == nil
	notFound := func() { writeLlmError(w, http.StatusNotFound, fmt.Sprintf("api key %s not found", id)) }

	switch {
	case action == "" && r.Method == http.MethodGet:
		if key == nil {
			notFound()
			return
		}
		_ = json.NewEncoder(w).Encode(key.json())

	case action == "" && r.Method == http.MethodPatch:
		var body struct {
			Disabled *bool `json:"disabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Disabled == nil {
			http.Error(w, "Failed to deserialize the JSON body into the target type: missing field `disabled`",
				http.StatusUnprocessableEntity)
			return
		}
		if !live {
			notFound()
			return
		}
		if *body.Disabled {
			if key.DisabledAt == nil {
				now := time.Now()
				key.DisabledAt = &now
			}
		} else {
			key.DisabledAt = nil
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"disabled": *body.Disabled})

	case action == "" && r.Method == http.MethodDelete:
		if !live {
			notFound()
			return
		}
		now := time.Now()
		key.RevokedAt = &now
		_ = json.NewEncoder(w).Encode(map[string]bool{"revoked": true})

	case action == "expiry" && r.Method == http.MethodPut:
		var raw map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		field, ok := raw["expires_at"]
		if !ok {
			http.Error(w, "Failed to deserialize the JSON body into the target type: missing field `expires_at`",
				http.StatusUnprocessableEntity)
			return
		}
		var expires *time.Time
		if string(field) != "null" {
			var s string
			_ = json.Unmarshal(field, &s)
			t, ok := parseFutureTime(w, s)
			if !ok {
				return
			}
			expires = t
		}
		if !live {
			notFound()
			return
		}
		key.ExpiresAt = expires
		_ = json.NewEncoder(w).Encode(key.json())

	case action == "rotate" && r.Method == http.MethodPost:
		var raw map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		var grace int64
		if v, ok := raw["grace_period_secs"]; ok {
			_ = json.Unmarshal(v, &grace)
		}
		if grace > 7*24*60*60 {
			writeLlmError(w, http.StatusBadRequest, "grace_period_secs may be at most 604800 (7 days).")
			return
		}
		setExpiry, newExpiry := false, (*time.Time)(nil)
		if v, ok := raw["expires_at"]; ok {
			setExpiry = true
			if string(v) != "null" {
				var s string
				_ = json.Unmarshal(v, &s)
				t, ok := parseFutureTime(w, s)
				if !ok {
					return
				}
				newExpiry = t
			}
		}
		if !live {
			notFound()
			return
		}
		now := time.Now()
		stillLive := key.ExpiresAt == nil || key.ExpiresAt.After(now)
		key.PreviousSecret, key.PreviousKeyExpiresAt = "", nil
		if grace > 0 && stillLive {
			until := now.Add(time.Duration(grace) * time.Second)
			key.PreviousSecret, key.PreviousKeyExpiresAt = key.Secret, &until
		}
		key.Secret = f.newSecret()
		key.RotatedAt = &now
		if setExpiry {
			key.ExpiresAt = newExpiry
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": key.ID, "name": key.Name, "key_prefix": key.Secret[:8], "key": key.Secret,
			"expires_at": fakeLlmChronoTime(key.ExpiresAt), "rotated_at": fakeLlmChronoTime(key.RotatedAt),
			"previous_key_expires_at": fakeLlmChronoTime(key.PreviousKeyExpiresAt),
		})

	default:
		http.NotFound(w, r)
	}
}

// createAPIKey mirrors create_api_key. Callers hold f.mu.
func (f *fakeLlmGatewayServer) createAPIKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name      *string  `json:"name"`
		UserID    *string  `json:"user_id"`
		Scopes    []string `json:"scopes"`
		GroupName *string  `json:"group_name"`
		ExpiresAt *string  `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == nil {
		http.Error(w, "Failed to deserialize the JSON body into the target type: missing field `name`",
			http.StatusUnprocessableEntity)
		return
	}
	var expires *time.Time
	if body.ExpiresAt != nil {
		t, ok := parseFutureTime(w, *body.ExpiresAt)
		if !ok {
			return
		}
		expires = t
	}
	var group *string
	if body.GroupName != nil {
		if g := strings.TrimSpace(*body.GroupName); g != "" {
			group = &g
		}
	}
	if body.UserID != nil && group != nil {
		writeLlmError(w, http.StatusBadRequest,
			"An API key may be assigned to either a user or a group, not both.")
		return
	}
	var user *string
	if body.UserID != nil {
		u := strings.ToLower(*body.UserID)
		user = &u
	}
	same := func(a, b *string) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
	for _, k := range f.apiKeys {
		if k.RevokedAt == nil && k.Name == *body.Name && same(k.UserID, user) && same(k.GroupName, group) {
			scope := "in this organization"
			if user != nil {
				scope = "for this user"
			} else if group != nil {
				scope = "for this group"
			}
			writeLlmError(w, http.StatusConflict, fmt.Sprintf(
				"An API key named '%s' already exists %s. Pick a different name, or revoke the existing key to reuse it.",
				*body.Name, scope))
			return
		}
	}
	kind := "machine"
	if user != nil {
		kind = "user"
	}
	scopes := body.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	key := &fakeLlmAPIKey{
		ID: f.newID("4d4d"), Kind: kind, UserID: user, Name: *body.Name, Secret: f.newSecret(),
		Scopes: scopes, GroupName: group, ExpiresAt: expires,
	}
	f.apiKeys = append(f.apiKeys, key)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": key.ID, "name": key.Name, "key_prefix": key.Secret[:8], "key": key.Secret,
		"scopes": key.Scopes, "group_name": key.GroupName, "expires_at": fakeLlmChronoTime(key.ExpiresAt),
		"created_at": fakeLlmTime,
	})
}
