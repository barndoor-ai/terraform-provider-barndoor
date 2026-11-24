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
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
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
	for _, attr := range []string{"id", "name", "mcp_server_ids", "rules", "status", "version", "created_by", "updated_by", "created_at", "updated_at"} {
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
	// B keeps its id by content wherever it moved. The edited A (index 1) has
	// no unclaimed stored rule at its index, so it is created. The new rule
	// sits at index 2, where C was dropped, and takes over C's id.
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

func TestAssignDlpMcpRuleIDs_PositionFallback(t *testing.T) {
	a := testMcpRule("TOOL_INPUT", "POLICY_ACTION_BLOCK")
	b := testMcpRule("TOOL_OUTPUT", "POLICY_ACTION_REDACT")
	c := testMcpRule("BOTH", "POLICY_ACTION_ALERT_ONLY")
	edit := func(r dlpMcpRulePayload) dlpMcpRulePayload { r.DryRun = true; return r }
	fresh := testMcpRule("TOOL_INPUT", "POLICY_ACTION_OMIT")

	for name, tc := range map[string]struct {
		planned []dlpMcpRulePayload
		want    []string
	}{
		"delete first and append: appended rule gets no id": {
			planned: []dlpMcpRulePayload{b, c, fresh},
			want:    []string{"id-b", "id-c", "<new>"},
		},
		"reorder and edit one: edited rule moved, no id": {
			planned: []dlpMcpRulePayload{c, edit(a), b},
			want:    []string{"id-c", "<new>", "id-b"},
		},
		"insert in the middle: no id stolen": {
			planned: []dlpMcpRulePayload{a, fresh, c, b},
			want:    []string{"id-a", "<new>", "id-c", "id-b"},
		},
		"swap and edit one: edited rule moved, no id": {
			planned: []dlpMcpRulePayload{edit(b), a, c},
			want:    []string{"<new>", "id-a", "id-c"},
		},
		"edits in place: paired": {
			planned: []dlpMcpRulePayload{edit(a), edit(b), c},
			want:    []string{"id-a", "id-b", "id-c"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			storedA, storedB, storedC := a, b, c
			storedA.ID, storedB.ID, storedC.ID = strPtr("id-a"), strPtr("id-b"), strPtr("id-c")
			planned := slices.Clone(tc.planned)

			assignDlpMcpRuleIDs(planned, []dlpMcpRulePayload{storedA, storedB, storedC})

			got := []string{}
			for _, r := range planned {
				if r.ID == nil {
					got = append(got, "<new>")
				} else {
					got = append(got, *r.ID)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("assigned ids = %v, want %v", got, tc.want)
			}
		})
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
		"policy archived": {
			status: http.StatusConflict,
			body:   `{"message":"This policy is archived.","code":"policy_archived"}`,
			want:   "MCP policy is archived",
		},
		"policies v2 not enabled": {
			status: http.StatusConflict,
			body:   `{"message":"Not available.","code":"policies_v2_not_enabled"}`,
			want:   "Policy lifecycle is not enabled for this organization",
		},
		"invalid status transition": {
			status: http.StatusConflict,
			body:   `{"message":"No.","code":"invalid_status_transition"}`,
			want:   "Status change not allowed",
		},
		"invalid policy rules": {
			status: http.StatusConflict,
			body:   `{"message":"Rule 1 is invalid.","code":"invalid_policy_rules"}`,
			want:   "The policy's stored rules are no longer valid",
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
		"padded label": {
			rule: `label = " padded "`,
			want: "rules[0].label must not be empty or have",
		},
		"blank label": {
			rule: `label = "   "`,
			want: "rules[0].label must not be empty or have",
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

func TestDlpMcpPolicyResource_statusAndVersion(t *testing.T) {
	fake := setupDlpMcpTest(t)
	const resourceName = "barndoor_dlp_mcp_policy.test"
	config := dlpMcpPolicyConfig("tf-mcp-version", `["github"]`, "[]")

	var policyID string
	var putsAfterCreate int
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpMcpPoliciesDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "status", "ACTIVE"),
					resource.TestCheckResourceAttr(resourceName, "version", "1"),
					func(s *terraform.State) error {
						policyID = s.RootModule().Resources[resourceName].Primary.ID
						putsAfterCreate = fake.putCount()
						return nil
					},
				),
			},
			{
				// Nothing changed: no PUT, and the revision number stays.
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "version", "1"),
					func(*terraform.State) error {
						if n := fake.putCount(); n != putsAfterCreate {
							return fmt.Errorf("an unchanged apply sent %d PUTs, want %d", n, putsAfterCreate)
						}
						if v := fake.policyVersion(policyID); v != 1 {
							return fmt.Errorf("stored version = %d after an unchanged apply, want 1", v)
						}
						return nil
					},
				),
			},
			{
				Config: dlpMcpPolicyConfig("tf-mcp-version", `["github", "jira"]`, "[]"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "status", "ACTIVE"),
					resource.TestCheckResourceAttr(resourceName, "version", "2"),
				),
			},
		},
	})
}

