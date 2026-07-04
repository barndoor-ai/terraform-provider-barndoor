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

func TestLlmTokenBudgetResource_Metadata(t *testing.T) {
	var resp frameworkresource.MetadataResponse
	NewLlmTokenBudgetResource().Metadata(context.Background(),
		frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_llm_token_budget"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestLlmTokenBudgetResource_Schema(t *testing.T) {
	var resp frameworkresource.SchemaResponse
	NewLlmTokenBudgetResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{
		"id", "org_id", "name", "scope_type", "scope_id", "scope_value", "period",
		"token_limit", "alert_thresholds", "action_on_exhaust", "traffic_type",
		"enabled", "created_at",
	} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
	for _, required := range []string{"name", "scope_type", "period", "token_limit"} {
		if !resp.Schema.Attributes[required].IsRequired() {
			t.Errorf("%s should be Required", required)
		}
	}
	for _, computed := range []string{
		"id", "org_id", "alert_thresholds", "action_on_exhaust", "traffic_type",
		"enabled", "created_at",
	} {
		if !resp.Schema.Attributes[computed].IsComputed() {
			t.Errorf("%s should be Computed", computed)
		}
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

func TestLlmTokenBudgetResource_lifecycle(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_token_budget.test"

	minimalConfig := `
resource "barndoor_llm_token_budget" "test" {
  name        = "Org monthly cap"
  scope_type  = "org"
  period      = "monthly"
  token_limit = 5000000
}
`
	expandedConfig := `
resource "barndoor_llm_token_budget" "test" {
  name        = "Org monthly cap (tuned)"
  scope_type  = "org"
  period      = "weekly"
  token_limit = 1000000

  alert_thresholds  = [50, 75, 95]
  action_on_exhaust = "warn"
  traffic_type      = "llm"
  enabled           = false
}
`

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmBudgetsDeleted(fake),
		Steps: []resource.TestStep{
			{
				// Minimal budget: thresholds, action, traffic lane, and
				// enabled all come from the server defaults.
				Config: minimalConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckResourceAttr(resourceName, "org_id", fakeLlmOrgID),
					resource.TestCheckResourceAttr(resourceName, "alert_thresholds.#", "2"),
					resource.TestCheckResourceAttr(resourceName, "alert_thresholds.0", "80"),
					resource.TestCheckResourceAttr(resourceName, "alert_thresholds.1", "90"),
					resource.TestCheckResourceAttr(resourceName, "action_on_exhaust", "block"),
					resource.TestCheckResourceAttr(resourceName, "traffic_type", "all"),
					resource.TestCheckResourceAttr(resourceName, "enabled", "true"),
					resource.TestCheckResourceAttrSet(resourceName, "created_at"),
				),
			},
			{
				// A second plan over unchanged configuration must be empty.
				Config:   minimalConfig,
				PlanOnly: true,
			},
			{
				// In-place update: retune every updatable knob (the scope
				// stays put — changing it would force a replacement).
				Config: expandedConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "name", "Org monthly cap (tuned)"),
					resource.TestCheckResourceAttr(resourceName, "period", "weekly"),
					resource.TestCheckResourceAttr(resourceName, "token_limit", "1000000"),
					resource.TestCheckResourceAttr(resourceName, "alert_thresholds.#", "3"),
					resource.TestCheckResourceAttr(resourceName, "alert_thresholds.2", "95"),
					resource.TestCheckResourceAttr(resourceName, "action_on_exhaust", "warn"),
					resource.TestCheckResourceAttr(resourceName, "traffic_type", "llm"),
					resource.TestCheckResourceAttr(resourceName, "enabled", "false"),
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

func TestLlmTokenBudgetResource_disabledOnCreateConverges(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_token_budget.test"

	// The create endpoint has no enabled field; the resource must converge
	// enabled = false with a follow-up update in the same apply.
	config := `
resource "barndoor_llm_token_budget" "test" {
  name        = "Staged budget"
  scope_type  = "group"
  scope_value = "contractors"
  period      = "daily"
  token_limit = 100000
  enabled     = false
}
`

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmBudgetsDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "enabled", "false"),
					func(*terraform.State) error {
						fake.mu.Lock()
						defer fake.mu.Unlock()
						if len(fake.budgets) != 1 {
							return fmt.Errorf("expected 1 budget, have %d", len(fake.budgets))
						}
						if fake.budgets[0].Enabled {
							return fmt.Errorf("budget is still enabled on the platform")
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

func TestLlmTokenBudgetResource_outOfBandDeletePlansRecreate(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_token_budget.test"

	config := `
resource "barndoor_llm_token_budget" "test" {
  name        = "tf-llm-budget-oob"
  scope_type  = "org"
  period      = "monthly"
  token_limit = 42
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
				PreConfig: func() { fake.markBudgetDeleted(t, firstID) },
				Config:    config,
				Check: func(s *terraform.State) error {
					rs, ok := s.RootModule().Resources[resourceName]
					if !ok {
						return fmt.Errorf("%s not in state", resourceName)
					}
					if rs.Primary.ID == firstID {
						return fmt.Errorf("expected a new budget id after out-of-band delete, still %s", firstID)
					}
					return nil
				},
			},
		},
	})
}

func TestLlmTokenBudgetResource_duplicateScopePeriodIs409(t *testing.T) {
	setupLlmGatewayTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Two budgets on the same (scope, traffic_type, period) would
				// share a usage counter; the platform's 409 must surface.
				Config: `
resource "barndoor_llm_token_budget" "first" {
  name        = "tf-llm-budget-a"
  scope_type  = "org"
  period      = "monthly"
  token_limit = 1000
}

resource "barndoor_llm_token_budget" "second" {
  name        = "tf-llm-budget-b"
  scope_type  = "org"
  period      = "monthly"
  token_limit = 2000

  depends_on = [barndoor_llm_token_budget.first]
}
`,
				ExpectError: regexp.MustCompile(`A budget with this scope, target, traffic type, and period already exists`),
			},
		},
	})
}
