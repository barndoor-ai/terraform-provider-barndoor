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
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const (
	testMcpAgentA = "a1111111-1111-4111-8111-11111111111f"
	testMcpAgentB = "22222222-2222-4222-8222-222222222222"
)

// --- schema tests --------------------------------------------------------------

func TestDlpMcpPolicyResource_Metadata(t *testing.T) {
	var resp frameworkresource.MetadataResponse
	NewDlpMcpPolicyResource().Metadata(context.Background(),
		frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)
	if got, want := resp.TypeName, "barndoor_dlp_mcp_policy"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestDlpMcpPolicyResource_Schema(t *testing.T) {
	var resp frameworkresource.SchemaResponse
	NewDlpMcpPolicyResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}
	for _, attr := range []string{"id", "name", "mcp_server_ids", "rules", "created_by", "updated_by", "created_at", "updated_at"} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
	for _, required := range []string{"name", "mcp_server_ids", "rules"} {
		if !resp.Schema.Attributes[required].IsRequired() {
			t.Errorf("%s should be Required", required)
		}
	}
}

// --- conversion tests ------------------------------------------------------------

func testMcpRule(boundary, action string) dlpMcpRulePayload {
	return dlpMcpRulePayload{
		RuntimeBoundary:    boundary,
		Action:             action,
		AgentScope:         dlpMcpAgentScopePayload{Mode: dlpMcpScopeAll, AgentIDs: []string{}},
		PrincipalScope:     dlpMcpPrincipalScopePayload{Mode: dlpMcpScopeAll, Principals: []dlpPrincipalPayload{}},
		DetectionEngineIDs: []string{fakeDlpDetectionEngineID},
	}
}

func TestAssignDlpMcpRuleIDs(t *testing.T) {
	a := testMcpRule("TOOL_INPUT", "POLICY_ACTION_BLOCK")
	b := testMcpRule("TOOL_OUTPUT", "POLICY_ACTION_REDACT")
	b.AgentScope = dlpMcpAgentScopePayload{Mode: dlpMcpScopeOnly, AgentIDs: []string{testMcpAgentB, testMcpAgentA}}
	c := testMcpRule("BOTH", "POLICY_ACTION_ALERT_ONLY")

	storedA, storedB, storedC := a, b, c
	storedA.ID, storedB.ID, storedC.ID = strPtr("id-a"), strPtr("id-b"), strPtr("id-c")
	// The API echoes agent ids in its own order; content matching must not
	// care.
	storedB.AgentScope.AgentIDs = []string{testMcpAgentA, testMcpAgentB}
	stored := []dlpMcpRulePayload{storedA, storedB, storedC}

	// New order: B moved to the front unchanged, A edited in place (now at
	// position 1, where B was), C dropped, a brand-new rule appended.
	editedA := a
	editedA.Action = "POLICY_ACTION_TOKENIZE"
	fresh := testMcpRule("TOOL_INPUT", "POLICY_ACTION_OMIT")
	planned := []dlpMcpRulePayload{b, editedA, fresh}

	assignDlpMcpRuleIDs(planned, stored)

	got := []string{}
	for _, r := range planned {
		if r.ID == nil {
			got = append(got, "<new>")
		} else {
			got = append(got, *r.ID)
		}
	}
	// B keeps its id by content wherever it moved. Edited A sits at position
	// 1, whose stored rule (B) is already claimed, so it is created. The new
	// rule sits at position 2, where C was dropped, and so takes over C's id
	// as an in-place edit.
	want := []string{"id-b", "<new>", "id-c"}
	if !slices.Equal(got, want) {
		t.Errorf("assigned ids = %v, want %v", got, want)
	}
}

func TestAssignDlpMcpRuleIDs_EditInPlaceKeepsID(t *testing.T) {
	a := testMcpRule("TOOL_INPUT", "POLICY_ACTION_BLOCK")
	b := testMcpRule("TOOL_OUTPUT", "POLICY_ACTION_REDACT")
	storedA, storedB := a, b
	storedA.ID, storedB.ID = strPtr("id-a"), strPtr("id-b")

	editedB := b
	editedB.DryRun = true
	planned := []dlpMcpRulePayload{a, editedB}
	assignDlpMcpRuleIDs(planned, []dlpMcpRulePayload{storedA, storedB})

	if planned[0].ID == nil || *planned[0].ID != "id-a" {
		t.Errorf("unchanged rule should keep id-a, got %v", planned[0].ID)
	}
	if planned[1].ID == nil || *planned[1].ID != "id-b" {
		t.Errorf("rule edited in place should keep id-b, got %v", planned[1].ID)
	}
}

