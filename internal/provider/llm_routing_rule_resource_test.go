// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	frameworkresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// --- schema tests --------------------------------------------------------------

func TestLlmRoutingRuleResource_Metadata(t *testing.T) {
	var resp frameworkresource.MetadataResponse
	NewLlmRoutingRuleResource().Metadata(context.Background(),
		frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_llm_routing_rule"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestLlmRoutingRuleResource_Schema(t *testing.T) {
	var resp frameworkresource.SchemaResponse
	NewLlmRoutingRuleResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{
		"id", "org_id", "policy_id", "name", "description", "floor_slot", "deny_slots", "enabled",
	} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
	for _, required := range []string{"policy_id", "name", "description"} {
		if !resp.Schema.Attributes[required].IsRequired() {
			t.Errorf("%s should be Required", required)
		}
	}
	for _, computed := range []string{"id", "org_id", "enabled"} {
		if !resp.Schema.Attributes[computed].IsComputed() {
			t.Errorf("%s should be Computed", computed)
		}
	}
}

// The rule endpoints answer authoring-rule failures with a 422 and a flat
// `{"error": "..."}` body; the message must surface verbatim, like a 400.
func TestAddLlmGatewayAPIError_flat422(t *testing.T) {
	var diags diag.Diagnostics
	addLlmGatewayAPIError(&diags, "LLM routing rule", "create the LLM routing rule", &apiError{
		method: http.MethodPost, path: "/x", status: http.StatusUnprocessableEntity,
		body: `{"error":"a rule with this name already exists on this routing policy"}`,
	})
	if len(diags) != 1 {
		t.Fatalf("want one diagnostic, got %+v", diags)
	}
	if got, want := diags[0].Summary(), "LLM routing rule rejected by the LLM Gateway API"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if got, want := diags[0].Detail(), "a rule with this name already exists on this routing policy"; got != want {
		t.Errorf("detail = %q, want %q", got, want)
	}
}

func TestAddLlmRoutingRuleConflictWarnings(t *testing.T) {
	var diags diag.Diagnostics
	addLlmRoutingRuleConflictWarnings(&diags, "Legal", []llmRoutingRuleConflict{
		{Rule: "Legal", ConflictsWith: "Bulk", Detail: "'Legal' requires slot 1 or higher, which 'Bulk' bans."},
		{Rule: "Impossible", ConflictsWith: "Impossible", Detail: "bans every slot"},
		{Rule: "Other", ConflictsWith: "Legal", Detail: "the other way round"},
	})
	if diags.HasError() {
		t.Fatalf("conflicts must be warnings, got %+v", diags)
	}
	if len(diags) != 2 {
		t.Fatalf("want the 2 conflicts that involve the rule, got %d: %+v", len(diags), diags)
	}
	if !strings.Contains(diags[0].Detail(), `"Legal" conflicts with "Bulk"`) {
		t.Errorf("unexpected detail %q", diags[0].Detail())
	}

	diags = nil
	addLlmRoutingRuleConflictWarnings(&diags, "Impossible", []llmRoutingRuleConflict{
		{Rule: "Impossible", ConflictsWith: "Impossible", Detail: "bans every slot"},
	})
	if len(diags) != 1 || !strings.Contains(diags[0].Detail(), `"Impossible" contradicts itself`) {
		t.Errorf("self-conflict not reported as such: %+v", diags)
	}
}

// --- fixtures --------------------------------------------------------------------------

// llmRoutingRuleBaseHCL is the mappings plus a three-slot "auto" policy that
// rules attach to.
func llmRoutingRuleBaseHCL(providerID string) string {
	return llmRoutingMappingsHCL(providerID) + llmRoutingPolicyHCL(llmRoutingThreeSlots)
}

func llmRoutingRuleHCL(name, body string) string {
	return fmt.Sprintf(`
resource "barndoor_llm_routing_rule" %q {
  policy_id = barndoor_llm_routing_policy.auto.id
%s
}
`, name, body)
}

// --- lifecycle --------------------------------------------------------------------------

func TestLlmRoutingRuleResource_lifecycle(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	provider := fake.seedProvider()
	const resourceName = "barndoor_llm_routing_rule.legal"
	base := llmRoutingRuleBaseHCL(provider.ID)

	floorConfig := base + llmRoutingRuleHCL("legal", `
  name        = "Legal review"
  description = "Contract review or legal analysis must use at least the mid slot"
  floor_slot  = 1`)
	// A heredoc ends in a newline the platform trims; that must not diff.
	denyConfig := base + llmRoutingRuleHCL("legal", `
  name        = "Legal and compliance"
  description = <<-EOT
    Contract review, legal analysis or compliance questions.
  EOT
  deny_slots  = [2, 0, 2]
  enabled     = false`)

	var ruleID, policyID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmRoutingDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: floorConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckResourceAttrPair(resourceName, "policy_id",
						"barndoor_llm_routing_policy.auto", "id"),
					resource.TestCheckResourceAttr(resourceName, "org_id", fakeLlmOrgID),
					resource.TestCheckResourceAttr(resourceName, "floor_slot", "1"),
					resource.TestCheckNoResourceAttr(resourceName, "deny_slots"),
					resource.TestCheckResourceAttr(resourceName, "enabled", "true"),
					func(s *terraform.State) error {
						rs := s.RootModule().Resources[resourceName]
						ruleID, policyID = rs.Primary.ID, rs.Primary.Attributes["policy_id"]
						return nil
					},
				),
			},
			{
				Config:   floorConfig,
				PlanOnly: true,
			},
			{
				// The PUT is a full replace: dropping floor_slot must clear it
				// on the platform rather than keep the old floor.
				Config: denyConfig,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(resourceName, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "name", "Legal and compliance"),
					resource.TestCheckNoResourceAttr(resourceName, "floor_slot"),
					resource.TestCheckResourceAttr(resourceName, "deny_slots.#", "2"),
					resource.TestCheckTypeSetElemAttr(resourceName, "deny_slots.*", "0"),
					resource.TestCheckTypeSetElemAttr(resourceName, "deny_slots.*", "2"),
					resource.TestCheckResourceAttr(resourceName, "enabled", "false"),
					func(*terraform.State) error {
						r := fake.routingRuleSnapshot(ruleID)
						if r == nil {
							return fmt.Errorf("rule %s gone", ruleID)
						}
						if r.FloorSlot != nil || !slices.Equal(r.DenySlots, []int64{0, 2}) || r.Enabled ||
							r.Description != "Contract review, legal analysis or compliance questions." {
							return fmt.Errorf("platform rule = %+v", *r)
						}
						return nil
					},
				),
			},
			{
				Config:   denyConfig,
				PlanOnly: true,
			},
			{
				ResourceName: resourceName,
				ImportState:  true,
				ImportStateIdFunc: func(*terraform.State) (string, error) {
					return policyID + "/" + ruleID, nil
				},
				ImportStateVerify: true,
				// Import reads the trimmed description; state keeps the
				// configured heredoc.
				ImportStateVerifyIgnore: []string{"description"},
			},
			{
				// Back to the floor-only rule, then destroy (rules first).
				Config: floorConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "floor_slot", "1"),
					resource.TestCheckNoResourceAttr(resourceName, "deny_slots"),
					resource.TestCheckResourceAttr(resourceName, "enabled", "true"),
				),
			},
			{
				ResourceName: resourceName,
				ImportState:  true,
				ImportStateIdFunc: func(*terraform.State) (string, error) {
					return policyID + "/" + ruleID, nil
				},
				ImportStateVerify: true,
			},
		},
	})
}

