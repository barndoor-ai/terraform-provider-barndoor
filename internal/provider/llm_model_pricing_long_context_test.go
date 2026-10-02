// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

func llmLongContextPricingHCL(input float64, tier, effectiveFrom string) string {
	eff := ""
	if effectiveFrom != "" {
		eff = fmt.Sprintf("  effective_from = %q\n", effectiveFrom)
	}
	return fmt.Sprintf(`
resource "barndoor_llm_model_pricing" "test" {
  model_pattern                  = "gemini-2.5-pro"
  model_provider                 = "google_ai"
  input_cost_per_million_tokens  = %v
  output_cost_per_million_tokens = 10
%s%s}
`, input, eff, tier)
}

const llmLongContextTier = `
  long_context = {
    threshold_prompt_tokens        = 200000
    input_cost_per_million_tokens  = 2.5
    output_cost_per_million_tokens = 15
  }
`

// checkCurrentLongContext asserts the tier on the rule's newest version.
func checkCurrentLongContext(fake *fakeLlmGatewayServer, wantThreshold int64) resource.TestCheckFunc {
	return func(*terraform.State) error {
		group := fake.pricingGroupSnapshot("gemini-2.5-pro", "google_ai")
		if len(group) == 0 {
			return fmt.Errorf("rule has no versions")
		}
		got := group[0].LongContext
		switch {
		case wantThreshold == 0 && got != nil:
			return fmt.Errorf("newest version still carries a tier: %+v", *got)
		case wantThreshold != 0 && got == nil:
			return fmt.Errorf("newest version (input %v) has no long-context tier", group[0].InputCost)
		case wantThreshold != 0 && got.ThresholdPromptTokens != wantThreshold:
			return fmt.Errorf("tier threshold = %d, want %d", got.ThresholdPromptTokens, wantThreshold)
		}
		return nil
	}
}

// A price change appends a version that copies nothing from its predecessor,
// so the tier must be re-sent with it; before long_context was bound, every
// price change from Terraform silently dropped a tier.
func TestLlmModelPricingResource_longContext(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const name = "barndoor_llm_model_pricing.test"

	var versionID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmPricingArchived(fake),
		Steps: []resource.TestStep{
			{
				Config: llmLongContextPricingHCL(1.25, llmLongContextTier, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(name, "long_context.threshold_prompt_tokens", "200000"),
					resource.TestCheckResourceAttr(name, "long_context.input_cost_per_million_tokens", "2.5"),
					resource.TestCheckNoResourceAttr(name, "long_context.cache_read_cost_per_million_tokens"),
					checkCurrentLongContext(fake, 200000),
				),
			},
			{Config: llmLongContextPricingHCL(1.25, llmLongContextTier, ""), PlanOnly: true},
			{
				// The regression: a base-price change keeps the tier.
				Config: llmLongContextPricingHCL(1.5, llmLongContextTier, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkCurrentLongContext(fake, 200000),
					func(s *terraform.State) error {
						versionID = s.RootModule().Resources[name].Primary.ID
						return nil
					},
				),
			},
			{
				// A tier-only change is a real price change: a new version.
				Config: llmLongContextPricingHCL(1.5, `
  long_context = {
    threshold_prompt_tokens             = 128000
    input_cost_per_million_tokens       = 2.5
    output_cost_per_million_tokens      = 15
    cache_read_cost_per_million_tokens  = 0.625
  }
`, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectUnknownValue(name, tfjsonpath.New("id")),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkCurrentLongContext(fake, 128000),
					resource.TestCheckResourceAttr(name, "long_context.cache_read_cost_per_million_tokens", "0.625"),
					func(s *terraform.State) error {
						if s.RootModule().Resources[name].Primary.ID == versionID {
							return fmt.Errorf("a tier change must append a new version")
						}
						return nil
					},
				),
			},
			{
				Config: llmLongContextPricingHCL(1.5, "", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(name, "long_context.threshold_prompt_tokens"),
					checkCurrentLongContext(fake, 0),
				),
			},
			{Config: llmLongContextPricingHCL(1.5, "", ""), PlanOnly: true},
			{
				// A tier added in the app is drift Terraform reports, not one
				// its next price change silently drops.
				PreConfig: func() {
					fake.mu.Lock()
					defer fake.mu.Unlock()
					group := fake.pricingGroup("gemini-2.5-pro", "google_ai")
					current := pricingCurrentOf(group, time.Now().UTC())
					current.LongContext = &fakeLlmLongContext{ThresholdPromptTokens: 200000, InputCost: 2.5, OutputCost: 15}
				},
				Config:             llmLongContextPricingHCL(1.5, "", ""),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: llmLongContextPricingHCL(1.5, llmLongContextTier, ""),
				Check:  checkCurrentLongContext(fake, 200000),
			},
			{
				ResourceName:            name,
				ImportState:             true,
				ImportStateId:           "google_ai|gemini-2.5-pro",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"id"},
			},
		},
	})
}

// A still-pending scheduled version is edited in place, where long_context is
// tri-state: an object replaces the tier, a null removes it.
func TestLlmModelPricingResource_longContextScheduled(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const future = "2030-01-01T00:00:00Z"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmPricingArchived(fake),
		Steps: []resource.TestStep{
			{Config: llmLongContextPricingHCL(1.25, "", "")},
			{
				Config: llmLongContextPricingHCL(1, llmLongContextTier, future),
				Check:  checkCurrentLongContext(fake, 200000),
			},
			{
				Config: llmLongContextPricingHCL(1, "", future),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkCurrentLongContext(fake, 0),
					func(*terraform.State) error {
						if n := len(fake.pricingGroupSnapshot("gemini-2.5-pro", "google_ai")); n != 2 {
							return fmt.Errorf("the scheduled edit appended a version: %d versions", n)
						}
						return nil
					},
				),
			},
			{Config: llmLongContextPricingHCL(1, "", future), PlanOnly: true},
		},
	})
}

func TestLlmModelPricingResource_longContextRejectedAtPlan(t *testing.T) {
	setupLlmGatewayTest(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: llmLongContextPricingHCL(1, `
  long_context = {
    threshold_prompt_tokens        = 0
    input_cost_per_million_tokens  = 2.5
    output_cost_per_million_tokens = 15
  }
`, ""),
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`threshold_prompt_tokens[\s\S]*at least 1`),
		}},
	})
}