func TestApplyDlpMcpRules_SettlesDefaultScopesToNull(t *testing.T) {
	ctx := context.Background()
	wire := []dlpMcpRulePayload{
		testMcpRule("TOOL_INPUT", "POLICY_ACTION_BLOCK"),
		testMcpRule("TOOL_OUTPUT", "POLICY_ACTION_BLOCK"),
	}
	prior := []dlpMcpRuleModel{
		{},
		{AgentScope: &dlpMcpAgentScopeModel{Mode: types.StringValue("ALL"), AgentIDs: types.SetNull(types.StringType)}},
	}

	rules, err := applyDlpMcpRules(ctx, wire, prior)
	if err != nil {
		t.Fatalf("applyDlpMcpRules: %v", err)
	}
	if rules[0].AgentScope != nil || rules[0].PrincipalScope != nil {
		t.Error("an omitted scope reported back as mode ALL must settle to null")
	}
	if rules[1].AgentScope == nil || rules[1].AgentScope.Mode.ValueString() != "ALL" {
		t.Error("an explicit `mode = ALL` scope must be kept")
	} else if !rules[1].AgentScope.AgentIDs.IsNull() {
		t.Error("an explicit ALL scope's empty agent_ids must settle to null")
	}
	if rules[0].Label.IsNull() != true {
		t.Error("an unset label must stay null")
	}

	empty, err := applyDlpMcpRules(ctx, nil, nil)
	if err != nil {
		t.Fatalf("applyDlpMcpRules: %v", err)
	}
	if empty == nil {
		t.Error("an empty rule set must map to a known empty list, not null")
	}
}

