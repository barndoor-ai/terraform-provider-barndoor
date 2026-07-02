// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	frameworkresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// --- fake /agents endpoints -------------------------------------------------
//
// Extends fakeRegistryServer (mcp_server_resource_test.go) with the
// application-registration surface: register 422s (unknown directory,
// duplicate live registration), soft-delete-means-404 reads, per-flag PATCH
// endpoints, and the bare-row register response production returns.

// fakeAgentDirectories is the catalog the fake resolves registrations
// against: name and dcr (dcr=true renders as agent_type "external").
var fakeAgentDirectories = map[string]struct {
	Name string
	DCR  bool
}{
	"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa": {Name: "Internal Test Agent", DCR: false},
	"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb": {Name: "External Test Agent", DCR: true},
}

type fakeAgent struct {
	ID                         string `json:"id"`
	ApplicationDirectoryID     string `json:"application_directory_id"`
	WriteConfirmationsRequired bool   `json:"write_confirmations_required"`
	LlmGatewayEnabled          bool   `json:"llm_gateway_enabled"`

	deleted   bool
	protected bool
}

// respond renders the ApplicationResponse shape (directory join + computed
// agent_type), as the GET/PATCH endpoints return it.
func (a *fakeAgent) respond(w http.ResponseWriter) {
	dir := fakeAgentDirectories[a.ApplicationDirectoryID]
	agentType := "internal"
	if dir.DCR {
		agentType = "external"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":                           a.ID,
		"application_directory_id":     a.ApplicationDirectoryID,
		"write_confirmations_required": a.WriteConfirmationsRequired,
		"llm_gateway_enabled":          a.LlmGatewayEnabled,
		"agent_type":                   agentType,
		"deleted_at":                   nil,
		"application_directory":        map[string]any{"name": dir.Name},
	})
}

