// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework-jsontypes/jsontypes"
	frameworkresource "github.com/hashicorp/terraform-plugin-framework/resource"
	frameworkschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// --- in-process fake registry-service --------------------------------------------
//
// fakeRegistryServer emulates the registry public REST surface the provider
// binds (`/api/registry/v1/servers`) faithfully enough to drive real
// plan/apply cycles: exclude-unset PUT semantics, duplicate-name 409s,
// soft-delete-means-404 reads, idempotent deletes, and obfuscated credential
// echoes.

type fakeMcpServer struct {
	ID                     string   `json:"id"`
	Name                   string   `json:"name"`
	Slug                   string   `json:"slug"`
	Status                 string   `json:"status"`
	McpServerDirectoryID   string   `json:"mcp_server_directory_id"`
	OauthBaseURLOverride   *string  `json:"oauth_base_url_override"`
	UsesManagedCredentials *bool    `json:"uses_managed_credentials"`
	Scopes                 []string `json:"scopes"`
	// ClientID holds the obfuscated echo the real API returns; the real value
	// is never stored, mirroring production.
	ClientID *string `json:"client_id"`

	deleted bool
}

type fakeRegistryServer struct {
	mu      sync.Mutex
	nextID  int
	servers map[string]*fakeMcpServer

	// agents backs the /agents endpoints; see agent_resource_test.go.
	nextAgentID int
	agents      map[string]*fakeAgent

	// connections backs the /servers/{id}/connect|connection endpoints
	// (service-account-owned rows, keyed by server id); see
	// connection_resource_test.go.
	nextConnID  int
	connections map[string]*fakeConnection
}

func newFakeRegistryServer() *fakeRegistryServer {
	return &fakeRegistryServer{
		servers:     map[string]*fakeMcpServer{},
		agents:      map[string]*fakeAgent{},
		connections: map[string]*fakeConnection{},
	}
}

func obfuscate(s string) string {
	if len(s) <= 4 {
		return "****"
	}
	return s[:2] + "..." + s[len(s)-2:]
}

func writeJSONError(w http.ResponseWriter, status int, detail string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"detail": detail})
}

func (f *fakeRegistryServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			writeToken(w)
		case strings.HasPrefix(r.URL.Path, "/api/registry/v1/servers"):
			f.handleServers(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/registry/v1/agents"):
			f.handleAgents(w, r)
		default:
			http.NotFound(w, r)
		}
	}
}

