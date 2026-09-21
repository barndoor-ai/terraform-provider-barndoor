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
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework-jsontypes/jsontypes"
	frameworkresource "github.com/hashicorp/terraform-plugin-framework/resource"
	frameworkschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
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
	// PublishedAt mirrors the one-way publish stamp (BCP-3039): set exactly
	// once by POST /servers/{id}/publish, never cleared.
	PublishedAt *string `json:"published_at"`
	// AttentionTier and PublishBlockers mirror the two read-only publish-gate
	// fields the registry computes per read (BCP-3815). The fake deliberately
	// does NOT model their derivation: it is flag-resolved and lives in
	// registry-service, and the provider passes both through without
	// interpreting them, so simulating it here would only encode a guess. They
	// are seeded to the documented flag-off shape at create
	// (fakeDefaultAttentionTier / null blockers) and driven explicitly by
	// setServerAttention, which is the instrument the mapping tests need.
	AttentionTier   *string   `json:"attention_tier"`
	PublishBlockers *[]string `json:"publish_blockers"`

	deleted bool
	// hasActivePolicy emulates the publish route's ACTIVE-policy precondition
	// (verified against policy-service in production). Fresh servers have no
	// policy, so publish 422s until a test grants one via grantActivePolicy.
	hasActivePolicy bool
}

type fakeRegistryServer struct {
	mu      sync.Mutex
	nextID  int
	servers map[string]*fakeMcpServer

	// publishes counts state-changing publishes, so each one gets its own
	// timestamp (see fakePublishedAtFor). publishAttempts counts every call to
	// the publish endpoint, rejected or not — the observable that tells a
	// retried publish from a single attempt. grantPolicyOnAttempt, when
	// non-zero, grants every server an ACTIVE policy once that many attempts
	// have been made — a deterministic stand-in for "the policy lands during
	// the same apply".
	publishes            int
	publishAttempts      int
	grantPolicyOnAttempt int

	// failNextServerGet makes the next GET /servers/{id} return a 500 and
	// resets itself, so a test can drive the narrow window where create
	// succeeded but the follow-up read did not.
	failNextServerGet bool

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
		case strings.HasPrefix(r.URL.Path, "/api/registry/v1/server-directory"):
			f.handleServerDirectory(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/registry/v1/agent-directory"):
			f.handleAgentDirectory(w, r)
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
	case strings.HasSuffix(id, "/publish") && r.Method == http.MethodPost:
		f.publishServer(w, strings.TrimSuffix(id, "/publish"))
	case strings.HasSuffix(id, "/connection"):
		f.handleServerConnection(w, r, strings.TrimSuffix(id, "/connection"))
	case id != "" && r.Method == http.MethodGet:
		if f.failNextServerGet {
			f.failNextServerGet = false
			writeJSONError(w, http.StatusInternalServerError, "registry is having a moment")
			return
		}
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
	defaultTier := fakeDefaultAttentionTier
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
		AttentionTier:          &defaultTier,
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

// fakePublishedAtFor is the deterministic stamp the fake's publish endpoint
// sets on the nth state-changing publish (n is 1-based). Per-publish rather
// than one shared constant so a state assertion can tell "republished THIS
// server" from "carried an earlier stamp forward" — the distinction
// TestMcpServerPublicationResource_repointForcesReplace and
// _unpublishedOutOfBandRepublishes turn on.
func fakePublishedAtFor(n int) string {
	return fmt.Sprintf("2026-08-19T00:00:%02dZ", n)
}

// publishServer emulates POST /servers/{id}/publish: one-way, idempotent, and
// gated (in production order) on operational availability then an ACTIVE
// policy. Re-publishing an already-published server is a no-op success that
// skips the gates, like production.
//
// The operational-availability gate here is deliberately LOOSER than
// production, which requires status=active AND (requires_auth=false OR source
// in {embedded,local} OR a connected tenant service-account connection). The
// fake has no directory-entry model to express that second term, so a server
// activated by a client_id publishes cleanly here while an ordinary OAuth
// server would 422 in production. Keep that in mind before reading a green
// unit run as "this config applies against a real environment" — it is why
// the documented example and the acceptance-test setup both need explicit
// credentials or an embedded/local directory entry.
func (f *fakeRegistryServer) publishServer(w http.ResponseWriter, id string) {
	f.publishAttempts++
	s, ok := f.servers[id]
	if !ok || s.deleted {
		writeJSONError(w, http.StatusNotFound, "MCP server not found")
		return
	}
	if f.grantPolicyOnAttempt > 0 && f.publishAttempts >= f.grantPolicyOnAttempt {
		s.hasActivePolicy = true
	}
	if s.PublishedAt == nil {
		if s.Status != "active" {
			writeJSONError(w, http.StatusUnprocessableEntity, "Cannot publish: server is not operationally available")
			return
		}
		if !s.hasActivePolicy {
			writeJSONError(w, http.StatusUnprocessableEntity, "Cannot publish: server has no ACTIVE policy")
			return
		}
		f.publishes++
		ts := fakePublishedAtFor(f.publishes)
		s.PublishedAt = &ts
	}
	_ = json.NewEncoder(w).Encode(s)
}

// publishAttemptCount returns how many times the publish endpoint was called.
func (f *fakeRegistryServer) publishAttemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.publishAttempts
}

// grantActivePolicy marks a stored server as having an ACTIVE policy, standing
// in for the barndoor_policy resource a real policy_ids reference waits on.
func (f *fakeRegistryServer) grantActivePolicy(t *testing.T, id string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.servers[id]
	if !ok {
		t.Fatalf("fake has no server %q to grant a policy to", id)
	}
	s.hasActivePolicy = true
}

// fakeDefaultAttentionTier is the tier the fake stamps on a freshly created
// server: a ready-but-unpublished server with the `mcp-server-publishing` flag
// off, which is the shape of every server the fake serves.
const fakeDefaultAttentionTier = "available"

// setServerAttention overwrites a stored server's publish-gate read fields
// out-of-band, standing in for the registry recomputing them between reads.
// Both parameters are pointers so a test can drive the three distinct wire
// shapes the provider must keep apart: nil blockers (JSON null — the gate was
// not evaluated), a pointer to an empty slice (`[]` — a publish would be
// accepted), and a pointer to a populated slice.
func (f *fakeRegistryServer) setServerAttention(t *testing.T, id string, tier *string, blockers *[]string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.servers[id]
	if !ok {
		t.Fatalf("fake has no server %q to set attention fields on", id)
	}
	s.AttentionTier = tier
	s.PublishBlockers = blockers
}

// failServerGetOnce arms a one-shot 500 on the next GET /servers/{id}.
func (f *fakeRegistryServer) failServerGetOnce() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNextServerGet = true
}

