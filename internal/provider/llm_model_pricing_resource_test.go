// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"testing"
	"time"

	frameworkresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// --- schema tests --------------------------------------------------------------

func TestLlmModelPricingResource_Metadata(t *testing.T) {
	var resp frameworkresource.MetadataResponse
	NewLlmModelPricingResource().Metadata(context.Background(),
		frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_llm_model_pricing"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestLlmModelPricingResource_Schema(t *testing.T) {
	var resp frameworkresource.SchemaResponse
	NewLlmModelPricingResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{
		"id", "org_id", "model_pattern", "model_provider", "provider_id", "catalog_slug",
		"input_cost_per_million_tokens", "output_cost_per_million_tokens",
		"cache_read_cost_per_million_tokens", "cache_write_cost_per_million_tokens",
		"sync_mode", "effective_from", "change_reason", "change_source",
	} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
	for _, required := range []string{
		"model_pattern", "input_cost_per_million_tokens", "output_cost_per_million_tokens",
	} {
		if !resp.Schema.Attributes[required].IsRequired() {
			t.Errorf("%s should be Required", required)
		}
	}
	for _, computed := range []string{"id", "org_id", "sync_mode", "effective_from", "change_source"} {
		if !resp.Schema.Attributes[computed].IsComputed() {
			t.Errorf("%s should be Computed", computed)
		}
	}
}

// --- conversion tests ------------------------------------------------------------

// The scheduled-version update body must always carry both cache-cost keys:
// the endpoint's double-Option semantics treat an absent key as "keep" and an
// explicit null as "clear the override", and a removed configuration value
// means "clear".
func TestLlmPricingUpdateRequest_SendsExplicitNullForRemovedCacheCost(t *testing.T) {
	input := 2.5
	read := 1.25
	body := &llmPricingUpdateRequest{
		InputCost: &input,
		CacheRead: &read,
		// CacheWrite deliberately nil: removed from configuration.
	}

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if v, ok := keys["cache_write_cost_per_million_tokens"]; !ok || string(v) != "null" {
		t.Errorf("cache_write_cost_per_million_tokens = %s (present=%t), want an explicit null (clear)", v, ok)
	}
	if v := keys["cache_read_cost_per_million_tokens"]; string(v) != "1.25" {
		t.Errorf("cache_read_cost_per_million_tokens = %s, want 1.25", v)
	}
	if _, ok := keys["output_cost_per_million_tokens"]; ok {
		t.Error("output_cost_per_million_tokens should be omitted when unset (COALESCE keeps it)")
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

func TestLlmModelPricingResource_lifecycle(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_model_pricing.test"

	initialConfig := `
resource "barndoor_llm_model_pricing" "test" {
  model_pattern                  = "gpt-4*"
  input_cost_per_million_tokens  = 2.5
  output_cost_per_million_tokens = 10
}
`
	repricedConfig := `
resource "barndoor_llm_model_pricing" "test" {
  model_pattern                  = "gpt-4*"
  input_cost_per_million_tokens  = 3
  output_cost_per_million_tokens = 12
  change_reason                  = "Q3 price review"
}
`
	reasonOnlyConfig := `
resource "barndoor_llm_model_pricing" "test" {
  model_pattern                  = "gpt-4*"
  input_cost_per_million_tokens  = 3
  output_cost_per_million_tokens = 12
  change_reason                  = "note only"
}
`

	var firstVersionID, secondVersionID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		// Destroy must archive (tombstone), never hard-delete history.
		CheckDestroy: checkAllLlmPricingArchived(fake),
		Steps: []resource.TestStep{
			{
				// First version of the rule: the platform pins admin-authored
				// rules and stamps the write time.
				Config: initialConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckResourceAttr(resourceName, "org_id", fakeLlmOrgID),
					resource.TestCheckResourceAttr(resourceName, "input_cost_per_million_tokens", "2.5"),
					resource.TestCheckResourceAttr(resourceName, "output_cost_per_million_tokens", "10"),
					resource.TestCheckResourceAttr(resourceName, "sync_mode", "pinned"),
					resource.TestCheckResourceAttr(resourceName, "change_source", "admin_create"),
					resource.TestCheckResourceAttrSet(resourceName, "effective_from"),
					resource.TestCheckNoResourceAttr(resourceName, "model_provider"),
					func(s *terraform.State) error {
						firstVersionID = s.RootModule().Resources[resourceName].Primary.ID
						return nil
					},
				),
			},
			{
				// A second plan over unchanged configuration must be empty.
				Config:   initialConfig,
				PlanOnly: true,
			},
			{
				// A price change appends a new version: the rule's history
				// grows and the resource follows the new version row.
				Config: repricedConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "input_cost_per_million_tokens", "3"),
					resource.TestCheckResourceAttr(resourceName, "change_source", "admin_edit"),
					resource.TestCheckResourceAttr(resourceName, "change_reason", "Q3 price review"),
					func(s *terraform.State) error {
						secondVersionID = s.RootModule().Resources[resourceName].Primary.ID
						if secondVersionID == firstVersionID {
							return fmt.Errorf("expected the update to append a new version row, id still %s", firstVersionID)
						}
						group := fake.pricingGroupSnapshot("gpt-4*", "")
						if len(group) != 2 {
							return fmt.Errorf("expected 2 versions on the platform, have %d", len(group))
						}
						if group[1].InputCost != 2.5 {
							return fmt.Errorf("history lost the original version: oldest input cost = %v", group[1].InputCost)
						}
						return nil
					},
				),
			},
			{
				Config:   repricedConfig,
				PlanOnly: true,
			},
			{
				// change_reason is write-only metadata stamped on the next
				// version write: changing it alone must not append a version
				// (the platform's skip-on-no-op absorbs the POST).
				Config: reasonOnlyConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "change_reason", "note only"),
					func(s *terraform.State) error {
						if id := s.RootModule().Resources[resourceName].Primary.ID; id != secondVersionID {
							return fmt.Errorf("a change_reason-only edit rotated the version id: %s -> %s", secondVersionID, id)
						}
						if group := fake.pricingGroupSnapshot("gpt-4*", ""); len(group) != 2 {
							return fmt.Errorf("a change_reason-only edit appended a version: %d versions", len(group))
						}
						return nil
					},
				),
			},
			{
				Config:   reasonOnlyConfig,
				PlanOnly: true,
			},
			{
				// Version ids rotate, so import goes by the rule's logical
				// identity: the model pattern (optionally provider-scoped).
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateId:     "gpt-4*",
				ImportStateVerify: true,
				// change_reason is write-only metadata; an import cannot know it.
				ImportStateVerifyIgnore: []string{"change_reason"},
			},
		},
	})
}

