// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// --- in-process fake identity-service -----------------------------------------
//
// fakeIdentityServer emulates the identity-service public IdP surface the
// provider binds (`/api/identity/public/v1/idp/connection|config|settings`,
// BCP-3260) faithfully enough to drive real plan/apply cycles: the singleton
// connection whose POST answers 409 when one exists and whose GET stays 200
// with a null alias when none does, the PUT that mutates the connection in
// place (skipping absent/empty optional fields, exactly like production's
// update_idp_connection), write-only client credentials surfaced only as
// *_configured booleans, the role-mapping upsert-or-delete keyed on a truthy
// admin_group (404 without a connection), and the read-only settings view.
// Errors use FastAPI's HTTPException shape (`{"detail": "..."}`).

// fakeIdentityOrgID matches the BARNDOOR_ORGANIZATION_ID set by
// setupIdentityTest.
const fakeIdentityOrgID = "22222222-3333-4444-5555-666666666666"

// fakeIdentityIdpAlias mirrors production's `<org-alias>-idp` naming.
const fakeIdentityIdpAlias = "acme-idp"

// fakeIdpConnection is the platform's stored view of the org's IdP.
// Credentials are stored to let tests assert the write-only round trip; they
// are never rendered into a response.
type fakeIdpConnection struct {
	DisplayName  string
	Issuer       string
	ClientID     string
	ClientSecret string
	AuthzURL     string
	TokenURL     string
	UserinfoURL  string
	JwksURL      string
	Scopes       string
}

type fakeIdentityServer struct {
	mu sync.Mutex

	// connection is nil until created — the org has no IdP.
	connection *fakeIdpConnection
	// orgDomain mirrors the Keycloak organization's domain: pre-existing, or
	// updated when a connection write carries `domain`.
	orgDomain string
	// adminGroup is the role-mapping mapper's group claim; empty = no mapper.
	adminGroup string

	// createCount counts connection POSTs, letting tests assert that updates
	// mutate in place (no delete + recreate) and that out-of-band deletion
	// leads to a recreate.
	createCount int

	// settings backs the read-only /idp/settings view.
	settings fakeIdpSettings
}

type fakeIdpSettings struct {
	IdpRoleBindingOnly bool
	EnforceSso         bool
	BreakGlassEmail    string
	BreakGlassUserID   string
}

func newFakeIdentityServer() *fakeIdentityServer {
	return &fakeIdentityServer{}
}

func (f *fakeIdentityServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			writeToken(w)
		case "/api/identity/public/v1/idp/connection":
			f.handleConnection(w, r)
		case "/api/identity/public/v1/idp/config":
			f.handleRoleMapping(w, r)
		case "/api/identity/public/v1/idp/settings":
			f.handleSettings(w, r)
		default:
			http.NotFound(w, r)
		}
	}
}

// setupIdentityTest starts the fake identity-service and points the provider's
// BARNDOOR_* environment at it. REST traffic and token minting ride the same
// httptest server, exactly like production shares one platform host.
func setupIdentityTest(t *testing.T) *fakeIdentityServer {
	t.Helper()

	fake := newFakeIdentityServer()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	t.Setenv("BARNDOOR_BASE_URL", srv.URL)
	t.Setenv("BARNDOOR_TOKEN_URL", srv.URL+"/token")
	t.Setenv("BARNDOOR_CLIENT_ID", "test-client")
	t.Setenv("BARNDOOR_CLIENT_SECRET", "test-secret")
	t.Setenv("BARNDOOR_ORGANIZATION_ID", fakeIdentityOrgID)

	return fake
}

// writeJSONDetail renders FastAPI's HTTPException error shape.
func writeJSONDetail(w http.ResponseWriter, status int, detail string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"detail": detail})
}

// --- connection ------------------------------------------------------------------

