// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework-jsontypes/jsontypes"
	frameworkresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// --- schema tests --------------------------------------------------------------

func TestLlmProviderResource_Metadata(t *testing.T) {
	var resp frameworkresource.MetadataResponse
	NewLlmProviderResource().Metadata(context.Background(),
		frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_llm_provider"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestLlmProviderResource_Schema(t *testing.T) {
	var resp frameworkresource.SchemaResponse
	NewLlmProviderResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{
		"id", "org_id", "name", "model_provider", "base_url", "auth_type", "api_key",
		"settings", "enabled", "enforce_health_check", "health_status", "health_detail",
		"health_checked_at", "created_at", "updated_at",
	} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
	for _, required := range []string{"name", "model_provider", "base_url"} {
		if !resp.Schema.Attributes[required].IsRequired() {
			t.Errorf("%s should be Required", required)
		}
	}
	for _, computed := range []string{
		"id", "org_id", "auth_type", "settings", "enabled", "enforce_health_check",
		"health_status", "health_detail", "health_checked_at", "created_at", "updated_at",
	} {
		if !resp.Schema.Attributes[computed].IsComputed() {
			t.Errorf("%s should be Computed", computed)
		}
	}
	if !resp.Schema.Attributes["api_key"].IsSensitive() {
		t.Error("api_key should be Sensitive")
	}
	if resp.Schema.Attributes["api_key"].IsComputed() {
		t.Error("api_key is write-only and must not be Computed — the API never returns it")
	}
}

// --- conversion tests ------------------------------------------------------------

func TestBuildLlmProviderUpdateRequest_CarriesCredentialAndFullState(t *testing.T) {
	plan := &llmProviderResourceModel{
		Name:               types.StringValue("OpenAI"),
		BaseURL:            types.StringValue("https://api.openai.com/v1"),
		AuthType:           types.StringValue("bearer_api_key"),
		APIKey:             types.StringValue("sk-rotated"),
		Settings:           jsontypes.NewNormalizedNull(),
		Enabled:            types.BoolValue(false),
		EnforceHealthCheck: types.BoolValue(true),
	}
	body, err := buildLlmProviderUpdateRequest(plan)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if v := keys["api_key"]; string(v) != `"sk-rotated"` {
		t.Errorf("api_key = %s, want the configured credential (rotation is a plain update)", v)
	}
	if v := keys["enabled"]; string(v) != "false" {
		t.Errorf("enabled = %s, want false", v)
	}
	if _, ok := keys["settings"]; ok {
		t.Error("settings must be omitted when unset (an omitted key leaves the column unchanged)")
	}
}

func TestApplyLlmProviderResponse_KeepsConfiguredCredentialAndSettlesSettings(t *testing.T) {
	prior := &llmProviderResourceModel{
		APIKey:   types.StringValue("sk-configured"),
		Settings: jsontypes.NewNormalizedNull(),
	}
	response := &llmProviderResponse{
		ID:                 "aaaa0000-0000-0000-0000-000000000001",
		OrgID:              fakeLlmOrgID,
		Name:               "OpenAI",
		ModelProvider:      "openai",
		AuthType:           "bearer_api_key",
		BaseURL:            "https://api.openai.com/v1",
		Enabled:            true,
		Settings:           json.RawMessage("{}"),
		EnforceHealthCheck: true,
		HealthStatus:       "unverified",
		CreatedAt:          fakeLlmTime,
		UpdatedAt:          fakeLlmTime,
	}

	state := applyLlmProviderResponse(response, prior)

	if got, want := state.APIKey.ValueString(), "sk-configured"; got != want {
		t.Errorf("api_key = %q, want the configured value carried through (the API never echoes it)", got)
	}
	if !state.Settings.IsNull() {
		t.Error("an empty settings object with a null prior must settle to null, not \"{}\"")
	}
	if !state.HealthDetail.IsNull() {
		t.Error("health_detail absent from the response must map to null")
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

func TestLlmProviderResource_lifecycle(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_provider.test"

	minimalConfig := `
resource "barndoor_llm_provider" "test" {
  name           = "OpenAI"
  model_provider = "openai"
  base_url       = "https://api.openai.com/v1"
  api_key        = "sk-test-1"
}
`
	expandedConfig := `
resource "barndoor_llm_provider" "test" {
  name           = "OpenAI (EU)"
  model_provider = "openai"
  base_url       = "https://eu.api.openai.com/v1"
  api_key        = "sk-test-2"
  settings       = jsonencode({ organization = "acme" })

  enabled              = false
  enforce_health_check = false
}
`

	var providerID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmProvidersDeleted(fake),
		Steps: []resource.TestStep{
			{
				// Minimal provider: auth_type, enabled, enforce_health_check,
				// and the health fields all come from the server.
				Config: minimalConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckResourceAttr(resourceName, "org_id", fakeLlmOrgID),
					resource.TestCheckResourceAttr(resourceName, "auth_type", "bearer_api_key"),
					resource.TestCheckResourceAttr(resourceName, "enabled", "true"),
					resource.TestCheckResourceAttr(resourceName, "enforce_health_check", "true"),
					resource.TestCheckResourceAttr(resourceName, "health_status", "unverified"),
					resource.TestCheckNoResourceAttr(resourceName, "settings"),
					// The credential reached the platform but only ever lives
					// in state as the configured value.
					resource.TestCheckResourceAttr(resourceName, "api_key", "sk-test-1"),
					func(s *terraform.State) error {
						rs, ok := s.RootModule().Resources[resourceName]
						if !ok {
							return fmt.Errorf("%s not in state", resourceName)
						}
						providerID = rs.Primary.ID
						if got := fake.providerCredential(t, providerID); got != "sk-test-1" {
							return fmt.Errorf("stored credential = %q, want sk-test-1", got)
						}
						return nil
					},
				),
			},
			{
				// A second plan over unchanged configuration must be empty.
				Config:   minimalConfig,
				PlanOnly: true,
			},
			{
				// In-place update: rename, move base_url, rotate the key, add
				// settings, disable the provider and its health gate.
				Config: expandedConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "name", "OpenAI (EU)"),
					resource.TestCheckResourceAttr(resourceName, "base_url", "https://eu.api.openai.com/v1"),
					resource.TestCheckResourceAttr(resourceName, "enabled", "false"),
					resource.TestCheckResourceAttr(resourceName, "enforce_health_check", "false"),
					resource.TestCheckResourceAttr(resourceName, "settings", `{"organization":"acme"}`),
					func(*terraform.State) error {
						if got := fake.providerCredential(t, providerID); got != "sk-test-2" {
							return fmt.Errorf("stored credential = %q, want the rotated sk-test-2", got)
						}
						return nil
					},
				),
			},
			{
				Config:   expandedConfig,
				PlanOnly: true,
			},
			{
				// api_key is write-only: the API never returns it, so import
				// cannot fill it.
				ResourceName:            resourceName,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"api_key"},
			},
		},
	})
}

