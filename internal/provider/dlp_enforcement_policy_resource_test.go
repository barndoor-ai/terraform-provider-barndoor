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

func TestDlpEnforcementPolicyResource_Metadata(t *testing.T) {
	var resp frameworkresource.MetadataResponse
	NewDlpEnforcementPolicyResource().Metadata(context.Background(),
		frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_dlp_enforcement_policy"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestDlpEnforcementPolicyResource_Schema(t *testing.T) {
	var resp frameworkresource.SchemaResponse
	NewDlpEnforcementPolicyResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{
		"id", "org_id", "name", "target_kind", "provider_ids", "model_alias", "runtime_stage",
		"action", "priority", "dry_run", "mcp_targets", "principals", "detection_engine_ids",
		"created_by", "updated_by", "created_at", "updated_at",
	} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
	for _, required := range []string{"name", "action", "detection_engine_ids"} {
		if !resp.Schema.Attributes[required].IsRequired() {
			t.Errorf("%s should be Required", required)
		}
	}
	for _, computed := range []string{"id", "org_id", "target_kind", "runtime_stage", "priority", "created_at", "updated_at"} {
		if !resp.Schema.Attributes[computed].IsComputed() {
			t.Errorf("%s should be Computed", computed)
		}
	}
}

// --- conversion tests ------------------------------------------------------------

func TestBuildDlpEnforcementPolicyUpdateRequest_ExplicitClears(t *testing.T) {
	// Convergence contract: the update body always carries the collections
	// (as [] when unset) and model_alias (as "" when unset), so removing them
	// from configuration clears them server-side under the API's
	// presence-based partial-update semantics.
	plan := &dlpEnforcementPolicyResourceModel{
		ID:                 types.StringValue("eeee0000-0000-0000-0000-000000000001"),
		Name:               types.StringValue("Policy"),
		TargetKind:         types.StringValue("MCP_SERVER"),
		RuntimeStage:       types.StringValue("RUNTIME_STAGE_UNSPECIFIED"),
		Action:             types.StringValue("POLICY_ACTION_BLOCK"),
		Priority:           types.Int64Value(0),
		DryRun:             types.BoolValue(false),
		DetectionEngineIDs: mustStringList(t, fakeDlpDetectionEngineID),
	}
	body, err := buildDlpEnforcementPolicyUpdateRequest(context.Background(), plan)
	if err != nil {
		t.Fatalf("buildDlpEnforcementPolicyUpdateRequest: %v", err)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for key, want := range map[string]string{
		"provider_ids": "[]",
		"mcp_targets":  "[]",
		"principals":   "[]",
		"model_alias":  `""`,
	} {
		v, ok := keys[key]
		if !ok {
			t.Errorf("%s must be serialized even when unset (the API keeps the old value otherwise)", key)
			continue
		}
		if string(v) != want {
			t.Errorf("%s = %s, want %s", key, v, want)
		}
	}
	if v, ok := keys["detection_engine_ids"]; !ok || string(v) != fmt.Sprintf(`["%s"]`, fakeDlpDetectionEngineID) {
		t.Errorf("detection_engine_ids = %s, want the configured engine", v)
	}
}

func TestApplyDlpEnforcementPolicyResponse_SettlesEmptyToNull(t *testing.T) {
	prior := &dlpEnforcementPolicyResourceModel{
		ProviderIDs:        types.ListNull(types.StringType),
		ModelAlias:         types.StringNull(),
		DetectionEngineIDs: mustStringList(t, fakeDlpDetectionEngineID),
	}
	policy := &dlpEnforcementPolicyResponse{
		ID:                 "eeee0000-0000-0000-0000-000000000001",
		OrgID:              fakeDlpOrgID,
		Name:               "Policy",
		TargetKind:         "MCP_SERVER",
		ProviderIDs:        []string{},
		ModelAlias:         nil,
		RuntimeStage:       "RUNTIME_STAGE_UNSPECIFIED",
		Action:             "POLICY_ACTION_BLOCK",
		McpTargets:         []dlpMcpTargetPayload{},
		Principals:         []dlpPrincipalPayload{},
		DetectionEngineIDs: []string{fakeDlpDetectionEngineID},
	}

	state, err := applyDlpEnforcementPolicyResponse(context.Background(), policy, prior)
	if err != nil {
		t.Fatalf("applyDlpEnforcementPolicyResponse: %v", err)
	}

	if !state.ProviderIDs.IsNull() {
		t.Error("empty provider_ids with a null prior must settle to null, not []")
	}
	if !state.ModelAlias.IsNull() {
		t.Error("absent model_alias must settle to null")
	}
	if state.McpTargets != nil {
		t.Error("empty mcp_targets with a nil prior must settle to nil (null set)")
	}
	if state.Principals != nil {
		t.Error("empty principals with a nil prior must settle to nil (null set)")
	}
	if state.DetectionEngineIDs.IsNull() {
		t.Error("detection_engine_ids must map from the response")
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

func dlpPolicyConfig(name, extra string) string {
	return fmt.Sprintf(`
resource "barndoor_dlp_enforcement_policy" "test" {
  name                 = %q
  action               = "POLICY_ACTION_TOKENIZE"
  detection_engine_ids = [%q]
%s
}
`, name, fakeDlpDetectionEngineID, extra)
}

func TestDlpEnforcementPolicyResource_mcpServerLifecycle(t *testing.T) {
	fake := setupDlpTest(t)
	const resourceName = "barndoor_dlp_enforcement_policy.test"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpPoliciesDeleted(fake),
		Steps: []resource.TestStep{
			{
				// Minimal MCP-lane policy: target_kind, runtime_stage,
				// priority, and dry_run all come from the server.
				Config: dlpPolicyConfig("tf-dlp-policy", `
  mcp_targets = [
    { mcp_server_id = "*", direction = "BOTH" },
  ]
`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckResourceAttr(resourceName, "org_id", "org-123"),
					resource.TestCheckResourceAttr(resourceName, "target_kind", "MCP_SERVER"),
					resource.TestCheckResourceAttr(resourceName, "runtime_stage", "RUNTIME_STAGE_UNSPECIFIED"),
					resource.TestCheckResourceAttr(resourceName, "priority", "0"),
					resource.TestCheckResourceAttr(resourceName, "dry_run", "false"),
					resource.TestCheckResourceAttr(resourceName, "mcp_targets.#", "1"),
					resource.TestCheckNoResourceAttr(resourceName, "principals.#"),
					resource.TestCheckNoResourceAttr(resourceName, "provider_ids.#"),
				),
			},
			{
				// A second plan over unchanged configuration must be empty.
				Config: dlpPolicyConfig("tf-dlp-policy", `
  mcp_targets = [
    { mcp_server_id = "*", direction = "BOTH" },
  ]
`),
				PlanOnly: true,
			},
			{
				// In-place update: rename, change the action, add principals,
				// flip dry_run.
				Config: fmt.Sprintf(`
resource "barndoor_dlp_enforcement_policy" "test" {
  name                 = "tf-dlp-policy-renamed"
  action               = "POLICY_ACTION_REDACT"
  dry_run              = true
  detection_engine_ids = [%q]
  mcp_targets = [
    { mcp_server_id = "*", direction = "BOTH" },
  ]
  principals = [
    { principal_type = "GROUP", principal_id = "engineering" },
  ]
}
`, fakeDlpDetectionEngineID),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "name", "tf-dlp-policy-renamed"),
					resource.TestCheckResourceAttr(resourceName, "action", "POLICY_ACTION_REDACT"),
					resource.TestCheckResourceAttr(resourceName, "dry_run", "true"),
					resource.TestCheckResourceAttr(resourceName, "principals.#", "1"),
				),
			},
			{
				// Dropping principals must clear them server-side and settle
				// state back to null — no perpetual diff.
				Config: fmt.Sprintf(`
resource "barndoor_dlp_enforcement_policy" "test" {
  name                 = "tf-dlp-policy-renamed"
  action               = "POLICY_ACTION_REDACT"
  dry_run              = true
  detection_engine_ids = [%q]
  mcp_targets = [
    { mcp_server_id = "*", direction = "BOTH" },
  ]
}
`, fakeDlpDetectionEngineID),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(resourceName, "principals.#"),
				),
			},
			{
				Config: fmt.Sprintf(`
resource "barndoor_dlp_enforcement_policy" "test" {
  name                 = "tf-dlp-policy-renamed"
  action               = "POLICY_ACTION_REDACT"
  dry_run              = true
  detection_engine_ids = [%q]
  mcp_targets = [
    { mcp_server_id = "*", direction = "BOTH" },
  ]
}
`, fakeDlpDetectionEngineID),
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

func TestDlpEnforcementPolicyResource_modelProviderLifecycle(t *testing.T) {
	fake := setupDlpTest(t)
	const resourceName = "barndoor_dlp_enforcement_policy.test"

	modelProviderConfig := func(modelAlias string) string {
		return fmt.Sprintf(`
resource "barndoor_dlp_enforcement_policy" "test" {
  name                 = "tf-dlp-llm-policy"
  action               = "POLICY_ACTION_BLOCK"
  provider_ids         = ["openai"]
%s  runtime_stage        = "RUNTIME_STAGE_PROMPT"
  detection_engine_ids = [%q]
}
`, modelAlias, fakeDlpDetectionEngineID)
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpPoliciesDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: modelProviderConfig("  model_alias          = \"gpt-4o\"\n"),
				Check: resource.ComposeAggregateTestCheckFunc(
					// The API infers the lane from the scope attributes.
					resource.TestCheckResourceAttr(resourceName, "target_kind", "MODEL_PROVIDER"),
					resource.TestCheckResourceAttr(resourceName, "model_alias", "gpt-4o"),
					resource.TestCheckResourceAttr(resourceName, "provider_ids.0", "openai"),
					resource.TestCheckResourceAttr(resourceName, "runtime_stage", "RUNTIME_STAGE_PROMPT"),
				),
			},
			{
				Config:   modelProviderConfig("  model_alias          = \"gpt-4o\"\n"),
				PlanOnly: true,
			},
			{
				// Removing model_alias must clear it server-side (the update
				// body sends "") and settle state back to null.
				Config: modelProviderConfig(""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(resourceName, "model_alias"),
				),
			},
			{
				Config:   modelProviderConfig(""),
				PlanOnly: true,
			},
		},
	})
}

func TestDlpEnforcementPolicyResource_outOfBandDeletePlansRecreate(t *testing.T) {
	fake := setupDlpTest(t)
	const resourceName = "barndoor_dlp_enforcement_policy.test"

	var firstID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: dlpPolicyConfig("tf-dlp-oob", ""),
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
				PreConfig: func() { fake.markPolicyDeleted(t, firstID) },
				Config:    dlpPolicyConfig("tf-dlp-oob", ""),
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

func TestDlpEnforcementPolicyResource_unknownEngineIs404(t *testing.T) {
	setupDlpTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "barndoor_dlp_enforcement_policy" "test" {
  name                 = "tf-dlp-bad-engine"
  action               = "POLICY_ACTION_BLOCK"
  detection_engine_ids = ["dddd0000-0000-0000-0000-000000000099"]
}
`,
				ExpectError: regexp.MustCompile(`(?s)not found in org.*detection_engine_ids`),
			},
		},
	})
}

func TestDlpEnforcementPolicyResource_duplicateNameIs409(t *testing.T) {
	setupDlpTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: dlpPolicyConfig("dupe-policy", "") + fmt.Sprintf(`
resource "barndoor_dlp_enforcement_policy" "dupe" {
  name                 = "dupe-policy"
  action               = "POLICY_ACTION_TOKENIZE"
  detection_engine_ids = [%q]
  depends_on           = [barndoor_dlp_enforcement_policy.test]
}
`, fakeDlpDetectionEngineID),
				ExpectError: regexp.MustCompile(`already exists`),
			},
		},
	})
}

func TestDlpEnforcementPolicyResource_mixedTargetShapeIs422(t *testing.T) {
	setupDlpTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// MODEL_PROVIDER policies may not carry mcp_targets; the
				// API's 422 message must surface verbatim.
				Config: fmt.Sprintf(`
resource "barndoor_dlp_enforcement_policy" "test" {
  name                 = "tf-dlp-mixed"
  target_kind          = "MODEL_PROVIDER"
  action               = "POLICY_ACTION_BLOCK"
  detection_engine_ids = [%q]
  mcp_targets = [
    { mcp_server_id = "*", direction = "BOTH" },
  ]
}
`, fakeDlpDetectionEngineID),
				ExpectError: regexp.MustCompile(`MODEL_PROVIDER policies may not include mcp_targets`),
			},
		},
	})
}
