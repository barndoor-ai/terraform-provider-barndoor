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

func TestDlpMcpBaselinePolicyResource_Metadata(t *testing.T) {
	var resp frameworkresource.MetadataResponse
	NewDlpMcpBaselinePolicyResource().Metadata(context.Background(),
		frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)
	if got, want := resp.TypeName, "barndoor_dlp_mcp_baseline_policy"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestDlpMcpBaselinePolicyResource_Schema(t *testing.T) {
	var resp frameworkresource.SchemaResponse
	NewDlpMcpBaselinePolicyResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}
	for _, attr := range []string{"id", "rules", "created_by", "updated_by", "created_at", "updated_at"} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
	if !resp.Schema.Attributes["rules"].IsRequired() {
		t.Error("rules should be Required")
	}
	if !resp.Schema.Attributes["id"].IsComputed() {
		t.Error("id should be Computed")
	}
}

func dlpMcpBaselineConfig(rules string) string {
	return fmt.Sprintf(`
resource "barndoor_dlp_mcp_baseline_policy" "this" {
  rules = %s
}
`, rules)
}

func TestDlpMcpBaselinePolicyResource_lifecycle(t *testing.T) {
	fake := setupDlpMcpTest(t)
	const resourceName = "barndoor_dlp_mcp_baseline_policy.this"

	tokenize := testMcpRuleHCL("BOTH", "POLICY_ACTION_TOKENIZE", "")
	scoped := testMcpRuleHCL("TOOL_OUTPUT", "POLICY_ACTION_REDACT", fmt.Sprintf(`
      agent_scope = { mode = "ONLY", agent_ids = [%q, %q] }`, testMcpAgentA, testMcpAgentB))

	// The Baseline policy was saved in the app before Terraform took it over,
	// with a rule identical to the configured first one.
	seeded := testMcpRule("BOTH", "POLICY_ACTION_TOKENIZE")
	fake.seedBaseline(seeded)
	seededIDs := fake.ruleIDs(fakeMcpBaselineID)

	initial := dlpMcpBaselineConfig(fmt.Sprintf("[\n    %s,\n  ]", tokenize))
	updated := dlpMcpBaselineConfig(fmt.Sprintf("[\n    %s,\n    %s,\n  ]", tokenize, scoped))

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkDlpMcpBaselineCleared(fake),
		Steps: []resource.TestStep{
			{
				Config: initial,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "id", fakeMcpBaselineID),
					resource.TestCheckResourceAttr(resourceName, "rules.#", "1"),
					resource.TestCheckNoResourceAttr(resourceName, "rules.0.agent_scope"),
					func(*terraform.State) error {
						// Taking over a matching rule keeps it rather than
						// recreating it.
						if got := fake.ruleIDs(fakeMcpBaselineID); len(got) != 1 || got[0] != seededIDs[0] {
							return fmt.Errorf("rule ids = %v, want the app-saved rule %v kept", got, seededIDs)
						}
						return nil
					},
				),
			},
			{Config: initial, PlanOnly: true},
			{
				Config: updated,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "rules.#", "2"),
					resource.TestCheckResourceAttr(resourceName, "rules.1.agent_scope.mode", "ONLY"),
					resource.TestCheckResourceAttr(resourceName, "rules.1.agent_scope.agent_ids.#", "2"),
					func(*terraform.State) error {
						if got := fake.ruleIDs(fakeMcpBaselineID); len(got) != 2 || got[0] != seededIDs[0] {
							return fmt.Errorf("rule ids = %v, want the first rule's id %s kept", got, seededIDs[0])
						}
						return nil
					},
				),
			},
			{Config: updated, PlanOnly: true},
			{
				// Any import ID works; Read replaces it with the UUID.
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateId:     "baseline",
				ImportStateVerify: true,
			},
			{
				// An empty rule set is valid and converges.
				Config: dlpMcpBaselineConfig("[]"),
				Check:  resource.TestCheckResourceAttr(resourceName, "rules.#", "0"),
			},
			{Config: dlpMcpBaselineConfig("[]"), PlanOnly: true},
		},
	})
}

func TestDlpMcpBaselinePolicyResource_createsOnFirstSave(t *testing.T) {
	fake := setupDlpMcpTest(t)
	const resourceName = "barndoor_dlp_mcp_baseline_policy.this"
	config := dlpMcpBaselineConfig(fmt.Sprintf("[%s]", testMcpRuleHCL("TOOL_INPUT", "POLICY_ACTION_BLOCK", "")))

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkDlpMcpBaselineCleared(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "id", fakeMcpBaselineID),
					resource.TestCheckResourceAttr(resourceName, "rules.0.runtime_boundary", "TOOL_INPUT"),
				),
			},
			{Config: config, PlanOnly: true},
		},
	})
}

func TestDlpMcpBaselinePolicyResource_importNeverSaved(t *testing.T) {
	setupDlpMcpTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:        dlpMcpBaselineConfig("[]"),
				ResourceName:  "barndoor_dlp_mcp_baseline_policy.this",
				ImportState:   true,
				ImportStateId: "baseline",
				ExpectError:   regexp.MustCompile(`Cannot import non-existent remote object`),
			},
		},
	})
}

func TestDlpMcpBaselinePolicyResource_legacyOrgIs409(t *testing.T) {
	fake := setupDlpMcpTest(t)
	fake.setPolicyModel("LEGACY")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      dlpMcpBaselineConfig("[]"),
				ExpectError: regexp.MustCompile(`Organization is not on MCP server policies`),
			},
		},
	})
}
