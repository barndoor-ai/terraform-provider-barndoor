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

func TestLlmGovernanceConfigResource_Metadata(t *testing.T) {
	var resp frameworkresource.MetadataResponse
	NewLlmGovernanceConfigResource().Metadata(context.Background(),
		frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_llm_governance_config"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestLlmGovernanceConfigResource_Schema(t *testing.T) {
	var resp frameworkresource.SchemaResponse
	NewLlmGovernanceConfigResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{"id", "require_pricing_for_mappings"} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
	if !resp.Schema.Attributes["require_pricing_for_mappings"].IsRequired() {
		t.Error("require_pricing_for_mappings should be Required")
	}
	if !resp.Schema.Attributes["id"].IsComputed() {
		t.Error("id should be Computed")
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

func TestLlmGovernanceConfigResource_lifecycle(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_governance_config.test"

	offConfig := `
resource "barndoor_llm_governance_config" "test" {
  require_pricing_for_mappings = false
}
`
	onConfig := `
resource "barndoor_llm_governance_config" "test" {
  require_pricing_for_mappings = true
}
`

	governanceIs := func(want bool) resource.TestCheckFunc {
		return func(*terraform.State) error {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if fake.governance == nil {
				return fmt.Errorf("governance config row was never written")
			}
			if *fake.governance != want {
				return fmt.Errorf("platform require_pricing_for_mappings = %t, want %t", *fake.governance, want)
			}
			return nil
		}
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		// Destroy from the final (true) step must reset the singleton to the
		// platform default (false), not delete anything.
		CheckDestroy: checkLlmGovernanceReset(fake),
		Steps: []resource.TestStep{
			{
				// Adopting with the default value still materializes the row.
				Config: offConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "id", fakeLlmOrgID),
					resource.TestCheckResourceAttr(resourceName, "require_pricing_for_mappings", "false"),
					governanceIs(false),
				),
			},
			{
				// A second plan over unchanged configuration must be empty.
				Config:   offConfig,
				PlanOnly: true,
			},
			{
				// In-place update through the same PUT upsert.
				Config: onConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "require_pricing_for_mappings", "true"),
					governanceIs(true),
				),
			},
			{
				Config:   onConfig,
				PlanOnly: true,
			},
			{
				// The singleton imports by organization ID.
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateId:     fakeLlmOrgID,
				ImportStateVerify: true,
			},
		},
	})
}

func TestLlmGovernanceConfigResource_outOfBandChangeIsReconciled(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_governance_config.test"

	config := `
resource "barndoor_llm_governance_config" "test" {
  require_pricing_for_mappings = true
}
`

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
			},
			{
				// An admin flips the flag in the app; the refresh must surface
				// the drift and the apply must write the configuration back.
				PreConfig: func() { fake.setGovernanceValue(false) },
				Config:    config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "require_pricing_for_mappings", "true"),
					func(*terraform.State) error {
						fake.mu.Lock()
						defer fake.mu.Unlock()
						if fake.governance == nil || !*fake.governance {
							return fmt.Errorf("out-of-band change was not reconciled back to true")
						}
						return nil
					},
				),
			},
		},
	})
}

func TestLlmGovernanceConfigResource_permissionDeniedIs403(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	fake.setForbidden(true)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// A credential without the admin role fails the Cerbos check on
				// every handler; the 403 must surface as the actionable
				// permission diagnostic, not a generic HTTP error.
				Config: `
resource "barndoor_llm_governance_config" "test" {
  require_pricing_for_mappings = true
}
`,
				ExpectError: regexp.MustCompile(`Permission denied by the LLM Gateway API`),
			},
		},
	})
}