// idpConnectionBody mirrors production's IdpConnectionConfig request model,
// including the fields the provider deliberately never sends.
type idpConnectionBody struct {
	DisplayName  string  `json:"display_name"`
	Issuer       string  `json:"issuer"`
	ClientID     *string `json:"client_id"`
	ClientSecret *string `json:"client_secret"`
	AuthzURL     string  `json:"authorization_url"`
	TokenURL     string  `json:"token_url"`
	UserinfoURL  *string `json:"userinfo_url"`
	JwksURL      string  `json:"jwks_url"`
	LogoutURL    *string `json:"logout_url"`
	Scopes       *string `json:"scopes"`
	UseDiscovery *bool   `json:"use_discovery"`
	Domain       *string `json:"domain"`
}

func (f *fakeIdentityServer) handleConnection(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.Method {
	case http.MethodGet:
		f.writeConnection(w)
	case http.MethodPost:
		f.createConnection(w, r)
	case http.MethodPut:
		f.updateConnection(w, r)
	case http.MethodDelete:
		if f.connection == nil {
			writeJSONDetail(w, http.StatusNotFound, "No Identity Provider configured for this organization")
			return
		}
		// Deleting the Keycloak IdP takes its mappers (the role mapping)
		// with it.
		f.connection = nil
		f.adminGroup = ""
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// writeConnection renders the IdpConnectionResponse shape. Like production's
// get_idp_connection, absence is a 200 with `configured: false` and a null
// alias (never a 404), and credentials appear only as booleans. Callers hold
// f.mu.
func (f *fakeIdentityServer) writeConnection(w http.ResponseWriter) {
	if f.connection == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"configured":               false,
			"alias":                    nil,
			"expected_alias":           fakeIdentityIdpAlias,
			"client_id_configured":     false,
			"client_secret_configured": false,
			"domain":                   nullableString(f.orgDomain),
		})
		return
	}
	c := f.connection
	_ = json.NewEncoder(w).Encode(map[string]any{
		"configured":               c.Issuer != "",
		"alias":                    fakeIdentityIdpAlias,
		"expected_alias":           fakeIdentityIdpAlias,
		"display_name":             c.DisplayName,
		"issuer":                   nullableString(c.Issuer),
		"authorization_url":        nullableString(c.AuthzURL),
		"token_url":                nullableString(c.TokenURL),
		"userinfo_url":             nullableString(c.UserinfoURL),
		"jwks_url":                 nullableString(c.JwksURL),
		"logout_url":               nil, // production never writes it (no logout propagation)
		"scopes":                   nullableString(c.Scopes),
		"client_id_configured":     c.ClientID != "",
		"client_secret_configured": c.ClientSecret != "",
		"domain":                   nullableString(f.orgDomain),
	})
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// createConnection mirrors production's create_idp_connection: 409 when the
// org already has an IdP, 400 without both client credentials, the pydantic
// scopes default, and the org-domain fallback. Callers hold f.mu.
func (f *fakeIdentityServer) createConnection(w http.ResponseWriter, r *http.Request) {
	if f.connection != nil {
		writeJSONDetail(w, http.StatusConflict,
			"An Identity Provider is already configured for this organization. Use PUT to update it.")
		return
	}

	var body idpConnectionBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONDetail(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if body.ClientID == nil || *body.ClientID == "" || body.ClientSecret == nil || *body.ClientSecret == "" {
		writeJSONDetail(w, http.StatusBadRequest,
			"Client ID and client secret are required when creating a new IdP connection")
		return
	}

	// Pydantic default when the request omits scopes.
	scopes := "openid profile email"
	if body.Scopes != nil && *body.Scopes != "" {
		scopes = *body.Scopes
	}
	if body.Domain != nil && *body.Domain != "" {
		f.orgDomain = *body.Domain
	}

	f.createCount++
	f.connection = &fakeIdpConnection{
		DisplayName:  body.DisplayName,
		Issuer:       body.Issuer,
		ClientID:     *body.ClientID,
		ClientSecret: *body.ClientSecret,
		AuthzURL:     body.AuthzURL,
		TokenURL:     body.TokenURL,
		UserinfoURL:  derefOrEmpty(body.UserinfoURL),
		JwksURL:      body.JwksURL,
		Scopes:       scopes,
	}
	w.WriteHeader(http.StatusCreated)
	f.writeConnection(w)
}

// updateConnection mirrors production's update_idp_connection: an in-place
// mutation (404 without a connection) that always writes the endpoints and
// display name but skips absent/empty optional fields — credentials, scopes,
// userinfo_url, and domain keep their current values when omitted. Callers
// hold f.mu.
func (f *fakeIdentityServer) updateConnection(w http.ResponseWriter, r *http.Request) {
	if f.connection == nil {
		writeJSONDetail(w, http.StatusNotFound,
			"No Identity Provider configured for this organization. Contact support to set up SSO.")
		return
	}

	var body idpConnectionBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONDetail(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	c := f.connection
	c.DisplayName = body.DisplayName
	c.Issuer = body.Issuer
	c.AuthzURL = body.AuthzURL
	c.TokenURL = body.TokenURL
	c.JwksURL = body.JwksURL
	if v := derefOrEmpty(body.UserinfoURL); v != "" {
		c.UserinfoURL = v
	}
	if v := derefOrEmpty(body.Scopes); v != "" {
		c.Scopes = v
	}
	if v := derefOrEmpty(body.ClientID); v != "" {
		c.ClientID = v
	}
	if v := derefOrEmpty(body.ClientSecret); v != "" {
		c.ClientSecret = v
	}
	if v := derefOrEmpty(body.Domain); v != "" {
		f.orgDomain = v
	}

	f.writeConnection(w)
}

func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// seedIdpConnection plants a connection out-of-band, as if an admin had
// configured SSO in the portal.
func (f *fakeIdentityServer) seedIdpConnection(conn fakeIdpConnection) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCount++
	f.connection = &conn
}

// markIdpConnectionDeleted removes the connection out-of-band.
func (f *fakeIdentityServer) markIdpConnectionDeleted(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.connection == nil {
		t.Fatal("fake has no IdP connection to delete")
	}
	f.connection = nil
	f.adminGroup = ""
}

// --- role mapping ------------------------------------------------------------------

func (f *fakeIdentityServer) handleRoleMapping(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.Method {
	case http.MethodGet:
		f.writeRoleMapping(w)
	case http.MethodPut:
		if f.connection == nil {
			writeJSONDetail(w, http.StatusNotFound, "No Identity Provider configured for this organization")
			return
		}
		var body struct {
			AdminGroup *string `json:"admin_group"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONDetail(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		// Production: a truthy admin_group upserts the mapper; empty or null
		// deletes it.
		f.adminGroup = derefOrEmpty(body.AdminGroup)
		f.writeRoleMapping(w)
	default:
		http.NotFound(w, r)
	}
}

// writeRoleMapping renders the IdpRoleMappingResponse shape; without a
// connection every field is null (production returns an empty response, not
// a 404). Callers hold f.mu.
func (f *fakeIdentityServer) writeRoleMapping(w http.ResponseWriter) {
	if f.connection == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"idp_alias":        nil,
			"idp_display_name": nil,
			"admin_group":      nil,
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"idp_alias":        fakeIdentityIdpAlias,
		"idp_display_name": f.connection.DisplayName,
		"admin_group":      nullableString(f.adminGroup),
	})
}

// --- settings ------------------------------------------------------------------------

func (f *fakeIdentityServer) handleSettings(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.Method != http.MethodGet {
		// PUT /idp/settings is portal-only; the public surface has no other
		// method.
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"idp_role_binding_only": f.settings.IdpRoleBindingOnly,
		"enforce_sso":           f.settings.EnforceSso,
		"break_glass_email":     nullableString(f.settings.BreakGlassEmail),
		"break_glass_user_id":   nullableString(f.settings.BreakGlassUserID),
	})
}
