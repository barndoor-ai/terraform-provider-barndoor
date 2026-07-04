// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"regexp"
	"testing"

	frameworkresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// --- schema tests --------------------------------------------------------------

func TestLlmModelMappingResource_Metadata(t *testing.T) {
	var resp frameworkresource.MetadataResponse
	NewLlmModelMappingResource().Metadata(context.Background(),
		frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_llm_model_mapping"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestLlmModelMappingResource_Schema(t *testing.T) {
	var resp frameworkresource.SchemaResponse
	NewLlmModelMappingResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{
		"id", "provider_id", "model_alias", "upstream_model", "enabled", "priority",
		"retry_on_429_count", "retry_on_429_max_wait_secs", "bare_alias",
		"stream_idle_timeout_secs", "request_timeout_secs",
	} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
	for _, required := range []string{"provider_id", "model_alias", "upstream_model"} {
		if !resp.Schema.Attributes[required].IsRequired() {
			t.Errorf("%s should be Required", required)
		}
	}
	for _, computed := range []string{
		"id", "enabled", "priority", "retry_on_429_count", "retry_on_429_max_wait_secs",
		"bare_alias", "stream_idle_timeout_secs", "request_timeout_secs",
	} {
		if !resp.Schema.Attributes[computed].IsComputed() {
			t.Errorf("%s should be Computed", computed)
		}
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

func TestLlmModelMappingResource_lifecycle(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	provider := fake.seedProvider()
	const enableName = "barndoor_llm_model_mapping.enable"
	const aliasName = "barndoor_llm_model_mapping.alias"

	minimalConfig := fmt.Sprintf(`
resource "barndoor_llm_model_mapping" "enable" {
  provider_id    = %[1]q
  model_alias    = "gpt-4o"
  upstream_model = "gpt-4o"
}

resource "barndoor_llm_model_mapping" "alias" {
  provider_id    = %[1]q
  model_alias    = "fast"
  upstream_model = "gpt-4o"

  # The 1:1 enablement must exist before its custom aliases (the API's
  # orphan-alias guard).
  depends_on = [barndoor_llm_model_mapping.enable]
}
`, provider.ID)

	expandedConfig := fmt.Sprintf(`
resource "barndoor_llm_model_mapping" "enable" {
  provider_id    = %[1]q
  model_alias    = "gpt-4o"
  upstream_model = "gpt-4o"
  bare_alias     = true
}

resource "barndoor_llm_model_mapping" "alias" {
  provider_id    = %[1]q
  model_alias    = "fast"
  upstream_model = "gpt-4o"

  enabled                    = false
  priority                   = 5
  retry_on_429_count         = 3
  retry_on_429_max_wait_secs = 60
  stream_idle_timeout_secs   = 45
  request_timeout_secs       = 300

  depends_on = [barndoor_llm_model_mapping.enable]
}
`, provider.ID)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmMappingsDeleted(fake),
		Steps: []resource.TestStep{
			{
				// Minimal rows: the server infers bare_alias from the alias
				// shape and materializes the platform timeout defaults.
				Config: minimalConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(enableName, "id"),
					resource.TestCheckResourceAttr(enableName, "enabled", "true"),
					resource.TestCheckResourceAttr(enableName, "priority", "0"),
					resource.TestCheckResourceAttr(enableName, "bare_alias", "false"),
					resource.TestCheckResourceAttr(enableName, "stream_idle_timeout_secs", "30"),
					resource.TestCheckResourceAttr(enableName, "request_timeout_secs", "120"),
					resource.TestCheckResourceAttr(aliasName, "bare_alias", "true"),
					resource.TestCheckResourceAttr(aliasName, "retry_on_429_count", "0"),
					resource.TestCheckResourceAttr(aliasName, "retry_on_429_max_wait_secs", "0"),
				),
			},
			{
				// A second plan over unchanged configuration must be empty.
				Config:   minimalConfig,
				PlanOnly: true,
			},
			{
				// In-place update: opt the enablement into bare resolution;
				// tune the alias row's failover and timeouts and disable it.
				Config: expandedConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(enableName, "bare_alias", "true"),
					resource.TestCheckResourceAttr(aliasName, "enabled", "false"),
					resource.TestCheckResourceAttr(aliasName, "priority", "5"),
					resource.TestCheckResourceAttr(aliasName, "retry_on_429_count", "3"),
					resource.TestCheckResourceAttr(aliasName, "retry_on_429_max_wait_secs", "60"),
					resource.TestCheckResourceAttr(aliasName, "stream_idle_timeout_secs", "45"),
					resource.TestCheckResourceAttr(aliasName, "request_timeout_secs", "300"),
				),
			},
			{
				Config:   expandedConfig,
				PlanOnly: true,
			},
			{
				// Reads walk the org-wide listing (no get-by-id endpoint), so
				// import must reassemble the full attribute set from it.
				ResourceName:      aliasName,
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestLlmModelMappingResource_oneToOneCreateAdoptsExistingRow(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	provider := fake.seedProvider()
	const resourceName = "barndoor_llm_model_mapping.enable"

	// Pre-existing 1:1 enablement, as if created from the app's Models tab.
	fake.mu.Lock()
	existing := &fakeLlmModelMapping{
		ID:                    fake.newID("bbbb"),
		ProviderID:            provider.ID,
		ModelAlias:            "gpt-4o",
		UpstreamModel:         "gpt-4o",
		Enabled:               true,
		BareAlias:             false,
		StreamIdleTimeoutSecs: fakeLlmStreamIdleDefault,
		RequestTimeoutSecs:    fakeLlmRequestDefault,
	}
	fake.mappings = append(fake.mappings, existing)
	fake.mu.Unlock()

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// The create endpoint upserts 1:1 rows: this apply must fold
				// into (adopt) the pre-existing row, not duplicate it.
				Config: fmt.Sprintf(`
resource "barndoor_llm_model_mapping" "enable" {
  provider_id    = %q
  model_alias    = "gpt-4o"
  upstream_model = "gpt-4o"
  bare_alias     = true
}
`, provider.ID),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "id", existing.ID),
					resource.TestCheckResourceAttr(resourceName, "bare_alias", "true"),
					func(*terraform.State) error {
						fake.mu.Lock()
						defer fake.mu.Unlock()
						if len(fake.mappings) != 1 {
							return fmt.Errorf("expected the upsert to fold into the existing row, have %d rows",
								len(fake.mappings))
						}
						return nil
					},
				),
			},
		},
	})
}