func (f *fakeRegistryServer) handleServers(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/registry/v1/servers"), "/")

	switch {
	case id == "" && r.Method == http.MethodPost:
		f.createServer(w, r)
	case id == "" && r.Method == http.MethodGet:
		f.listServers(w, r)
	case strings.HasPrefix(id, "by-slug/") && r.Method == http.MethodGet:
		f.getServerBySlug(w, strings.TrimPrefix(id, "by-slug/"))
	case strings.HasSuffix(id, "/connect") && r.Method == http.MethodPost:
		f.connectServer(w, r, strings.TrimSuffix(id, "/connect"))
	case strings.HasSuffix(id, "/connection"):
		f.handleServerConnection(w, r, strings.TrimSuffix(id, "/connection"))
	case id != "" && r.Method == http.MethodGet:
		s, ok := f.servers[id]
		if !ok || s.deleted {
			writeJSONError(w, http.StatusNotFound, "MCP server not found")
			return
		}
		_ = json.NewEncoder(w).Encode(s)
	case id != "" && r.Method == http.MethodPut:
		f.updateServer(w, r, id)
	case id != "" && r.Method == http.MethodDelete:
		s, ok := f.servers[id]
		if !ok {
			writeJSONError(w, http.StatusNotFound, "MCP server not found")
			return
		}
		// Idempotent soft-delete: a re-delete is a silent 204, like production.
		s.deleted = true
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeRegistryServer) nameTaken(name, excludeID string) bool {
	for _, s := range f.servers {
		if s.ID != excludeID && !s.deleted && strings.EqualFold(strings.TrimSpace(s.Name), strings.TrimSpace(name)) {
			return true
		}
	}
	return false
}

func (f *fakeRegistryServer) createServer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name                    string          `json:"name"`
		McpServerDirectoryID    string          `json:"mcp_server_directory_id"`
		OauthBaseURLOverride    *string         `json:"oauth_base_url_override"`
		UsesManagedCredentials  *bool           `json:"uses_managed_credentials"`
		Scopes                  []string        `json:"scopes"`
		ClientID                *string         `json:"client_id"`
		ClientSecret            *string         `json:"client_secret"`
		PrepopulatedCredentials json.RawMessage `json:"prepopulated_credentials"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if body.McpServerDirectoryID == "" {
		writeJSONError(w, http.StatusUnprocessableEntity, "mcp_server_directory_id is required")
		return
	}
	if f.nameTaken(body.Name, "") {
		writeJSONError(w, http.StatusConflict,
			fmt.Sprintf("An MCP server named '%s' already exists in this organization", body.Name))
		return
	}

	f.nextID++
	id := fmt.Sprintf("00000000-0000-0000-0000-%012d", f.nextID)
	status := "pending"
	if body.ClientID != nil || (body.UsesManagedCredentials != nil && *body.UsesManagedCredentials) ||
		len(body.PrepopulatedCredentials) > 0 {
		status = "active"
	}
	s := &fakeMcpServer{
		ID:                     id,
		Name:                   body.Name,
		Slug:                   strings.ToLower(strings.ReplaceAll(strings.TrimSpace(body.Name), " ", "-")),
		Status:                 status,
		McpServerDirectoryID:   body.McpServerDirectoryID,
		OauthBaseURLOverride:   body.OauthBaseURLOverride,
		UsesManagedCredentials: body.UsesManagedCredentials,
		Scopes:                 body.Scopes,
	}
	if body.ClientID != nil {
		ob := obfuscate(*body.ClientID)
		s.ClientID = &ob
	}
	f.servers[id] = s
	// Production returns only the id from create.
	_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "connection_id": nil, "auth_url": nil})
}

func (f *fakeRegistryServer) updateServer(w http.ResponseWriter, r *http.Request, id string) {
	s, ok := f.servers[id]
	if !ok || s.deleted {
		writeJSONError(w, http.StatusNotFound, "MCP server not found")
		return
	}

	// Decode to a key-presence map to emulate Pydantic's exclude_unset: only
	// keys present in the payload are applied (with the deliberate
	// uses_managed_credentials exception below, mirroring production).
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	if v, ok := raw["name"]; ok {
		var name string
		_ = json.Unmarshal(v, &name)
		if name != "" {
			if f.nameTaken(name, s.ID) {
				writeJSONError(w, http.StatusConflict,
					fmt.Sprintf("An MCP server named '%s' already exists in this organization", name))
				return
			}
			s.Name = name
		}
	}
	if v, ok := raw["mcp_server_directory_id"]; ok {
		var dir string
		_ = json.Unmarshal(v, &dir)
		if dir != "" {
			s.McpServerDirectoryID = dir
		}
	}
	if v, ok := raw["oauth_base_url_override"]; ok {
		var override *string
		_ = json.Unmarshal(v, &override)
		s.OauthBaseURLOverride = override
	}
	if v, ok := raw["scopes"]; ok {
		var scopes []string
		_ = json.Unmarshal(v, &scopes)
		s.Scopes = scopes
	}
	if v, ok := raw["client_id"]; ok {
		var clientID *string
		_ = json.Unmarshal(v, &clientID)
		// Production ignores obfuscated echoes; a real value re-activates.
		if clientID != nil && !strings.Contains(*clientID, "...") {
			ob := obfuscate(*clientID)
			s.ClientID = &ob
			s.Status = "active"
		}
	}
	// Mirrors production: uses_managed_credentials is applied from the parsed
	// payload unconditionally (absent resets to null).
	var umc *bool
	if v, ok := raw["uses_managed_credentials"]; ok {
		_ = json.Unmarshal(v, &umc)
	}
	s.UsesManagedCredentials = umc
	if umc != nil && *umc {
		s.Status = "active"
	}

	_ = json.NewEncoder(w).Encode(s)
}

// markServerDeleted soft-deletes a stored server out-of-band.
func (f *fakeRegistryServer) markServerDeleted(t *testing.T, id string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.servers[id]
	if !ok {
		t.Fatalf("fake has no server %q to delete", id)
	}
	s.deleted = true
}

// setupRegistryTest starts the fake registry and points the provider's
// BARNDOOR_* environment at it. REST traffic and token minting ride the same
// httptest server, exactly like production shares one platform host.
func setupRegistryTest(t *testing.T) *fakeRegistryServer {
	t.Helper()

	fake := newFakeRegistryServer()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	t.Setenv("BARNDOOR_BASE_URL", srv.URL)
	t.Setenv("BARNDOOR_TOKEN_URL", srv.URL+"/token")
	t.Setenv("BARNDOOR_CLIENT_ID", "test-client")
	t.Setenv("BARNDOOR_CLIENT_SECRET", "test-secret")
	t.Setenv("BARNDOOR_ORGANIZATION_ID", "org-123")

	return fake
}

// checkAllServersDeleted is the CheckDestroy for MCP server tests: destroy
// must soft-delete every server the test created.
func checkAllServersDeleted(fake *fakeRegistryServer) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for id, s := range fake.servers {
			if !s.deleted {
				return fmt.Errorf("server %s (%s) was not deleted on destroy", id, s.Name)
			}
		}
		return nil
	}
}

func mustNormalized(t *testing.T, s string) jsontypes.Normalized {
	t.Helper()
	return jsontypes.NewNormalizedValue(s)
}

// --- schema tests --------------------------------------------------------------

func newMcpServerSchema(t *testing.T) frameworkschema.Schema {
	t.Helper()
	var resp frameworkresource.SchemaResponse
	NewMcpServerResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}
	return resp.Schema
}

func TestMcpServerResource_Metadata(t *testing.T) {
	var resp frameworkresource.MetadataResponse
	NewMcpServerResource().Metadata(context.Background(),
		frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_mcp_server"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestMcpServerResource_Schema(t *testing.T) {
	s := newMcpServerSchema(t)

	for _, attr := range []string{
		"id", "name", "mcp_server_directory_id", "slug", "status", "oauth_base_url_override",
		"uses_managed_credentials", "client_id", "client_secret", "scopes", "meta",
		"prepopulated_credentials", "cascaded_fields",
	} {
		if _, ok := s.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}

	for _, sensitive := range []string{"client_secret", "prepopulated_credentials"} {
		if !s.Attributes[sensitive].IsSensitive() {
			t.Errorf("%s must be Sensitive", sensitive)
		}
	}
	for _, computed := range []string{"id", "slug", "status"} {
		if !s.Attributes[computed].IsComputed() {
			t.Errorf("%s should be Computed", computed)
		}
	}
	for _, required := range []string{"name", "mcp_server_directory_id"} {
		if !s.Attributes[required].IsRequired() {
			t.Errorf("%s should be Required", required)
		}
	}
}

// --- conversion tests ------------------------------------------------------------

func TestBuildMcpServerWriteRequest_ExplicitNulls(t *testing.T) {
	// Convergence contract: oauth_base_url_override, uses_managed_credentials,
	// and scopes are always serialized (null when unset) so removing them from
	// configuration clears them server-side under exclude-unset semantics.
	plan := &mcpServerResourceModel{
		Name:                 types.StringValue("Acme"),
		McpServerDirectoryID: types.StringValue("dir-1"),
	}
	body, err := buildMcpServerWriteRequest(context.Background(), plan, false)
	if err != nil {
		t.Fatalf("buildMcpServerWriteRequest: %v", err)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, alwaysSent := range []string{"oauth_base_url_override", "uses_managed_credentials", "scopes"} {
		v, ok := keys[alwaysSent]
		if !ok {
			t.Errorf("%s must be serialized even when unset", alwaysSent)
			continue
		}
		if string(v) != "null" {
			t.Errorf("%s = %s, want null", alwaysSent, v)
		}
	}
	for _, omitted := range []string{"client_id", "client_secret", "meta", "prepopulated_credentials", "cascaded_fields"} {
		if _, ok := keys[omitted]; ok {
			t.Errorf("%s must be omitted when unset", omitted)
		}
	}
}

func TestBuildMcpServerWriteRequest_CreateOnlyFields(t *testing.T) {
	plan := &mcpServerResourceModel{
		Name:                    types.StringValue("Acme"),
		McpServerDirectoryID:    types.StringValue("dir-1"),
		PrepopulatedCredentials: mustNormalized(t, `{"api_key":"k"}`),
		CascadedFields:          mustStringList(t, "api_key"),
	}

	createBody, err := buildMcpServerWriteRequest(context.Background(), plan, true)
	if err != nil {
		t.Fatalf("create body: %v", err)
	}
	if len(createBody.PrepopulatedCredentials) == 0 || len(createBody.CascadedFields) != 1 {
		t.Errorf("create body must carry prepopulated_credentials and cascaded_fields: %+v", createBody)
	}

	updateBody, err := buildMcpServerWriteRequest(context.Background(), plan, false)
	if err != nil {
		t.Fatalf("update body: %v", err)
	}
	if len(updateBody.PrepopulatedCredentials) != 0 || updateBody.CascadedFields != nil {
		t.Errorf("update body must not carry create-only fields: %+v", updateBody)
	}
}

func TestApplyMcpServerResponse_WriteOnlyFieldsFollowConfig(t *testing.T) {
	prior := &mcpServerResourceModel{
		ClientID:                types.StringValue("real-client-id"),
		ClientSecret:            types.StringValue("real-secret"),
		Meta:                    mustNormalized(t, `{"access_context":{"region":"us"}}`),
		PrepopulatedCredentials: mustNormalized(t, `{"api_key":"k"}`),
		CascadedFields:          mustStringList(t, "api_key"),
		Scopes:                  types.ListNull(types.StringType),
	}
	override := "https://auth.acme.example"
	server := &mcpServerResponse{
		ID:                   "srv-1",
		Name:                 "Acme",
		Slug:                 "acme",
		Status:               "active",
		McpServerDirectoryID: "dir-1",
		OauthBaseURLOverride: &override,
	}

	state, err := applyMcpServerResponse(context.Background(), server, prior)
	if err != nil {
		t.Fatalf("applyMcpServerResponse: %v", err)
	}

	if state.ClientID.ValueString() != "real-client-id" || state.ClientSecret.ValueString() != "real-secret" {
		t.Error("credentials must be carried from the prior model, never from the API's obfuscated echo")
	}
	if state.Meta.IsNull() || state.PrepopulatedCredentials.IsNull() {
		t.Error("meta/prepopulated_credentials must follow configuration")
	}
	if !state.Scopes.IsNull() {
		t.Error("empty server scopes with a null prior must settle to null, not []")
	}
	if state.Status.ValueString() != "active" || state.Slug.ValueString() != "acme" {
		t.Errorf("computed fields mismapped: %+v", state)
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

func mcpServerConfig(name, extra string) string {
	return fmt.Sprintf(`
resource "barndoor_mcp_server" "test" {
  name                    = %q
  mcp_server_directory_id = "11111111-1111-1111-1111-111111111111"
%s
}
`, name, extra)
}

func TestMcpServerResource_basicLifecycle(t *testing.T) {
	fake := setupRegistryTest(t)
	resourceName := "barndoor_mcp_server.test"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllServersDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: mcpServerConfig("tf-test-server", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckResourceAttr(resourceName, "name", "tf-test-server"),
					resource.TestCheckResourceAttr(resourceName, "slug", "tf-test-server"),
					resource.TestCheckResourceAttr(resourceName, "status", "pending"),
					resource.TestCheckNoResourceAttr(resourceName, "scopes.#"),
				),
			},
			{
				// Rename + add scopes + a credential: converges in-place and
				// activates the server.
				Config: mcpServerConfig("tf-test-server-renamed", `
  scopes    = ["read", "write"]
  client_id = "tenant-client-id"
`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "name", "tf-test-server-renamed"),
					resource.TestCheckResourceAttr(resourceName, "scopes.#", "2"),
					resource.TestCheckResourceAttr(resourceName, "scopes.0", "read"),
					resource.TestCheckResourceAttr(resourceName, "status", "active"),
					// State must keep the configured value, not the API's mask.
					resource.TestCheckResourceAttr(resourceName, "client_id", "tenant-client-id"),
				),
			},
			{
				// Dropping scopes must clear them server-side (explicit null in
				// the PUT body) and settle state back to null — no perpetual diff.
				Config: mcpServerConfig("tf-test-server-renamed", `
  client_id = "tenant-client-id"
`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(resourceName, "scopes.#"),
				),
			},
			{
				Config: mcpServerConfig("tf-test-server-renamed", `
  client_id = "tenant-client-id"
`),
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateVerify: true,
				// Write-only attributes cannot survive an import (the API never
				// returns their real values).
				ImportStateVerifyIgnore: []string{
					"client_id", "client_secret", "meta", "prepopulated_credentials", "cascaded_fields",
				},
			},
		},
	})
}

func TestMcpServerResource_nameConflict(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: mcpServerConfig("dupe-name", "") + `
resource "barndoor_mcp_server" "dupe" {
  name                    = "dupe-name"
  mcp_server_directory_id = "11111111-1111-1111-1111-111111111111"
  depends_on              = [barndoor_mcp_server.test]
}
`,
				ExpectError: regexp.MustCompile(`(?s)name or slug already in use.*dupe-name`),
			},
		},
	})
}

func TestMcpServerResource_outOfBandDeletePlansRecreate(t *testing.T) {
	fake := setupRegistryTest(t)
	resourceName := "barndoor_mcp_server.test"

	var firstID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: mcpServerConfig("tf-test-oob", ""),
				Check: func(s *terraform.State) error {
					rs, ok := s.RootModule().Resources[resourceName]
					if !ok {
						return fmt.Errorf("%s not in state", resourceName)
					}
					firstID = rs.Primary.ID
					return nil
				},
			},
			{
				// Simulate an out-of-band delete; the refresh must drop the
				// resource and the apply must create a replacement.
				PreConfig: func() { fake.markServerDeleted(t, firstID) },
				Config:    mcpServerConfig("tf-test-oob", ""),
				Check: func(s *terraform.State) error {
					rs, ok := s.RootModule().Resources[resourceName]
					if !ok {
						return fmt.Errorf("%s not in state", resourceName)
					}
					if rs.Primary.ID == firstID {
						return fmt.Errorf("expected a new server id after out-of-band delete, still %s", firstID)
					}
					return nil
				},
			},
		},
	})
}

func TestMcpServerResource_prepopulatedCredentialsCreate(t *testing.T) {
	fake := setupRegistryTest(t)
	resourceName := "barndoor_mcp_server.test"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllServersDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: mcpServerConfig("tf-test-nonoauth", `
  prepopulated_credentials = jsonencode({ api_key = "k-123" })
  cascaded_fields          = ["api_key"]
`),
				Check: resource.ComposeAggregateTestCheckFunc(
					// Non-OAuth credentials activate the server at create.
					resource.TestCheckResourceAttr(resourceName, "status", "active"),
					resource.TestCheckResourceAttr(resourceName, "cascaded_fields.0", "api_key"),
				),
			},
		},
	})
}

func TestMcpServerResource_validateConfigRejectsBadInput(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      mcpServerConfig("tf-test-badmeta", "\n  meta = jsonencode([1, 2])\n"),
				ExpectError: regexp.MustCompile(`must be a JSON object`),
			},
			{
				Config:      mcpServerConfig("tf-test-badcascade", "\n  cascaded_fields = [\"api_key\"]\n"),
				ExpectError: regexp.MustCompile(`cascaded_fields requires prepopulated_credentials`),
			},
		},
	})
}