func TestLlmRoutingRuleResource_importRequiresPolicyID(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	provider := fake.seedProvider()
	config := llmRoutingRuleBaseHCL(provider.ID) + llmRoutingRuleHCL("legal", `
  name        = "Legal review"
  description = "Contract review"
  floor_slot  = 1`)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmRoutingDeleted(fake),
		Steps: []resource.TestStep{
			{Config: config},
			{
				ResourceName:  "barndoor_llm_routing_rule.legal",
				ImportState:   true,
				ImportStateId: "dddd0000-0000-0000-0000-000000000001",
				ExpectError:   regexp.MustCompile(`form <policy_id>/<rule_id>`),
			},
		},
	})
}

// --- validation and server rejections ---------------------------------------------------

func TestLlmRoutingRuleResource_mustConstrainSomething(t *testing.T) {
	setupLlmGatewayTest(t)
	for name, body := range map[string]string{
		"neither":          ``,
		"empty deny_slots": `deny_slots = []`,
	} {
		t.Run(name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{
					{
						Config: fmt.Sprintf(`
resource "barndoor_llm_routing_rule" "noop" {
  policy_id   = "cccc0000-0000-0000-0000-000000000001"
  name        = "No-op"
  description = "Anything"
  %s
}
`, body),
						PlanOnly:    true,
						ExpectError: regexp.MustCompile(`Routing rule constrains nothing`),
					},
				},
			})
		})
	}
}