func TestLlmModelPricingResource_catalogSlugBecomesProviderScope(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_model_pricing.test"

	config := `
resource "barndoor_llm_model_pricing" "test" {
  model_pattern                  = "gpt-4o"
  catalog_slug                   = "openai"
  input_cost_per_million_tokens  = 2.5
  output_cost_per_million_tokens = 10

  cache_read_cost_per_million_tokens = 1.25
  sync_mode                          = "tracking"
}
`

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmPricingArchived(fake),
		Steps: []resource.TestStep{
			{
				// The platform persists the catalog slug as the rule's
				// provider scope when model_provider is unset (its import-path
				// semantics); the computed attribute must track that.
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "model_provider", "openai"),
					resource.TestCheckResourceAttr(resourceName, "catalog_slug", "openai"),
					resource.TestCheckResourceAttr(resourceName, "cache_read_cost_per_million_tokens", "1.25"),
					resource.TestCheckNoResourceAttr(resourceName, "cache_write_cost_per_million_tokens"),
					resource.TestCheckResourceAttr(resourceName, "sync_mode", "tracking"),
				),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
		},
	})
}

func TestLlmModelPricingResource_scheduledChangeLifecycle(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_model_pricing.test"

	liveConfig := `
resource "barndoor_llm_model_pricing" "test" {
  model_pattern                  = "claude-*"
  model_provider                 = "anthropic"
  input_cost_per_million_tokens  = 3
  output_cost_per_million_tokens = 15
}
`
	scheduledConfig := `
resource "barndoor_llm_model_pricing" "test" {
  model_pattern                  = "claude-*"
  model_provider                 = "anthropic"
  input_cost_per_million_tokens  = 2.4
  output_cost_per_million_tokens = 12
  effective_from                 = "2030-01-01T00:00:00Z"
}
`
	revisedScheduledConfig := `
resource "barndoor_llm_model_pricing" "test" {
  model_pattern                  = "claude-*"
  model_provider                 = "anthropic"
  input_cost_per_million_tokens  = 2
  output_cost_per_million_tokens = 10
  effective_from                 = "2030-01-01T00:00:00Z"
}
`

	var scheduledVersionID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmPricingArchived(fake),
		Steps: []resource.TestStep{
			{
				Config: liveConfig,
			},
			{
				// A future-dated effective_from appends a scheduled version;
				// the platform keeps billing the live price until it activates
				// and the resource tracks the scheduled version meanwhile.
				Config: scheduledConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "change_source", "admin_schedule"),
					resource.TestCheckResourceAttr(resourceName, "effective_from", "2030-01-01T00:00:00Z"),
					resource.TestCheckResourceAttr(resourceName, "input_cost_per_million_tokens", "2.4"),
					func(s *terraform.State) error {
						scheduledVersionID = s.RootModule().Resources[resourceName].Primary.ID
						group := fake.pricingGroupSnapshot("claude-*", "anthropic")
						if len(group) != 2 {
							return fmt.Errorf("expected 2 versions (live + scheduled), have %d", len(group))
						}
						// Newest first: the scheduled version tops the stack,
						// the live version keeps billing.
						if group[0].ChangeSource != "admin_schedule" || group[0].InputCost != 2.4 {
							return fmt.Errorf("scheduled version not on top of the stack: %+v", group[0])
						}
						if group[1].InputCost != 3 {
							return fmt.Errorf("live version was disturbed: input cost = %v", group[1].InputCost)
						}
						return nil
					},
				),
			},
			{
				// The refresh must keep tracking the pending scheduled version
				// (not fall back to the live one and show drift).
				Config:   scheduledConfig,
				PlanOnly: true,
			},
			{
				// Revising a still-pending scheduled change edits the version
				// in place (the platform's PUT): no third version appears.
				Config: revisedScheduledConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "input_cost_per_million_tokens", "2"),
					resource.TestCheckResourceAttr(resourceName, "change_source", "admin_schedule"),
					func(s *terraform.State) error {
						if id := s.RootModule().Resources[resourceName].Primary.ID; id != scheduledVersionID {
							return fmt.Errorf("in-place scheduled edit rotated the version id: %s -> %s", scheduledVersionID, id)
						}
						if group := fake.pricingGroupSnapshot("claude-*", "anthropic"); len(group) != 2 {
							return fmt.Errorf("scheduled edit appended a version: %d versions", len(group))
						}
						return nil
					},
				),
			},
			{
				Config:   revisedScheduledConfig,
				PlanOnly: true,
			},
			// Destroy archives the rule: the platform cancels the pending
			// scheduled version and appends the tombstone (CheckDestroy).
		},
	})
}