func TestLlmProviderResource_disabledOnCreateConverges(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_provider.test"

	// The create endpoint has no enabled field; the resource must converge
	// enabled = false with a follow-up update in the same apply.
	config := `
resource "barndoor_llm_provider" "test" {
  name           = "Staged"
  model_provider = "anthropic"
  base_url       = "https://api.anthropic.com"
  api_key        = "sk-ant-test"
  enabled        = false
}
`

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmProvidersDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "enabled", "false"),
					resource.TestCheckResourceAttr(resourceName, "auth_type", "x_api_key"),
					func(*terraform.State) error {
						fake.mu.Lock()
						defer fake.mu.Unlock()
						if len(fake.providers) != 1 {
							return fmt.Errorf("expected 1 provider, have %d", len(fake.providers))
						}
						if fake.providers[0].Enabled {
							return fmt.Errorf("provider is still enabled on the platform")
						}
						return nil
					},
				),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
		},
	})
}

func TestLlmProviderResource_outOfBandDeletePlansRecreate(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_provider.test"

	config := `
resource "barndoor_llm_provider" "test" {
  name           = "tf-llm-oob"
  model_provider = "openai"
  base_url       = "https://api.openai.com/v1"
  api_key        = "sk-oob"
}
`

	var firstID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
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
				PreConfig: func() { fake.markProviderDeleted(t, firstID) },
				Config:    config,
				Check: func(s *terraform.State) error {
					rs, ok := s.RootModule().Resources[resourceName]
					if !ok {
						return fmt.Errorf("%s not in state", resourceName)
					}
					if rs.Primary.ID == firstID {
						return fmt.Errorf("expected a new provider id after out-of-band delete, still %s", firstID)
					}
					return nil
				},
			},
		},
	})
}

func TestLlmProviderResource_nonObjectSettingsIs400(t *testing.T) {
	setupLlmGatewayTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// The API requires settings to be a JSON object; the 400
				// message must surface verbatim.
				Config: `
resource "barndoor_llm_provider" "test" {
  name           = "tf-llm-bad-settings"
  model_provider = "openai"
  base_url       = "https://api.openai.com/v1"
  api_key        = "sk-bad"
  settings       = jsonencode(["not", "an", "object"])
}
`,
				ExpectError: regexp.MustCompile(`settings must be a JSON object`),
			},
		},
	})
}