func TestDlpMcpPolicyResource_archivedOutsideTerraformIsRecreated(t *testing.T) {
	for name, policiesV2 := range map[string]bool{
		"lifecycle flag off (archived reads 404)": false,
		"lifecycle flag on (archived reads 200)":  true,
	} {
		t.Run(name, func(t *testing.T) {
			fake := setupDlpMcpTest(t)
			fake.setPoliciesV2(policiesV2)
			const resourceName = "barndoor_dlp_mcp_policy.test"
			config := dlpMcpPolicyConfig("tf-mcp-archived", `["github"]`, "[]")

			var firstID string
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				CheckDestroy:             checkAllDlpMcpPoliciesDeleted(fake),
				Steps: []resource.TestStep{
					{
						Config: config,
						Check: func(s *terraform.State) error {
							firstID = s.RootModule().Resources[resourceName].Primary.ID
							return nil
						},
					},
					{
						PreConfig: func() { fake.markPolicyArchived(t, firstID) },
						Config:    config,
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr(resourceName, "status", "ACTIVE"),
							func(s *terraform.State) error {
								if id := s.RootModule().Resources[resourceName].Primary.ID; id == firstID {
									return fmt.Errorf("expected a new policy after the old one was archived, still %s", firstID)
								}
								return nil
							},
						),
					},
				},
			})
		})
	}
}

// Destroying archives the policy; its name and servers must be free to use
// again immediately.
func TestDlpMcpPolicyResource_destroyThenCreateSameNameAndServers(t *testing.T) {
	fake := setupDlpMcpTest(t)
	config := dlpMcpPolicyConfig("tf-mcp-reuse", `["github", "jira"]`, "[]")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpMcpPoliciesDeleted(fake),
		Steps:                    []resource.TestStep{{Config: config}},
	})
	if n := fake.archivedCount(); n != 1 {
		t.Fatalf("%d archived policies after destroy, want 1 (destroy should archive)", n)
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpMcpPoliciesDeleted(fake),
		Steps: []resource.TestStep{{
			Config: config,
			Check:  resource.TestCheckResourceAttr("barndoor_dlp_mcp_policy.test", "name", "tf-mcp-reuse"),
		}},
	})
	if n := fake.archivedCount(); n != 2 {
		t.Fatalf("%d archived policies after the second destroy, want 2", n)
	}
}

// beforeApplyCheck is a plan check that runs after the plan and before the
// apply, so the change under test reaches Update instead of Read.
type beforeApplyCheck func()

func (c beforeApplyCheck) CheckPlan(context.Context, plancheck.CheckPlanRequest, *plancheck.CheckPlanResponse) {
	c()
}

