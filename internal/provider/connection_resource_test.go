// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"testing"

	frameworkresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// --- fake /servers/{id}/connect|connection endpoints -------------------------------
//
// Extends fakeRegistryServer (mcp_server_resource_test.go) with the
// service-account connection surface the barndoor_connection resource binds:
// provider-type credential validation on connect, the OAuth auth_url shape,
// as_service-only ownership, and hard-deleting disconnects.

// fakeServerDirectoryProviders maps a directory entry id to its credential
// provider. Directory ids not listed here behave as `api_key`.
var fakeServerDirectoryProviders = map[string]string{
	"dir-api-key": "api_key",
	"dir-basic":   "basic_auth",
	"dir-bearer":  "bearer_token",
	"dir-generic": "generic",
	"dir-oauth":   "oauth2",
}

type fakeConnection struct {
	ID          string `json:"id"`
	McpServerID string `json:"mcp_server_id"`
	Status      string `json:"status"`

	creds map[string]any
}

// resolveServerRef finds a non-deleted server by id or slug. Callers hold f.mu.
func (f *fakeRegistryServer) resolveServerRef(ref string) *fakeMcpServer {
	if s, ok := f.servers[ref]; ok && !s.deleted {
		return s
	}
	for _, s := range f.servers {
		if !s.deleted && s.Slug == ref {
			return s
		}
	}
	return nil
}

// connectServer serves POST /api/registry/v1/servers/{ref}/connect. Only the
// service-account flow is modeled: a request without as_service=true is
// rejected, proving the provider always sends it. Callers hold f.mu
// (handleServers took it).
func (f *fakeRegistryServer) connectServer(w http.ResponseWriter, r *http.Request, ref string) {
	if r.URL.Query().Get("as_service") != "true" {
		writeJSONError(w, http.StatusBadRequest, "fake models only as_service=true connections")
		return
	}

	s := f.resolveServerRef(ref)
	if s == nil {
		writeJSONError(w, http.StatusNotFound, "MCP server not found")
		return
	}

	var creds map[string]any
	_ = json.NewDecoder(r.Body).Decode(&creds)
	has := func(k string) bool {
		v, ok := creds[k].(string)
		return ok && v != ""
	}

	provider := fakeServerDirectoryProviders[s.McpServerDirectoryID]
	if provider == "" {
		provider = "api_key"
	}

	status := "connected"
	var authURL *string
	switch provider {
	case "oauth2":
		// OAuth: the platform creates a pending row and hands back the consent
		// URL for a human browser.
		status = "pending"
		u := "https://auth.example.com/oauth/authorize?state=fake"
		authURL = &u
	case "api_key":
		if !has("api_key") {
			writeJSONError(w, http.StatusBadRequest, "API key required for this provider")
			return
		}
	case "bearer_token":
		if !has("token") && !has("bearer_token") {
			writeJSONError(w, http.StatusBadRequest, "Bearer token required for this provider. Please provide a token in the request body.")
			return
		}
	case "basic_auth":
		if !has("username") {
			writeJSONError(w, http.StatusBadRequest, "Username required for basic auth")
			return
		}
		if !has("password") {
			writeJSONError(w, http.StatusBadRequest, "Password required for basic auth")
			return
		}
	case "generic":
		// Schema-driven; the fake accepts any credential set.
	}

	conn := f.connections[s.ID]
	if conn == nil {
		f.nextConnID++
		conn = &fakeConnection{ID: fmt.Sprintf("cccc0000-0000-0000-0000-%012d", f.nextConnID)}
		f.connections[s.ID] = conn
	}
	conn.McpServerID = s.ID
	conn.Status = status
	conn.creds = creds

	_ = json.NewEncoder(w).Encode(map[string]any{
		"connection_id": conn.ID,
		"auth_url":      authURL,
		"state":         "",
		"message":       "saved",
	})
}

// handleServerConnection serves GET/DELETE /api/registry/v1/servers/{ref}/connection.
// Callers hold f.mu (handleServers took it).
func (f *fakeRegistryServer) handleServerConnection(w http.ResponseWriter, r *http.Request, ref string) {
	if r.URL.Query().Get("as_service") != "true" {
		// Without as_service the endpoint resolves the calling user's own
		// connection; the fake holds none, so it is always a 404 — which also
		// proves the provider sends the parameter.
		writeJSONError(w, http.StatusNotFound, "Connection not found")
		return
	}

	s := f.resolveServerRef(ref)
	if s == nil {
		writeJSONError(w, http.StatusNotFound, "MCP server not found")
		return
	}

	conn := f.connections[s.ID]
	switch r.Method {
	case http.MethodGet:
		if conn == nil {
			writeJSONError(w, http.StatusNotFound, "Connection not found")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":             conn.ID,
			"mcp_server_id":  conn.McpServerID,
			"user_id":        nil,
			"application_id": nil,
			"status":         conn.Status,
			"created_at":     "2026-07-02T00:00:00Z",
		})
	case http.MethodDelete:
		if conn == nil {
			writeJSONError(w, http.StatusNotFound, "Connection not found")
			return
		}
		delete(f.connections, s.ID)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// markConnectionDeleted removes a stored connection out-of-band.
func (f *fakeRegistryServer) markConnectionDeleted(t *testing.T, serverID string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.connections[serverID]; !ok {
		t.Fatalf("fake has no connection for server %q to delete", serverID)
	}
	delete(f.connections, serverID)
}

// checkAllConnectionsDeleted is the CheckDestroy for connection tests: destroy
// must remove every connection the test created.
func checkAllConnectionsDeleted(fake *fakeRegistryServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for serverID, c := range fake.connections {
			return fmt.Errorf("connection %s (server %s) was not deleted on destroy", c.ID, serverID)
		}
		return nil
	}
}