// serverPublishedAt returns the stored publish stamp (nil = unpublished).
func (f *fakeRegistryServer) serverPublishedAt(t *testing.T, id string) *string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.servers[id]
	if !ok {
		t.Fatalf("fake has no server %q", id)
	}
	return s.PublishedAt
}

// unpublishServer clears a stored server's publish stamp out-of-band. There is
// deliberately NO unpublish route on the fake (the real API has none) — this
// mutates fake state directly, to exercise the provider's handling of a server
// that reads back unpublished (a replaced/restored row).
func (f *fakeRegistryServer) unpublishServer(t *testing.T, id string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.servers[id]
	if !ok {
		t.Fatalf("fake has no server %q to unpublish", id)
	}
	s.PublishedAt = nil
}

// serverDeleted reports whether the stored server is soft-deleted.
func (f *fakeRegistryServer) serverDeleted(t *testing.T, id string) bool {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.servers[id]
	if !ok {
		t.Fatalf("fake has no server %q", id)
	}
	return s.deleted
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
		"prepopulated_credentials", "cascaded_fields", "published_at", "attention_tier",
		"publish_blockers",
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
	for _, computed := range []string{"id", "slug", "status", "published_at", "attention_tier", "publish_blockers"} {
		if !s.Attributes[computed].IsComputed() {
			t.Errorf("%s should be Computed", computed)
		}
	}
	// The publish-gate fields are server-derived and change between reads
	// without any configuration change. Computed-ONLY is what keeps that from
	// ever surfacing as a plan diff: making either of them Optional would
	// invite a practitioner to set it and turn every registry recomputation
	// into drift.
	//
	// They must also carry NO plan modifiers. `id` and `slug` pin themselves
	// with UseStateForUnknown because they are immutable for a server's whole
	// life; these two are the opposite — the registry recomputes them per read
	// — and pinning a volatile value to prior state is how a provider ends up
	// reporting a stale tier, or failing an apply with "Provider produced
	// inconsistent result after apply" when the value moves between the plan
	// and the write. A lifecycle test cannot catch this (the refresh that
	// precedes every plan hides it), so it is pinned here.
	for _, readOnly := range []string{"attention_tier", "publish_blockers"} {
		attr := s.Attributes[readOnly]
		if attr.IsOptional() || attr.IsRequired() {
			t.Errorf("%s must be Computed-only, never Optional or Required", readOnly)
		}
		var planModifiers int
		switch a := attr.(type) {
		case frameworkschema.StringAttribute:
			planModifiers = len(a.PlanModifiers)
		case frameworkschema.ListAttribute:
			planModifiers = len(a.PlanModifiers)
		default:
			t.Errorf("%s has unexpected attribute type %T; extend this check", readOnly, attr)
			continue
		}
		if planModifiers != 0 {
			t.Errorf("%s must carry no plan modifiers, got %d", readOnly, planModifiers)
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

// TestMcpServerResponse_PublishGateFieldsUnmarshal pins the WIRE contract of
// the two publish-gate fields: what the registry's JSON decodes to in
// mcpServerResponse, before any mapping runs.
//
// This is where the null-vs-`[]` guarantee on publish_blockers actually lives.
// The registry distinguishes "the gate was not evaluated" (null, or the key
// absent) from "evaluated, nothing blocks a publish" (`[]`); collapsing them
// would turn an unanswered gate into a green light. A *[]string keeps the two
// apart at the decode boundary — nil pointer versus a pointer to an empty
// slice — and this test fails if the field is ever widened back to a bare
// slice, where the distinction would survive only by convention.
func TestMcpServerResponse_PublishGateFieldsUnmarshal(t *testing.T) {
	tierAvailable := "available"
	tierPendingPublish := "pending_publish"

	for _, tc := range []struct {
		name         string
		payload      string
		wantNilPtr   bool
		wantElements []string
		wantTier     *string
	}{
		{
			name:       "blockers null: gate undetermined",
			payload:    `{"id":"srv-1","attention_tier":"available","publish_blockers":null}`,
			wantNilPtr: true,
			wantTier:   &tierAvailable,
		},
		{
			name:       "blockers key absent: gate undetermined",
			payload:    `{"id":"srv-1"}`,
			wantNilPtr: true,
			wantTier:   nil,
		},
		{
			name:         "blockers []: gate evaluated, publish would be accepted",
			payload:      `{"id":"srv-1","attention_tier":"pending_publish","publish_blockers":[]}`,
			wantElements: []string{},
			wantTier:     &tierPendingPublish,
		},
		{
			name:         "blockers populated: reasons in gate order",
			payload:      `{"id":"srv-1","publish_blockers":["not_operationally_available"]}`,
			wantElements: []string{"not_operationally_available"},
			wantTier:     nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got mcpServerResponse
			if err := json.Unmarshal([]byte(tc.payload), &got); err != nil {
				t.Fatalf("json.Unmarshal(%s): %v", tc.payload, err)
			}

			if (got.PublishBlockers == nil) != tc.wantNilPtr {
				t.Fatalf("publish_blockers pointer nil = %t, want %t (null/absent and [] mean "+
					"different things here)", got.PublishBlockers == nil, tc.wantNilPtr)
			}
			if !tc.wantNilPtr && !slices.Equal(*got.PublishBlockers, tc.wantElements) {
				t.Errorf("publish_blockers = %v, want %v", *got.PublishBlockers, tc.wantElements)
			}

			switch {
			case tc.wantTier == nil && got.AttentionTier != nil:
				t.Errorf("attention_tier = %q, want nil for an absent key", *got.AttentionTier)
			case tc.wantTier != nil && got.AttentionTier == nil:
				t.Errorf("attention_tier = nil, want %q", *tc.wantTier)
			case tc.wantTier != nil && *got.AttentionTier != *tc.wantTier:
				t.Errorf("attention_tier = %q, want %q", *got.AttentionTier, *tc.wantTier)
			}
		})
	}
}

// TestApplyMcpServerResponse_PublishGateFieldsRoundTrip pins the MAPPER's
// wire→state handling of the two read-only publish-gate fields, for BOTH the
// resource and the data source, so the two paths cannot drift apart. It is
// hermetic: it builds the response struct in Go and needs no terraform binary.
//
// The load-bearing case is null-vs-`[]` on publish_blockers: the registry
// distinguishes "the gate was not evaluated" (null — the publishing feature
// flag is off, or policy-service did not answer) from "evaluated, nothing
// blocks a publish" (`[]`). Collapsing them would turn an unanswered gate into
// a green light. The decode side of that guarantee is pinned by
// TestMcpServerResponse_PublishGateFieldsUnmarshal, and the end-to-end path by
// TestMcpServerResource_publishGateFieldsTrackTheServer.
func TestApplyMcpServerResponse_PublishGateFieldsRoundTrip(t *testing.T) {
	tierAvailable := "available"
	tierPendingPublish := "pending_publish"
	noBlockers := []string{}
	twoBlockers := []string{"not_operationally_available", "no_active_policy"}

	for _, tc := range []struct {
		name         string
		tier         *string
		blockers     *[]string
		wantTier     types.String
		wantListNull bool
		wantElements []string
	}{
		{
			name:         "undetermined gate: both null",
			tier:         nil,
			blockers:     nil,
			wantTier:     types.StringNull(),
			wantListNull: true,
		},
		{
			name:         "evaluated and clear: empty list, not null",
			tier:         &tierPendingPublish,
			blockers:     &noBlockers,
			wantTier:     types.StringValue("pending_publish"),
			wantElements: []string{},
		},
		{
			name:         "evaluated and blocked: reasons in gate order",
			tier:         &tierAvailable,
			blockers:     &twoBlockers,
			wantTier:     types.StringValue("available"),
			wantElements: []string{"not_operationally_available", "no_active_policy"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := &mcpServerResponse{
				ID:              "srv-1",
				Name:            "Acme",
				Slug:            "acme",
				Status:          "active",
				AttentionTier:   tc.tier,
				PublishBlockers: tc.blockers,
			}

			prior := &mcpServerResourceModel{Scopes: types.ListNull(types.StringType)}
			state, err := applyMcpServerResponse(context.Background(), server, prior)
			if err != nil {
				t.Fatalf("applyMcpServerResponse: %v", err)
			}

			var data mcpServerDataSourceModel
			if err := applyMcpServerDataSource(context.Background(), server, &data); err != nil {
				t.Fatalf("applyMcpServerDataSource: %v", err)
			}

			for _, got := range []struct {
				source   string
				tier     types.String
				blockers types.List
			}{
				{"resource", state.AttentionTier, state.PublishBlockers},
				{"data source", data.AttentionTier, data.PublishBlockers},
			} {
				if !got.tier.Equal(tc.wantTier) {
					t.Errorf("%s attention_tier = %s, want %s", got.source, got.tier, tc.wantTier)
				}
				if got.blockers.IsNull() != tc.wantListNull {
					t.Errorf("%s publish_blockers IsNull() = %t, want %t (null and [] mean different "+
						"things here)", got.source, got.blockers.IsNull(), tc.wantListNull)
					continue
				}
				if tc.wantListNull {
					continue
				}
				var elems []string
				if diags := got.blockers.ElementsAs(context.Background(), &elems, false); diags.HasError() {
					t.Fatalf("%s publish_blockers ElementsAs: %+v", got.source, diags)
				}
				if !slices.Equal(elems, tc.wantElements) {
					t.Errorf("%s publish_blockers = %v, want %v (order is the gate's)",
						got.source, elems, tc.wantElements)
				}
			}
		})
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

// gateStateChecks asserts attention_tier and publish_blockers on both read
// paths — the managed resource and a data source reading the same server — so
// the two mappings cannot drift apart.
//
// These are statecheck/knownvalue assertions rather than TestCheckResourceAttr
// ones on purpose. The legacy flatmap helpers deliberately conflate an absent
// collection with a zero-length one (testCheckNoResourceAttr returns nil for a
// ".#" of "0", and TestCheckResourceAttrPair treats unset and "0" as equal), so
// they are structurally incapable of telling null publish_blockers from [] —
// precisely the distinction under test. knownvalue.Null and
// knownvalue.ListSizeExact(0) read the JSON state and are mutually exclusive.
func gateStateChecks(tier, blockers knownvalue.Check) []statecheck.StateCheck {
	var checks []statecheck.StateCheck
	for _, addr := range []string{"barndoor_mcp_server.test", "data.barndoor_mcp_server.gate"} {
		checks = append(checks,
			statecheck.ExpectKnownValue(addr, tfjsonpath.New("attention_tier"), tier),
			statecheck.ExpectKnownValue(addr, tfjsonpath.New("publish_blockers"), blockers),
		)
	}
	return checks
}

// TestMcpServerResource_publishGateFieldsTrackTheServer drives the two
// read-only publish-gate fields through every wire shape the registry can
// return, against real plan/apply cycles.
//
// Three properties are under test, and each step is built so that breaking one
// of them fails the step:
//
//   - The values follow the server. Each step changes them out-of-band first,
//     so a step passes only if the refresh picked the new ones up — including
//     step 3, where the mapping runs off the PUT response rather than a GET.
//   - null and [] stay distinct in both directions (steps 2 and 4).
//   - They never produce a plan diff. Steps 2 and 4 change them with the
//     configuration untouched, so the framework's post-apply empty-plan check
//     is the assertion.
//
// Plan-modifier absence is pinned in TestMcpServerResource_Schema; a
// refresh-preceded plan cannot see it.
func TestMcpServerResource_publishGateFieldsTrackTheServer(t *testing.T) {
	fake := setupRegistryTest(t)

	withDataSource := func(name string) string {
		return mcpServerConfig(name, "") + `
data "barndoor_mcp_server" "gate" {
  id = barndoor_mcp_server.test.id
}
`
	}

	pendingPublish := "pending_publish"
	connectionError := "connection_error"
	noBlockers := []string{}
	twoBlockers := []string{"not_operationally_available", "no_active_policy"}

	var serverID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllServersDeleted(fake),
		Steps: []resource.TestStep{
			{
				// Created: the fake serves the flag-off shape — a tier, and an
				// unevaluated (null) gate.
				Config: withDataSource("tf-test-gate"),
				ConfigStateChecks: gateStateChecks(
					knownvalue.StringExact(fakeDefaultAttentionTier),
					knownvalue.Null(),
				),
				Check: func(s *terraform.State) error {
					serverID = s.RootModule().Resources["barndoor_mcp_server.test"].Primary.ID
					return nil
				},
			},
			{
				// The gate was evaluated and nothing blocks a publish: [] must
				// read back as an empty list, NOT as null. Configuration is
				// untouched, so the post-apply plan must still be empty.
				PreConfig: func() {
					fake.setServerAttention(t, serverID, &pendingPublish, &noBlockers)
				},
				Config: withDataSource("tf-test-gate"),
				ConfigStateChecks: gateStateChecks(
					knownvalue.StringExact("pending_publish"),
					knownvalue.ListSizeExact(0),
				),
			},
			{
				// Blocked, and changing during a real Update (the rename), so
				// the mapping is exercised off the PUT response too: the reasons
				// arrive in the gate's order.
				PreConfig: func() {
					fake.setServerAttention(t, serverID, &connectionError, &twoBlockers)
				},
				Config: withDataSource("tf-test-gate-renamed"),
				ConfigStateChecks: gateStateChecks(
					knownvalue.StringExact("connection_error"),
					knownvalue.ListExact([]knownvalue.Check{
						knownvalue.StringExact("not_operationally_available"),
						knownvalue.StringExact("no_active_policy"),
					}),
				),
				Check: resource.TestCheckResourceAttr(
					"barndoor_mcp_server.test", "name", "tf-test-gate-renamed"),
			},
			{
				// Back to undetermined: [] -> null must round-trip too, and a
				// null tier must settle to null rather than "".
				PreConfig: func() {
					fake.setServerAttention(t, serverID, nil, nil)
				},
				Config:            withDataSource("tf-test-gate-renamed"),
				ConfigStateChecks: gateStateChecks(knownvalue.Null(), knownvalue.Null()),
			},
		},
	})
}

// TestMcpServerResource_createReadFailureNullsUnknownComputed covers the
// narrow window where the create POST succeeded but the follow-up GET did not.
// The server exists, so the provider still writes it to state — and that means
// every computed attribute it could not learn has to be explicitly nulled
// first, because Terraform rejects an apply result that still carries an
// unknown value.
//
// The regexp is deliberately ANCHORED to the start of the output, and that is
// the whole assertion. Both the correct and the broken provider report "Failed
// to read the MCP server after create", so an unanchored pattern matches
// either way and proves nothing. A provider that leaves one of these
// attributes unknown emits an EXTRA, earlier diagnostic — "Provider returned
// invalid result object after apply ... still indicated an unknown value for
// barndoor_mcp_server.test.<attr>" — which displaces our message from the
// first position and fails the match. Do not relax the anchor.
func TestMcpServerResource_createReadFailureNullsUnknownComputed(t *testing.T) {
	fake := setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllServersDeleted(fake),
		Steps: []resource.TestStep{
			{
				PreConfig: func() { fake.failServerGetOnce() },
				Config:    mcpServerConfig("tf-test-create-read-fail", ""),
				ExpectError: regexp.MustCompile(
					`(?s)\AError running apply[^\n]*\n+Error: Failed to read the MCP server after create`),
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