func TestDlpMcpPolicyResource_updateOfPolicyChangedAfterPlan(t *testing.T) {
	const resourceName = "barndoor_dlp_mcp_policy.test"
	initial := dlpMcpPolicyConfig("tf-mcp-race", `["github"]`, "[]")
	changed := dlpMcpPolicyConfig("tf-mcp-race", `["github", "jira"]`, "[]")

	for name, tc := range map[string]struct {
		policiesV2 bool
		disturb    func(t *testing.T, fake *fakeDlpMcpServer, id string)
		want       string
	}{
		"archived, flag off (404)": {
			disturb: func(t *testing.T, f *fakeDlpMcpServer, id string) { f.markPolicyArchived(t, id) },
			want:    "MCP policy no longer exists",
		},
		"archived, flag on (200 ARCHIVED)": {
			policiesV2: true,
			disturb:    func(t *testing.T, f *fakeDlpMcpServer, id string) { f.markPolicyArchived(t, id) },
			want:       "MCP policy no longer exists",
		},
		"deactivated (unmanaged status)": {
			policiesV2: true,
			disturb:    func(t *testing.T, f *fakeDlpMcpServer, id string) { f.setPolicyStatus(t, id, "INACTIVE") },
			want:       "MCP policy status changed outside Terraform",
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake := setupDlpMcpTest(t)
			fake.setPoliciesV2(tc.policiesV2)

			var id string
			var puts int
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{
					{
						Config: initial,
						Check: func(s *terraform.State) error {
							id = s.RootModule().Resources[resourceName].Primary.ID
							puts = fake.putCount()
							return nil
						},
					},
					{
						Config: changed,
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{
								beforeApplyCheck(func() { tc.disturb(t, fake, id) }),
							},
						},
						ExpectError: regexp.MustCompile(tc.want),
					},
					{
						// The failed apply must not have sent a PUT.
						PreConfig: func() {
							if n := fake.putCount(); n != puts {
								t.Errorf("%d PUTs sent despite the error, want %d", n, puts)
							}
							fake.setPolicyStatus(t, id, "ACTIVE")
						},
						Config:  initial,
						Destroy: true,
					},
				},
			})
		})
	}
}

func TestWarnDlpMcpPolicyInactive(t *testing.T) {
	for status, want := range map[string]int{"ACTIVE": 0, "DRAFT": 1, "INACTIVE": 1} {
		t.Run(status, func(t *testing.T) {
			var diags diag.Diagnostics
			warnDlpMcpPolicyInactive(&diags, &dlpMcpPolicyResponse{Name: "p", Status: status})
			if got := len(diags.Warnings()); got != want || diags.HasError() {
				t.Errorf("status %s: %d warnings (want %d), diagnostics %+v", status, got, want, diags)
			}
		})
	}
}

// State written before `status` existed holds null for it. The plan must leave
// it unknown then, not copy the null, or apply fails with "was null, but now".
func TestDlpMcpStatusPlanModifierIgnoresNullState(t *testing.T) {
	for name, newResource := range map[string]func() frameworkresource.Resource{
		"policy":   NewDlpMcpPolicyResource,
		"baseline": NewDlpMcpBaselinePolicyResource,
	} {
		t.Run(name, func(t *testing.T) {
			var resp frameworkresource.SchemaResponse
			newResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
			attr, ok := resp.Schema.Attributes["status"].(schema.StringAttribute)
			if !ok {
				t.Fatal("status is not a string attribute")
			}
			for _, tc := range []struct {
				state, want types.String
			}{
				{types.StringNull(), types.StringUnknown()},
				{types.StringValue("ACTIVE"), types.StringValue("ACTIVE")},
			} {
				req := planmodifier.StringRequest{
					StateValue:  tc.state,
					PlanValue:   types.StringUnknown(),
					ConfigValue: types.StringNull(),
					Plan:        tfsdk.Plan{Raw: tftypes.NewValue(tftypes.Object{}, nil)},
				}
				mresp := &planmodifier.StringResponse{PlanValue: types.StringUnknown()}
				for _, m := range attr.PlanModifiers {
					m.PlanModifyString(context.Background(), req, mresp)
				}
				if !mresp.PlanValue.Equal(tc.want) {
					t.Errorf("state %v: planned %v, want %v", tc.state, mresp.PlanValue, tc.want)
				}
			}
		})
	}
}