// --- schema tests --------------------------------------------------------------

func TestConnectionResource_Metadata(t *testing.T) {
	var resp frameworkresource.MetadataResponse
	NewConnectionResource().Metadata(context.Background(),
		frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_connection"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestConnectionResource_Schema(t *testing.T) {
	var resp frameworkresource.SchemaResponse
	NewConnectionResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{"api_key", "bearer_token", "password", "additional_fields"} {
		if !resp.Schema.Attributes[attr].IsSensitive() {
			t.Errorf("%s attribute should be marked Sensitive", attr)
		}
	}
}

// --- test configurations ----------------------------------------------------------

func connectionConfig(directoryID, credentials string) string {
	return fmt.Sprintf(`
resource "barndoor_mcp_server" "seed" {
  name                    = "Connection Test Server"
  mcp_server_directory_id = %[1]q
}

resource "barndoor_connection" "test" {
  server_id = barndoor_mcp_server.seed.id
%[2]s
}
`, directoryID, credentials)
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

func TestConnectionResource_apiKeyLifecycle(t *testing.T) {
	fake := setupRegistryTest(t)
	const resourceName = "barndoor_connection.test"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllConnectionsDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: connectionConfig("dir-api-key", "\n  api_key = \"sk-test-123\"\n"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckResourceAttr(resourceName, "status", "connected"),
					resource.TestCheckResourceAttrPair(
						resourceName, "mcp_server_id", "barndoor_mcp_server.seed", "id"),
					resource.TestCheckResourceAttr(resourceName, "api_key", "sk-test-123"),
				),
			},
			{
				// A second plan over unchanged configuration must be empty —
				// write-only credentials never come back from the API.
				Config:   connectionConfig("dir-api-key", "\n  api_key = \"sk-test-123\"\n"),
				PlanOnly: true,
			},
			{
				// Changing a credential forces replacement (no update path).
				Config: connectionConfig("dir-api-key", "\n  api_key = \"sk-test-456\"\n"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(resourceName, plancheck.ResourceActionReplace),
					},
				},
			},
			{
				// Import by the server id; write-only credentials read back null.
				ResourceName: resourceName,
				ImportState:  true,
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					rs, ok := s.RootModule().Resources[resourceName]
					if !ok {
						return "", fmt.Errorf("%s not in state", resourceName)
					}
					return rs.Primary.Attributes["server_id"], nil
				},
				ImportStateVerify: true,
				ImportStateVerifyIgnore: []string{
					"api_key", "bearer_token", "username", "password", "additional_fields",
				},
			},
		},
	})
}

func TestConnectionResource_basicAuthLifecycle(t *testing.T) {
	fake := setupRegistryTest(t)
	const resourceName = "barndoor_connection.test"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllConnectionsDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: connectionConfig("dir-basic", "\n  username = \"svc\"\n  password = \"hunter2\"\n"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "status", "connected"),
					resource.TestCheckResourceAttr(resourceName, "username", "svc"),
				),
			},
		},
	})
}

func TestConnectionResource_missingCredentialIs400(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      connectionConfig("dir-api-key", ""),
				ExpectError: regexp.MustCompile(`API key required for this provider`),
			},
		},
	})
}

func TestConnectionResource_oauthProviderIsRejected(t *testing.T) {
	fake := setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		// The failed create still tracked the pending row, and the framework's
		// cleanup destroy must have removed it.
		CheckDestroy: checkAllConnectionsDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config:      connectionConfig("dir-oauth", ""),
				ExpectError: regexp.MustCompile(`OAuth MCP servers cannot be connected by Terraform`),
			},
		},
	})
}

func TestConnectionResource_outOfBandDeletePlansRecreate(t *testing.T) {
	fake := setupRegistryTest(t)
	const resourceName = "barndoor_connection.test"

	var firstID, serverID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: connectionConfig("dir-api-key", "\n  api_key = \"sk-test-123\"\n"),
				Check: func(s *terraform.State) error {
					rs, ok := s.RootModule().Resources[resourceName]
					if !ok {
						return fmt.Errorf("%s not in state", resourceName)
					}
					firstID = rs.Primary.ID
					serverID = rs.Primary.Attributes["server_id"]
					return nil
				},
			},
			{
				// Simulate an out-of-band disconnect; the refresh must drop the
				// resource and the apply must connect a replacement.
				PreConfig: func() { fake.markConnectionDeleted(t, serverID) },
				Config:    connectionConfig("dir-api-key", "\n  api_key = \"sk-test-123\"\n"),
				Check: func(s *terraform.State) error {
					rs, ok := s.RootModule().Resources[resourceName]
					if !ok {
						return fmt.Errorf("%s not in state", resourceName)
					}
					if rs.Primary.ID == firstID {
						return fmt.Errorf("expected a new connection id after out-of-band delete, still %s", firstID)
					}
					return nil
				},
			},
		},
	})
}