func TestLlmRoutingRuleResource_fieldValidation(t *testing.T) {
	setupLlmGatewayTest(t)
	cases := map[string]struct{ body, want string }{
		"padded name":       {`name = "Legal "` + "\n  description = \"x\"\n  floor_slot = 1", `leading/trailing whitespace`},
		"long name":         {`name = "` + strings.Repeat("n", 121) + `"` + "\n  description = \"x\"\n  floor_slot = 1", `at most 120`},
		"blank description": {`name = "Legal"` + "\n  description = \"   \"\n  floor_slot = 1", `must not be empty`},
		"negative floor":    {`name = "Legal"` + "\n  description = \"x\"\n  floor_slot = -1", `floor_slot value must be between 0`},
		"negative deny":     {`name = "Legal"` + "\n  description = \"x\"\n  deny_slots = [-1]", `must be between 0`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{
					{
						Config: fmt.Sprintf(`
resource "barndoor_llm_routing_rule" "r" {
  policy_id = "cccc0000-0000-0000-0000-000000000001"
  %s
}
`, c.body),
						PlanOnly:    true,
						ExpectError: regexp.MustCompile(c.want),
					},
				},
			})
		})
	}
}

func TestLlmRoutingRuleResource_duplicateNameIs422(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	provider := fake.seedProvider()

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmRoutingDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: llmRoutingRuleBaseHCL(provider.ID) +
					llmRoutingRuleHCL("first", `
  name        = "Legal review"
  description = "Contract review"
  floor_slot  = 1`) +
					llmRoutingRuleHCL("second", `
  name        = "Legal review"
  description = "Legal analysis"
  floor_slot  = 2
  depends_on  = [barndoor_llm_routing_rule.first]`),
				ExpectError: regexp.MustCompile(
					`(?s)LLM routing rule rejected by the LLM Gateway API.*a rule with this name already\s+exists on\s+this\s+routing\s+policy`),
			},
		},
	})
}

