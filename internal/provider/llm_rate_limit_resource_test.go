// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"testing"

	frameworkresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// --- schema tests --------------------------------------------------------------

func TestLlmRateLimitResource_Metadata(t *testing.T) {
	var resp frameworkresource.MetadataResponse
	NewLlmRateLimitResource().Metadata(context.Background(),
		frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_llm_rate_limit"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestLlmRateLimitResource_Schema(t *testing.T) {
	var resp frameworkresource.SchemaResponse
	NewLlmRateLimitResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{
		"id", "org_id", "name", "scope_type", "scope_id", "scope_value",
		"requests_per_minute", "tokens_per_minute", "traffic_type", "enabled",
	} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
	for _, required := range []string{"name", "scope_type"} {
		if !resp.Schema.Attributes[required].IsRequired() {
			t.Errorf("%s should be Required", required)
		}
	}
	for _, computed := range []string{"id", "org_id", "traffic_type", "enabled"} {
		if !resp.Schema.Attributes[computed].IsComputed() {
			t.Errorf("%s should be Computed", computed)
		}
	}
}

// --- conversion tests ------------------------------------------------------------

// The update body must always carry both metric keys: the API's tri-state
// PATCH semantics treat an absent key as "keep" and an explicit null as
// "clear", and a removed configuration value means "clear".
func TestLlmRateLimitUpdateRequest_SendsExplicitNullForRemovedMetric(t *testing.T) {
	name := "p"
	rpm := int32PtrFromInt64(types.Int64Value(100))
	body := &llmRateLimitUpdateRequest{
		Name:              &name,
		RequestsPerMinute: rpm,
		TokensPerMinute:   int32PtrFromInt64(types.Int64Null()),
	}

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if v, ok := keys["tokens_per_minute"]; !ok || string(v) != "null" {
		t.Errorf("tokens_per_minute = %s (present=%t), want an explicit null (clear the metric)", v, ok)
	}
	if v := keys["requests_per_minute"]; string(v) != "100" {
		t.Errorf("requests_per_minute = %s, want 100", v)
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

func TestLlmRateLimitResource_lifecycle(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_rate_limit.test"

	minimalConfig := `
resource "barndoor_llm_rate_limit" "test" {
  name                = "Org ceiling"
  scope_type          = "org"
  requests_per_minute = 600
}
`
	expandedConfig := `
resource "barndoor_llm_rate_limit" "test" {
  name              = "Engineering ceiling"
  scope_type        = "group"
  scope_value       = "engineering"
  tokens_per_minute = 250000

  traffic_type = "llm"
  enabled      = false
}
`

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmRateLimitsDeleted(fake),
		Steps: []resource.TestStep{
			{
				// Minimal policy: traffic_type defaults to all, enabled to
				// true; the unset metric stays null.
				Config: minimalConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckResourceAttr(resourceName, "org_id", fakeLlmOrgID),
					resource.TestCheckResourceAttr(resourceName, "requests_per_minute", "600"),
					resource.TestCheckNoResourceAttr(resourceName, "tokens_per_minute"),
					resource.TestCheckResourceAttr(resourceName, "traffic_type", "all"),
					resource.TestCheckResourceAttr(resourceName, "enabled", "true"),
				),
			},
			{
				// A second plan over unchanged configuration must be empty.
				Config:   minimalConfig,
				PlanOnly: true,
			},
			{
				// In-place update: rescope to an IdP group, swap the enforced
				// metric (rpm must be cleared via explicit null), narrow the
				// lane, disable.
				Config: expandedConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "name", "Engineering ceiling"),
					resource.TestCheckResourceAttr(resourceName, "scope_type", "group"),
					resource.TestCheckResourceAttr(resourceName, "scope_value", "engineering"),
					resource.TestCheckNoResourceAttr(resourceName, "requests_per_minute"),
					resource.TestCheckResourceAttr(resourceName, "tokens_per_minute", "250000"),
					resource.TestCheckResourceAttr(resourceName, "traffic_type", "llm"),
					resource.TestCheckResourceAttr(resourceName, "enabled", "false"),
					func(*terraform.State) error {
						fake.mu.Lock()
						defer fake.mu.Unlock()
						if len(fake.rateLimits) != 1 {
							return fmt.Errorf("expected 1 policy, have %d", len(fake.rateLimits))
						}
						if fake.rateLimits[0].RequestsPerMinute != nil {
							return fmt.Errorf("requests_per_minute was not cleared on the platform")
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
				// Reads walk the org-wide listing (no get-by-id endpoint), so
				// import must reassemble the full attribute set from it.
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestLlmRateLimitResource_outOfBandDeletePlansRecreate(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_rate_limit.test"

	config := `
resource "barndoor_llm_rate_limit" "test" {
  name                = "tf-llm-rl-oob"
  scope_type          = "org"
  requests_per_minute = 60
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
				PreConfig: func() { fake.markRateLimitDeleted(t, firstID) },
				Config:    config,
				Check: func(s *terraform.State) error {
					rs, ok := s.RootModule().Resources[resourceName]
					if !ok {
						return fmt.Errorf("%s not in state", resourceName)
					}
					if rs.Primary.ID == firstID {
						return fmt.Errorf("expected a new policy id after out-of-band delete, still %s", firstID)
					}
					return nil
				},
			},
		},
	})
}

func TestLlmRateLimitResource_duplicateScopeIs409(t *testing.T) {
	setupLlmGatewayTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Two policies on the same (scope, traffic_type) would share
				// a rate-limit counter; the platform's 409 must surface.
				Config: `
resource "barndoor_llm_rate_limit" "first" {
  name                = "tf-llm-rl-a"
  scope_type          = "org"
  requests_per_minute = 60
}

resource "barndoor_llm_rate_limit" "second" {
  name              = "tf-llm-rl-b"
  scope_type        = "org"
  tokens_per_minute = 1000

  depends_on = [barndoor_llm_rate_limit.first]
}
`,
				ExpectError: regexp.MustCompile(`A policy with this scope and traffic type already exists`),
			},
		},
	})
}