func TestAddDlpMcpPolicyAPIError(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"legacy org": {
			status: http.StatusConflict,
			body:   `{"message":"Your organization doesn't use MCP policies yet. Keep editing its Data Protection policies."}`,
			want:   "Organization is not on MCP server policies",
		},
		"server already assigned": {
			status: http.StatusConflict,
			body:   `{"message":"Each MCP server can belong to only one policy.","code":"mcp_server_already_assigned"}`,
			want:   "MCP server already belongs to another policy",
		},
		"name taken": {
			status: http.StatusConflict,
			body:   `{"message":"name taken"}`,
			want:   "MCP policy conflicts with an existing one",
		},
		"agent directory down": {
			status: http.StatusServiceUnavailable,
			body:   `{"message":"retry"}`,
			want:   "Agent directory unavailable",
		},
		"validation": {
			status: http.StatusUnprocessableEntity,
			body:   `{"message":"rule 2: list 1 to 200 agents when agent scope is ONLY"}`,
			want:   "MCP policy rejected by the Data Protection API",
		},
	} {
		t.Run(name, func(t *testing.T) {
			var diags diag.Diagnostics
			addDlpMcpPolicyAPIError(&diags, "save", &apiError{method: "PUT", path: "x", status: tc.status, body: tc.body})
			if len(diags) != 1 || diags[0].Summary() != tc.want {
				t.Errorf("diagnostics = %+v, want summary %q", diags, tc.want)
			}
		})
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

func dlpMcpPolicyConfig(name, servers, rules string) string {
	return fmt.Sprintf(`
resource "barndoor_dlp_mcp_policy" "test" {
  name           = %q
  mcp_server_ids = %s
  rules          = %s
}
`, name, servers, rules)
}

func testMcpRuleHCL(boundary, action, extra string) string {
	return fmt.Sprintf(`{
      runtime_boundary     = %q
      action               = %q
      detection_engine_ids = [%q]
%s
    }`, boundary, action, fakeDlpDetectionEngineID, extra)
}

func TestDlpMcpPolicyResource_lifecycle(t *testing.T) {
	fake := setupDlpMcpTest(t)
	const resourceName = "barndoor_dlp_mcp_policy.test"

	ruleBlock := testMcpRuleHCL("TOOL_INPUT", "POLICY_ACTION_BLOCK", "")
	ruleScoped := testMcpRuleHCL("TOOL_OUTPUT", "POLICY_ACTION_MASK", fmt.Sprintf(`
      label       = "Mask secrets for contractors"
      mask_format = { keep = "SUFFIX", keep_chars = 4 }
      agent_scope = { mode = "EXCEPT", agent_ids = [%q] }
      principal_scope = {
        mode       = "ONLY"
        principals = [{ principal_type = "GROUP", principal_id = "contractors" }]
      }`, testMcpAgentA))
	initial := dlpMcpPolicyConfig("tf-mcp-policy", `["github", "jira"]`,
		fmt.Sprintf("[\n    %s,\n    %s,\n  ]", ruleBlock, ruleScoped))

	var policyID string
	var firstRuleIDs []string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpMcpPoliciesDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: initial,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckResourceAttr(resourceName, "mcp_server_ids.#", "2"),
					resource.TestCheckResourceAttr(resourceName, "rules.#", "2"),
					resource.TestCheckResourceAttr(resourceName, "rules.0.dry_run", "false"),
					resource.TestCheckNoResourceAttr(resourceName, "rules.0.agent_scope"),
					resource.TestCheckNoResourceAttr(resourceName, "rules.0.principal_scope"),
					resource.TestCheckNoResourceAttr(resourceName, "rules.0.mask_format"),
					resource.TestCheckResourceAttr(resourceName, "rules.1.label", "Mask secrets for contractors"),
					resource.TestCheckResourceAttr(resourceName, "rules.1.mask_format.mask_char", "X"),
					resource.TestCheckResourceAttr(resourceName, "rules.1.mask_format.preserve_separators", "true"),
					resource.TestCheckResourceAttr(resourceName, "rules.1.agent_scope.mode", "EXCEPT"),
					resource.TestCheckResourceAttr(resourceName, "rules.1.principal_scope.principals.#", "1"),
					func(s *terraform.State) error {
						policyID = s.RootModule().Resources[resourceName].Primary.ID
						firstRuleIDs = fake.ruleIDs(policyID)
						return nil
					},
				),
			},
			{
				// A second plan over unchanged configuration must be empty.
				Config:   initial,
				PlanOnly: true,
			},
			{
				// Rename, swap a server, and edit the first rule in place.
				// The scoped rule is unchanged, so both rules keep their ids
				// and the API mints none.
				Config: dlpMcpPolicyConfig("tf-mcp-policy-renamed", `["github", "linear"]`,
					fmt.Sprintf("[\n    %s,\n    %s,\n  ]",
						testMcpRuleHCL("TOOL_INPUT", "POLICY_ACTION_BLOCK", "      dry_run = true"), ruleScoped)),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "name", "tf-mcp-policy-renamed"),
					resource.TestCheckTypeSetElemAttr(resourceName, "mcp_server_ids.*", "linear"),
					resource.TestCheckResourceAttr(resourceName, "rules.0.dry_run", "true"),
					func(*terraform.State) error {
						if got := fake.ruleIDs(policyID); !slices.Equal(got, firstRuleIDs) {
							return fmt.Errorf("rule ids = %v, want the original %v (rules recreated)", got, firstRuleIDs)
						}
						if n := fake.mintedRuleCount(); n != 2 {
							return fmt.Errorf("the API minted %d rule ids, want only the 2 from create", n)
						}
						return nil
					},
				),
			},
			{
				// Drop the first rule and the scopes on the remaining one:
				// the scoped rule keeps its id, and state settles back to null.
				Config: dlpMcpPolicyConfig("tf-mcp-policy-renamed", `["github", "linear"]`,
					fmt.Sprintf("[\n    %s,\n  ]", testMcpRuleHCL("TOOL_OUTPUT", "POLICY_ACTION_MASK", `
      label       = "Mask secrets for contractors"
      mask_format = { keep = "SUFFIX", keep_chars = 4 }`))),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "rules.#", "1"),
					resource.TestCheckNoResourceAttr(resourceName, "rules.0.agent_scope"),
					resource.TestCheckNoResourceAttr(resourceName, "rules.0.principal_scope"),
					func(*terraform.State) error {
						if got := fake.ruleIDs(policyID); len(got) != 1 || got[0] != firstRuleIDs[0] {
							return fmt.Errorf("rule ids = %v, want the first stored id %s kept by position", got, firstRuleIDs[0])
						}
						return nil
					},
				),
			},
			{
				Config: dlpMcpPolicyConfig("tf-mcp-policy-renamed", `["github", "linear"]`,
					fmt.Sprintf("[\n    %s,\n  ]", testMcpRuleHCL("TOOL_OUTPUT", "POLICY_ACTION_MASK", `
      label       = "Mask secrets for contractors"
      mask_format = { keep = "SUFFIX", keep_chars = 4 }`))),
				PlanOnly: true,
			},
			{
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestDlpMcpPolicyResource_emptyRules(t *testing.T) {
	fake := setupDlpMcpTest(t)
	const resourceName = "barndoor_dlp_mcp_policy.test"
	config := dlpMcpPolicyConfig("tf-mcp-exempt", `["github"]`, "[]")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpMcpPoliciesDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  resource.TestCheckResourceAttr(resourceName, "rules.#", "0"),
			},
			{Config: config, PlanOnly: true},
		},
	})
}