func (f *fakeRegistryServer) handleAgents(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/registry/v1/agents"), "/")

	if rest == "" && r.Method == http.MethodPost {
		f.registerAgent(w, r)
		return
	}

	id, action, _ := strings.Cut(rest, "/")
	a, ok := f.agents[id]

	switch {
	case action == "" && r.Method == http.MethodGet:
		if !ok || a.deleted {
			writeJSONError(w, http.StatusNotFound, "Application not found")
			return
		}
		a.respond(w)
	case action == "" && r.Method == http.MethodDelete:
		if !ok || a.deleted {
			// Production 404s a missing row via get_to_delete; a re-delete of a
			// soft-deleted row is a silent 204 (CAS loser).
			if !ok {
				writeJSONError(w, http.StatusNotFound, "Application not found")
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if a.protected {
			writeJSONError(w, http.StatusForbidden, "Cannot delete protected application")
			return
		}
		a.deleted = true
		w.WriteHeader(http.StatusNoContent)
	case action == "write-confirmations" && r.Method == http.MethodPatch:
		if !ok || a.deleted {
			writeJSONError(w, http.StatusNotFound, "Application not found")
			return
		}
		var body struct {
			WriteConfirmationsRequired bool `json:"write_confirmations_required"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		a.WriteConfirmationsRequired = body.WriteConfirmationsRequired
		a.respond(w)
	case action == "llm-gateway" && r.Method == http.MethodPatch:
		if !ok || a.deleted {
			writeJSONError(w, http.StatusNotFound, "Application not found")
			return
		}
		var body struct {
			LlmGatewayEnabled bool `json:"llm_gateway_enabled"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		a.LlmGatewayEnabled = body.LlmGatewayEnabled
		a.respond(w)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeRegistryServer) registerAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ApplicationDirectoryID     string `json:"application_directory_id"`
		WriteConfirmationsRequired *bool  `json:"write_confirmations_required"`
		LlmGatewayEnabled          *bool  `json:"llm_gateway_enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if _, ok := fakeAgentDirectories[body.ApplicationDirectoryID]; !ok {
		writeJSONError(w, http.StatusUnprocessableEntity,
			"Invalid application_directory_id: Application directory not found")
		return
	}
	// One live registration per (org, directory) — production's partial unique
	// index surfaces as a generic constraint-violation 422.
	for _, existing := range f.agents {
		if !existing.deleted && existing.ApplicationDirectoryID == body.ApplicationDirectoryID {
			writeJSONError(w, http.StatusUnprocessableEntity,
				"Application registration failed due to data constraint violation")
			return
		}
	}

	f.nextAgentID++
	a := &fakeAgent{
		ID:                         fmt.Sprintf("aaaa0000-0000-0000-0000-%012d", f.nextAgentID),
		ApplicationDirectoryID:     body.ApplicationDirectoryID,
		WriteConfirmationsRequired: true,
		LlmGatewayEnabled:          false,
	}
	if body.WriteConfirmationsRequired != nil {
		a.WriteConfirmationsRequired = *body.WriteConfirmationsRequired
	}
	if body.LlmGatewayEnabled != nil {
		a.LlmGatewayEnabled = *body.LlmGatewayEnabled
	}
	f.agents[a.ID] = a

	// Production's register endpoint returns the bare Application row: no
	// directory join, no agent_type. Returning it here proves the provider
	// does the follow-up GET rather than trusting this shape.
	_ = json.NewEncoder(w).Encode(a)
}

// markAgentDeleted soft-deletes a stored agent out-of-band.
func (f *fakeRegistryServer) markAgentDeleted(t *testing.T, id string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.agents[id]
	if !ok {
		t.Fatalf("fake has no agent %q to delete", id)
	}
	a.deleted = true
}

// checkAllAgentsDeleted is the CheckDestroy for agent tests: destroy must
// soft-delete every registration the test created.
func checkAllAgentsDeleted(fake *fakeRegistryServer) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for id, a := range fake.agents {
			if !a.deleted {
				return fmt.Errorf("agent %s was not unregistered on destroy", id)
			}
		}
		return nil
	}
}

// --- schema tests --------------------------------------------------------------

func TestAgentResource_Metadata(t *testing.T) {
	var resp frameworkresource.MetadataResponse
	NewAgentResource().Metadata(context.Background(),
		frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_agent"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestAgentResource_Schema(t *testing.T) {
	var resp frameworkresource.SchemaResponse
	NewAgentResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}
	s := resp.Schema

	for _, attr := range []string{
		"id", "application_directory_id", "write_confirmations_required", "llm_gateway_enabled",
		"name", "agent_type",
	} {
		if _, ok := s.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
	if !s.Attributes["application_directory_id"].IsRequired() {
		t.Error("application_directory_id should be Required")
	}
	for _, computed := range []string{"id", "name", "agent_type"} {
		if !s.Attributes[computed].IsComputed() {
			t.Errorf("%s should be Computed", computed)
		}
	}
	for _, optComputed := range []string{"write_confirmations_required", "llm_gateway_enabled"} {
		a := s.Attributes[optComputed]
		if !a.IsOptional() || !a.IsComputed() {
			t.Errorf("%s should be Optional+Computed", optComputed)
		}
	}
}

// --- error-mapping tests ---------------------------------------------------------

func TestAddAgentAPIError(t *testing.T) {
	t.Run("duplicate registration names the uniqueness rule", func(t *testing.T) {
		var diags diag.Diagnostics
		addAgentAPIError(&diags, "register the AI Agent", &apiError{
			method: http.MethodPost, path: "api/registry/v1/agents",
			status: http.StatusUnprocessableEntity,
			body:   `{"detail":"Application registration failed due to data constraint violation"}`,
		})
		if got := diags.Errors()[0].Detail(); !strings.Contains(got, "one live registration per agent directory") {
			t.Errorf("detail %q should explain the per-directory uniqueness rule", got)
		}
	})
	t.Run("protected agent 403 is actionable", func(t *testing.T) {
		var diags diag.Diagnostics
		addAgentAPIError(&diags, "unregister (destroy) the AI Agent", &apiError{
			method: http.MethodDelete, path: "api/registry/v1/agents/x",
			status: http.StatusForbidden,
			body:   `{"detail":"Cannot delete protected application: ToolIQ"}`,
		})
		if got := diags.Errors()[0].Detail(); !strings.Contains(got, "protected platform agent") {
			t.Errorf("detail %q should mention protected agents", got)
		}
	})
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

func agentConfig(directoryID, extra string) string {
	return fmt.Sprintf(`
resource "barndoor_agent" "test" {
  application_directory_id = %q
%s
}
`, directoryID, extra)
}

const (
	internalDirID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	externalDirID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
)

func TestAgentResource_basicLifecycle(t *testing.T) {
	fake := setupRegistryTest(t)
	resourceName := "barndoor_agent.test"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllAgentsDeleted(fake),
		Steps: []resource.TestStep{
			{
				// Create with the toggles unset: server defaults apply and the
				// computed name/agent_type come from the directory join.
				Config: agentConfig(internalDirID, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckResourceAttr(resourceName, "application_directory_id", internalDirID),
					resource.TestCheckResourceAttr(resourceName, "write_confirmations_required", "true"),
					resource.TestCheckResourceAttr(resourceName, "llm_gateway_enabled", "false"),
					resource.TestCheckResourceAttr(resourceName, "name", "Internal Test Agent"),
					resource.TestCheckResourceAttr(resourceName, "agent_type", "internal"),
				),
			},
			{
				// Toggle both flags in place (each rides its own PATCH endpoint).
				Config: agentConfig(internalDirID, `
  write_confirmations_required = false
  llm_gateway_enabled          = true
`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "write_confirmations_required", "false"),
					resource.TestCheckResourceAttr(resourceName, "llm_gateway_enabled", "true"),
				),
			},
			{
				// Import by registration id: everything is server-authoritative,
				// so the verify needs no ignores.
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestAgentResource_externalDirectoryType(t *testing.T) {
	fake := setupRegistryTest(t)
	resourceName := "barndoor_agent.test"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllAgentsDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: agentConfig(externalDirID, "\n  llm_gateway_enabled = true\n"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "agent_type", "external"),
					resource.TestCheckResourceAttr(resourceName, "name", "External Test Agent"),
					// Toggles set at create ride the register payload itself.
					resource.TestCheckResourceAttr(resourceName, "llm_gateway_enabled", "true"),
				),
			},
		},
	})
}

func TestAgentResource_duplicateRegistration(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: agentConfig(internalDirID, "") + fmt.Sprintf(`
resource "barndoor_agent" "dupe" {
  application_directory_id = %q
  depends_on               = [barndoor_agent.test]
}
`, internalDirID),
				ExpectError: regexp.MustCompile(`one live registration per agent directory`),
			},
		},
	})
}

func TestAgentResource_unknownDirectoryIs422(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      agentConfig("cccccccc-cccc-cccc-cccc-cccccccccccc", ""),
				ExpectError: regexp.MustCompile(`Application directory not found`),
			},
		},
	})
}

func TestAgentResource_outOfBandDeletePlansRecreate(t *testing.T) {
	fake := setupRegistryTest(t)
	resourceName := "barndoor_agent.test"

	var firstID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: agentConfig(internalDirID, ""),
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
				// Simulate an out-of-band unregister; the refresh must drop the
				// resource and the apply must register a replacement.
				PreConfig: func() { fake.markAgentDeleted(t, firstID) },
				Config:    agentConfig(internalDirID, ""),
				Check: func(s *terraform.State) error {
					rs, ok := s.RootModule().Resources[resourceName]
					if !ok {
						return fmt.Errorf("%s not in state", resourceName)
					}
					if rs.Primary.ID == firstID {
						return fmt.Errorf("expected a new registration id after out-of-band delete, still %s", firstID)
					}
					return nil
				},
			},
		},
	})
}