func TestLlmModelMappingResource_outOfBandDeletePlansRecreate(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	provider := fake.seedProvider()
	const resourceName = "barndoor_llm_model_mapping.enable"

	config := fmt.Sprintf(`
resource "barndoor_llm_model_mapping" "enable" {
  provider_id    = %q
  model_alias    = "gpt-4o"
  upstream_model = "gpt-4o"
}
`, provider.ID)

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
				PreConfig: func() { fake.markMappingDeleted(t, firstID) },
				Config:    config,
				Check: func(s *terraform.State) error {
					rs, ok := s.RootModule().Resources[resourceName]
					if !ok {
						return fmt.Errorf("%s not in state", resourceName)
					}
					if rs.Primary.ID == firstID {
						return fmt.Errorf("expected a new mapping id after out-of-band delete, still %s", firstID)
					}
					return nil
				},
			},
		},
	})
}

func TestLlmModelMappingResource_orphanAliasIs400(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	provider := fake.seedProvider()

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// A custom alias without the 1:1 enablement for its
				// (provider, upstream) pair is rejected up front (BCP-2920) —
				// the API's message must surface verbatim.
				Config: fmt.Sprintf(`
resource "barndoor_llm_model_mapping" "orphan" {
  provider_id    = %q
  model_alias    = "fast"
  upstream_model = "gpt-4o"
}
`, provider.ID),
				ExpectError: regexp.MustCompile(`the model is not enabled`),
			},
		},
	})
}