func TestLlmModelPricingResource_outOfBandArchivePlansRecreate(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_model_pricing.test"

	config := `
resource "barndoor_llm_model_pricing" "test" {
  model_pattern                  = "gemini-*"
  input_cost_per_million_tokens  = 1
  output_cost_per_million_tokens = 2
}
`

	var firstID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: func(s *terraform.State) error {
					firstID = s.RootModule().Resources[resourceName].Primary.ID
					return nil
				},
			},
			{
				// An admin archives the rule in the app: the refresh must drop
				// the resource and the apply must resurrect the rule with a
				// fresh live version on top of the tombstone.
				PreConfig: func() { fake.markPricingArchived(t, "gemini-*", "") },
				Config:    config,
				Check: resource.ComposeAggregateTestCheckFunc(
					// The resurrecting write lands on a rule that already has
					// history, so the platform records it as an edit.
					resource.TestCheckResourceAttr(resourceName, "change_source", "admin_edit"),
					func(s *terraform.State) error {
						rs, ok := s.RootModule().Resources[resourceName]
						if !ok {
							return fmt.Errorf("%s not in state", resourceName)
						}
						if rs.Primary.ID == firstID {
							return fmt.Errorf("expected a new version id after out-of-band archive, still %s", firstID)
						}
						group := fake.pricingGroupSnapshot("gemini-*", "")
						if len(group) < 3 {
							return fmt.Errorf("expected original + tombstone + resurrection, have %d versions", len(group))
						}
						if group[0].IsArchived {
							return fmt.Errorf("rule is still archived after the recreate")
						}
						return nil
					},
				),
			},
		},
	})
}