func TestLlmRoutingRuleResource_unknownPolicyIs404(t *testing.T) {
	setupLlmGatewayTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "barndoor_llm_routing_rule" "orphan" {
  policy_id   = "cccc0000-0000-0000-0000-000000000099"
  name        = "Legal review"
  description = "Contract review"
  floor_slot  = 1
}
`,
				ExpectError: regexp.MustCompile(`(?s)unexpected status 404.*routing policy\s+'cccc0000-0000-0000-0000-000000000099'\s+not\s+found`),
			},
		},
	})
}

// Contradictory rules are advisory on the platform: the apply must succeed
// (the provider reports the conflict as a warning, never an error).
func TestLlmRoutingRuleResource_conflictsDoNotFail(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	provider := fake.seedProvider()

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmRoutingDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: llmRoutingRuleBaseHCL(provider.ID) +
					llmRoutingRuleHCL("legal", `
  name        = "Legal review"
  description = "Contract review"
  floor_slot  = 1`) +
					llmRoutingRuleHCL("bulk", `
  name        = "Bulk"
  description = "Bulk translation"
  deny_slots  = [1, 2]
  depends_on  = [barndoor_llm_routing_rule.legal]`) +
					llmRoutingRuleHCL("impossible", `
  name        = "Impossible"
  description = "Self-contradictory"
  floor_slot  = 2
  deny_slots  = [2]`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("barndoor_llm_routing_rule.bulk", "id"),
					resource.TestCheckResourceAttrSet("barndoor_llm_routing_rule.impossible", "id"),
					func(*terraform.State) error {
						fake.mu.Lock()
						defer fake.mu.Unlock()
						p := fake.routingPolicies[0]
						if got := len(fakeLlmDetectConflicts(fake.rulesForPolicy(p.ID), len(p.Slots))); got != 3 {
							// Legal vs Bulk, Impossible vs itself, Impossible vs Bulk.
							return fmt.Errorf("expected the fake to report 3 conflicts, got %d", got)
						}
						return nil
					},
				),
			},
		},
	})
}

// --- out-of-band changes --------------------------------------------------------------

func TestLlmRoutingRuleResource_outOfBandDeletePlansRecreate(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	provider := fake.seedProvider()
	const resourceName = "barndoor_llm_routing_rule.legal"
	config := llmRoutingRuleBaseHCL(provider.ID) + llmRoutingRuleHCL("legal", `
  name        = "Legal review"
  description = "Contract review"
  floor_slot  = 1`)

	var firstID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmRoutingDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: func(s *terraform.State) error {
					firstID = s.RootModule().Resources[resourceName].Primary.ID
					return nil
				},
			},
			{
				PreConfig: func() { fake.markRoutingRuleDeleted(t, firstID) },
				Config:    config,
				Check: func(s *terraform.State) error {
					if id := s.RootModule().Resources[resourceName].Primary.ID; id == firstID {
						return fmt.Errorf("expected a new rule id after out-of-band delete, still %s", id)
					}
					return nil
				},
			},
		},
	})
}

// Deleting the policy cascades its rules on the platform. The rule's refresh
// then lists a missing policy (404) and must drop the rule from state, so the
// apply recreates both instead of failing.
func TestLlmRoutingRuleResource_policyDeleteCascades(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	provider := fake.seedProvider()
	const ruleName = "barndoor_llm_routing_rule.legal"
	const policyName = "barndoor_llm_routing_policy.auto"
	config := llmRoutingRuleBaseHCL(provider.ID) + llmRoutingRuleHCL("legal", `
  name        = "Legal review"
  description = "Contract review"
  floor_slot  = 1`)

	var firstRule, firstPolicy string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmRoutingDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: func(s *terraform.State) error {
					firstRule = s.RootModule().Resources[ruleName].Primary.ID
					firstPolicy = s.RootModule().Resources[policyName].Primary.ID
					return nil
				},
			},
			{
				PreConfig: func() {
					fake.markRoutingPolicyDeleted(t, firstPolicy)
					if fake.routingRuleSnapshot(firstRule) != nil {
						t.Fatal("fake did not cascade the policy delete to its rules")
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(policyName, plancheck.ResourceActionCreate),
						plancheck.ExpectResourceAction(ruleName, plancheck.ResourceActionCreate),
					},
				},
				Check: func(s *terraform.State) error {
					rule := s.RootModule().Resources[ruleName].Primary
					policy := s.RootModule().Resources[policyName].Primary
					if rule.ID == firstRule || policy.ID == firstPolicy {
						return fmt.Errorf("expected both recreated, have rule %s policy %s", rule.ID, policy.ID)
					}
					if rule.Attributes["policy_id"] != policy.ID {
						return fmt.Errorf("rule points at %s, want new policy %s", rule.Attributes["policy_id"], policy.ID)
					}
					return nil
				},
			},
		},
	})
}
