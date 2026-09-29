// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// member_of_group behaves identically on the two governance resources that
// carry it, so each case runs against both.
type llmMemberOfGroupCase struct {
	resourceType string
	// body is the resource-specific attribute block (limits, period, …) that
	// every config shares.
	body string
	// stored returns the member_of_group the fake holds for the only row.
	stored func(fake *fakeLlmGatewayServer) (*string, int)
}

var llmMemberOfGroupCases = []llmMemberOfGroupCase{
	{
		resourceType: "barndoor_llm_token_budget",
		body: `
  period      = "monthly"
  token_limit = 100000`,
		stored: func(fake *fakeLlmGatewayServer) (*string, int) {
			if len(fake.budgets) == 0 {
				return nil, 0
			}
			return fake.budgets[0].MemberOfGroup, len(fake.budgets)
		},
	},
	{
		resourceType: "barndoor_llm_rate_limit",
		body: `
  requests_per_minute = 60`,
		stored: func(fake *fakeLlmGatewayServer) (*string, int) {
			if len(fake.rateLimits) == 0 {
				return nil, 0
			}
			return fake.rateLimits[0].MemberOfGroup, len(fake.rateLimits)
		},
	},
}

func (c llmMemberOfGroupCase) config(scope string) string {
	return fmt.Sprintf(`
resource %q "test" {
  name = "Per-member allowance"
%s
%s
}
`, c.resourceType, scope, c.body)
}

func TestLlmMemberOfGroup_createReadAndReplace(t *testing.T) {
	for _, c := range llmMemberOfGroupCases {
		t.Run(c.resourceType, func(t *testing.T) {
			fake := setupLlmGatewayTest(t)
			resourceName := c.resourceType + ".test"

			engineering := c.config(`  scope_type      = "user"
  member_of_group = "engineering"`)
			research := c.config(`  scope_type      = "user"
  member_of_group = "research"`)

			var firstID string
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{
					{
						Config: engineering,
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr(resourceName, "scope_type", "user"),
							resource.TestCheckResourceAttr(resourceName, "member_of_group", "engineering"),
							resource.TestCheckNoResourceAttr(resourceName, "scope_id"),
							resource.TestCheckNoResourceAttr(resourceName, "scope_value"),
							func(s *terraform.State) error {
								firstID = s.RootModule().Resources[resourceName].Primary.ID
								fake.mu.Lock()
								defer fake.mu.Unlock()
								if got, _ := c.stored(fake); got == nil || *got != "engineering" {
									return fmt.Errorf("platform member_of_group = %v, want engineering", got)
								}
								return nil
							},
						),
					},
					{
						Config:   engineering,
						PlanOnly: true,
					},
					{
						// Import reassembles the filter from the listing.
						ResourceName:      resourceName,
						ImportState:       true,
						ImportStateVerify: true,
					},
					{
						// Neither update API accepts member_of_group; a change
						// must replace rather than PUT a field the platform
						// would silently drop.
						Config: research,
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{
								plancheck.ExpectResourceAction(resourceName, plancheck.ResourceActionReplace),
							},
						},
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr(resourceName, "member_of_group", "research"),
							func(s *terraform.State) error {
								if s.RootModule().Resources[resourceName].Primary.ID == firstID {
									return fmt.Errorf("expected a replacement, id unchanged (%s)", firstID)
								}
								fake.mu.Lock()
								defer fake.mu.Unlock()
								got, n := c.stored(fake)
								if n != 1 || got == nil || *got != "research" {
									return fmt.Errorf("platform holds %d rows, member_of_group = %v; want 1 row, research", n, got)
								}
								return nil
							},
						),
					},
					{
						// Removing the filter is also a replacement: the rule
						// goes from per-member to a single user-scoped rule.
						Config: c.config(`  scope_type = "user"`),
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{
								plancheck.ExpectResourceAction(resourceName, plancheck.ResourceActionReplace),
							},
						},
						Check: resource.TestCheckNoResourceAttr(resourceName, "member_of_group"),
					},
				},
			})
		})
	}
}

// Every shape the platform CHECKs reject must fail at plan, before any API
// call.
func TestLlmMemberOfGroup_rejectedShapesFailAtPlan(t *testing.T) {
	shapes := []struct {
		name  string
		scope string
		want  *regexp.Regexp
	}{
		{
			name: "pooled group scope",
			scope: `  scope_type      = "group"
  scope_value     = "engineering"
  member_of_group = "engineering"`,
			want: regexp.MustCompile(`member_of_group requires scope_type = "user"`),
		},
		{
			name: "org scope",
			scope: `  scope_type      = "org"
  member_of_group = "engineering"`,
			want: regexp.MustCompile(`member_of_group requires scope_type = "user"`),
		},
		{
			name: "one named user",
			scope: `  scope_type      = "user"
  scope_id        = "99999999-0000-0000-0000-000000000001"
  member_of_group = "engineering"`,
			want: regexp.MustCompile(`member_of_group cannot be combined with scope_id`),
		},
		{
			name: "user with scope_value",
			scope: `  scope_type      = "user"
  scope_value     = "someone"
  member_of_group = "engineering"`,
			want: regexp.MustCompile(`member_of_group cannot be combined with scope_value`),
		},
		{
			name: "blank",
			scope: `  scope_type      = "user"
  member_of_group = "  "`,
			want: regexp.MustCompile(`must not be empty or have leading/trailing\s+whitespace`),
		},
		{
			name: "padded",
			scope: `  scope_type      = "user"
  member_of_group = " engineering"`,
			want: regexp.MustCompile(`must not be empty or have leading/trailing\s+whitespace`),
		},
	}

	for _, c := range llmMemberOfGroupCases {
		for _, shape := range shapes {
			t.Run(c.resourceType+"/"+shape.name, func(t *testing.T) {
				fake := setupLlmGatewayTest(t)
				resource.UnitTest(t, resource.TestCase{
					ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
					Steps: []resource.TestStep{
						{
							Config:      c.config(shape.scope),
							PlanOnly:    true,
							ExpectError: shape.want,
						},
					},
				})
				fake.mu.Lock()
				defer fake.mu.Unlock()
				if _, n := c.stored(fake); n != 0 {
					t.Fatalf("a rejected shape reached the API: %d rows created", n)
				}
			})
		}
	}
}