func TestLlmModelPricingResource_createRefusesLiveRuleTakeover(t *testing.T) {
	fake := setupLlmGatewayTest(t)

	// A rule authored in the app (out-of-band) for the same pattern.
	fake.mu.Lock()
	fake.pricing = append(fake.pricing, &fakeLlmPricingVersion{
		ID:               fake.newID("ffff"),
		ModelPattern:     "gpt-4*",
		InputCost:        9,
		OutputCost:       27,
		EffectiveFrom:    mustParseTime(t, "2026-01-01T00:00:00Z"),
		EffectiveFromRaw: "2026-01-01T00:00:00Z",
		SyncMode:         "pinned",
		ChangeSource:     "admin_create",
	})
	fake.mu.Unlock()

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "barndoor_llm_model_pricing" "test" {
  model_pattern                  = "gpt-4*"
  input_cost_per_million_tokens  = 2.5
  output_cost_per_million_tokens = 10
}
`,
				ExpectError: regexp.MustCompile(`LLM model pricing rule already exists`),
			},
		},
	})
}

func TestLlmModelPricingResource_staleEffectiveFromOnUpdateErrors(t *testing.T) {
	setupLlmGatewayTest(t)

	pinnedPast := `
resource "barndoor_llm_model_pricing" "test" {
  model_pattern                  = "mistral-*"
  effective_from                 = "2020-01-01T00:00:00Z"
  input_cost_per_million_tokens  = 1
  output_cost_per_million_tokens = 2
}
`

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: pinnedPast,
			},
			{
				// The version store is keyed by (rule, effective_from) and
				// past versions are immutable: a price change cannot reuse the
				// already-active timestamp. The provider must fail with
				// guidance instead of tripping the store's unique key.
				Config: `
resource "barndoor_llm_model_pricing" "test" {
  model_pattern                  = "mistral-*"
  effective_from                 = "2020-01-01T00:00:00Z"
  input_cost_per_million_tokens  = 5
  output_cost_per_million_tokens = 10
}
`,
				ExpectError: regexp.MustCompile(`Stale effective_from on a pricing update`),
			},
		},
	})
}

func TestLlmModelPricingResource_permissionDeniedIs403(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	fake.setForbidden(true)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "barndoor_llm_model_pricing" "test" {
  model_pattern                  = "gpt-4*"
  input_cost_per_million_tokens  = 2.5
  output_cost_per_million_tokens = 10
}
`,
				ExpectError: regexp.MustCompile(`Permission denied by the LLM Gateway API`),
			},
		},
	})
}

// mustParseTime parses an RFC 3339 timestamp for fixture rows.
func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := llmPricingParseTime(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return parsed
}