func TestDlpMcpPolicyResource_outOfBandDeletePlansRecreate(t *testing.T) {
	fake := setupDlpMcpTest(t)
	const resourceName = "barndoor_dlp_mcp_policy.test"
	config := dlpMcpPolicyConfig("tf-mcp-oob", `["github"]`, "[]")

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
				PreConfig: func() { fake.markPolicyDeleted(t, firstID) },
				Config:    config,
				Check: func(s *terraform.State) error {
					if id := s.RootModule().Resources[resourceName].Primary.ID; id == firstID {
						return fmt.Errorf("expected a new policy id after out-of-band delete, still %s", firstID)
					}
					return nil
				},
			},
		},
	})
}

func TestDlpMcpPolicyResource_serverAlreadyAssignedIs409(t *testing.T) {
	setupDlpMcpTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: dlpMcpPolicyConfig("first", `["github"]`, "[]") + `
resource "barndoor_dlp_mcp_policy" "second" {
  name           = "second"
  mcp_server_ids = ["github", "jira"]
  rules          = []
  depends_on     = [barndoor_dlp_mcp_policy.test]
}
`,
				ExpectError: regexp.MustCompile(`(?s)MCP server already belongs to another policy.*'first'`),
			},
		},
	})
}

func TestDlpMcpPolicyResource_legacyOrgIs409(t *testing.T) {
	fake := setupDlpMcpTest(t)
	fake.setPolicyModel("LEGACY")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      dlpMcpPolicyConfig("tf-mcp-legacy", `["github"]`, "[]"),
				ExpectError: regexp.MustCompile(`(?s)Organization is not on MCP server policies.*barndoor_dlp_enforcement_policy`),
			},
		},
	})
}

func TestDlpMcpPolicyResource_unknownEngineIs404(t *testing.T) {
	setupDlpMcpTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: dlpMcpPolicyConfig("tf-mcp-bad-engine", `["github"]`, `[{
      runtime_boundary     = "BOTH"
      action               = "POLICY_ACTION_BLOCK"
      detection_engine_ids = ["dddd0000-0000-0000-0000-000000000099"]
    }]`),
				ExpectError: regexp.MustCompile(`(?s)not found in org.*detection_engine_ids`),
			},
		},
	})
}

func TestDlpMcpPolicyResource_importingBaselineFails(t *testing.T) {
	fake := setupDlpMcpTest(t)
	fake.seedBaseline()

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:        dlpMcpPolicyConfig("anything", `["github"]`, "[]"),
				ResourceName:  "barndoor_dlp_mcp_policy.test",
				ImportState:   true,
				ImportStateId: fakeMcpBaselineID,
				ExpectError:   regexp.MustCompile(`barndoor_dlp_mcp_baseline_policy`),
			},
		},
	})
}

func TestDlpMcpPolicyResource_planTimeValidation(t *testing.T) {
	setupDlpMcpTest(t)

	for name, tc := range map[string]struct {
		name, servers, rule string
		want                string
	}{
		"ALL with agent ids": {
			rule: fmt.Sprintf(`agent_scope = { mode = "ALL", agent_ids = [%q] }`, testMcpAgentA),
			want: "must be omitted when `agent_scope.mode` is `ALL`",
		},
		"ONLY without agents": {
			rule: `agent_scope = { mode = "ONLY" }`,
			want: "`agent_scope.agent_ids` must list at least one entry",
		},
		"EXCEPT without principals": {
			rule: `principal_scope = { mode = "EXCEPT" }`,
			want: "`principal_scope.principals` must list at least one entry",
		},
		"mask_format without MASK": {
			rule: `mask_format = { keep = "PREFIX", keep_chars = 2 }`,
			want: "only valid with",
		},
		"uppercase agent id": {
			rule: fmt.Sprintf(`agent_scope = { mode = "ONLY", agent_ids = [%q] }`, strings.ToUpper(testMcpAgentA)),
			want: "must be a lowercase UUID",
		},
		"reserved name": {
			name: "baseline POLICY",
			want: "reserved for the Baseline policy",
		},
		"wildcard server": {
			servers: `["*"]`,
			want:    `value must be none of`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.name == "" {
				tc.name = "tf-mcp-invalid"
			}
			if tc.servers == "" {
				tc.servers = `["github"]`
			}
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{
					{
						Config: dlpMcpPolicyConfig(tc.name, tc.servers,
							fmt.Sprintf("[%s]", testMcpRuleHCL("BOTH", "POLICY_ACTION_BLOCK", "      "+tc.rule))),
						PlanOnly:    true,
						ExpectError: regexp.MustCompile(regexp.QuoteMeta(tc.want)),
					},
				},
			})
		})
	}
}
