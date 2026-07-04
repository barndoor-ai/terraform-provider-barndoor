// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework-jsontypes/jsontypes"
	frameworkresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// --- schema tests --------------------------------------------------------------

func TestDlpFieldControlPolicyResource_Metadata(t *testing.T) {
	var resp frameworkresource.MetadataResponse
	NewDlpFieldControlPolicyResource().Metadata(context.Background(),
		frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_dlp_field_control_policy"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestDlpFieldControlPolicyResource_Schema(t *testing.T) {
	var resp frameworkresource.SchemaResponse
	NewDlpFieldControlPolicyResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{
		"id", "org_id", "mcp_server_id", "name", "enabled", "rules", "version",
		"created_by", "updated_by", "created_at", "updated_at",
	} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
	for _, required := range []string{"mcp_server_id", "name"} {
		if !resp.Schema.Attributes[required].IsRequired() {
			t.Errorf("%s should be Required", required)
		}
	}
	for _, computed := range []string{"id", "org_id", "enabled", "version", "created_at", "updated_at"} {
		if !resp.Schema.Attributes[computed].IsComputed() {
			t.Errorf("%s should be Computed", computed)
		}
	}
}

// --- conversion tests ------------------------------------------------------------

func TestBuildDlpFieldControlPolicyUpdateRequest_ExplicitClears(t *testing.T) {
	// Convergence contract: the update body always carries rules (as [] when
	// unset), so removing them from configuration clears them server-side
	// under the API's presence-based partial-update semantics.
	plan := &dlpFieldControlPolicyResourceModel{
		Name:    types.StringValue("Policy"),
		Enabled: types.BoolValue(true),
		Rules:   jsontypes.NewNormalizedNull(),
	}
	body, err := buildDlpFieldControlPolicyUpdateRequest(plan)
	if err != nil {
		t.Fatalf("buildDlpFieldControlPolicyUpdateRequest: %v", err)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if v, ok := keys["rules"]; !ok || string(v) != "[]" {
		t.Errorf("rules = %s, want [] (the API keeps the old rules when the key is absent)", v)
	}
	for _, key := range []string{"name", "enabled"} {
		if _, ok := keys[key]; !ok {
			t.Errorf("%s must be serialized on every update", key)
		}
	}
	// The API rejects unknown keys (deny_unknown_fields); the body must carry
	// exactly the documented set.
	if len(keys) != 3 {
		t.Errorf("update body carries %d keys (%v), want exactly name/enabled/rules", len(keys), keys)
	}
}

func TestApplyDlpFieldControlPolicyResponse_SettlesEmptyRulesToNull(t *testing.T) {
	prior := &dlpFieldControlPolicyResourceModel{Rules: jsontypes.NewNormalizedNull()}
	policy := &dlpFieldControlPolicyResponse{
		ID:          "abab0000-0000-0000-0000-000000000001",
		OrgID:       fakeDlpOrgID,
		McpServerID: "srv-1",
		Name:        "Policy",
		Enabled:     true,
		Version:     1,
		Rules:       json.RawMessage("[]"),
	}

	state := applyDlpFieldControlPolicyResponse(policy, prior)

	if !state.Rules.IsNull() {
		t.Error("empty rules with a null prior must settle to null, not []")
	}
	if got, want := state.Version.ValueInt64(), int64(1); got != want {
		t.Errorf("version = %d, want %d", got, want)
	}

	// With a configured prior, the same empty array must stay [] so an
	// explicit jsonencode([]) does not drift.
	prior.Rules = jsontypes.NewNormalizedValue("[]")
	state = applyDlpFieldControlPolicyResponse(policy, prior)
	if state.Rules.IsNull() {
		t.Error("empty rules with a configured [] prior must stay [], not settle to null")
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

// fieldPolicyConfig renders a barndoor_dlp_field_control_policy block. extra
// is appended verbatim inside the block.
func fieldPolicyConfig(serverID, name, extra string) string {
	return fmt.Sprintf(`
resource "barndoor_dlp_field_control_policy" "test" {
  mcp_server_id = %q
  name          = %q
%s
}
`, serverID, name, extra)
}

const fieldPolicyRules = `
  rules = jsonencode([
    {
      tool      = "search_contacts"
      direction = "output"
      fields = [
        { path = "records.email", action = "redact" },
      ]
    },
  ])
`

func TestDlpFieldControlPolicyResource_lifecycle(t *testing.T) {
	fake := setupDlpTest(t)
	const resourceName = "barndoor_dlp_field_control_policy.test"

	var firstID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpFieldControlPoliciesDeleted(fake),
		Steps: []resource.TestStep{
			{
				// Minimal policy: enabled defaults to true, version starts at 1.
				Config: fieldPolicyConfig("srv-crm", "tf-dlp-field-policy", fieldPolicyRules),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckResourceAttr(resourceName, "org_id", fakeDlpOrgID),
					resource.TestCheckResourceAttr(resourceName, "mcp_server_id", "srv-crm"),
					resource.TestCheckResourceAttr(resourceName, "enabled", "true"),
					resource.TestCheckResourceAttr(resourceName, "version", "1"),
					func(s *terraform.State) error {
						rs, ok := s.RootModule().Resources[resourceName]
						if !ok {
							return fmt.Errorf("%s not in state", resourceName)
						}
						firstID = rs.Primary.ID
						return nil
					},
				),
			},
			{
				// A second plan over unchanged configuration must be empty.
				Config:   fieldPolicyConfig("srv-crm", "tf-dlp-field-policy", fieldPolicyRules),
				PlanOnly: true,
			},
			{
				// In-place update: rename, disable, change the rules; the
				// server bumps version.
				Config: fieldPolicyConfig("srv-crm", "tf-dlp-field-policy-renamed", `
  enabled = false
  rules = jsonencode([
    {
      tool      = "search_contacts"
      direction = "output"
      fields = [
        { path = "records.email", action = "block" },
      ]
      default_action = "pass"
    },
  ])
`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "name", "tf-dlp-field-policy-renamed"),
					resource.TestCheckResourceAttr(resourceName, "enabled", "false"),
					resource.TestCheckResourceAttr(resourceName, "version", "2"),
				),
			},
			{
				// Dropping rules must clear them server-side (the update body
				// sends []) and settle state back to null — no perpetual diff.
				Config: fieldPolicyConfig("srv-crm", "tf-dlp-field-policy-renamed", "  enabled = false"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(resourceName, "rules"),
					resource.TestCheckResourceAttr(resourceName, "version", "3"),
				),
			},
			{
				Config:   fieldPolicyConfig("srv-crm", "tf-dlp-field-policy-renamed", "  enabled = false"),
				PlanOnly: true,
			},
			{
				// Changing mcp_server_id forces a replacement (the API's PUT
				// cannot move a policy between servers).
				Config: fieldPolicyConfig("srv-erp", "tf-dlp-field-policy-renamed", "  enabled = false"),
				Check: func(s *terraform.State) error {
					rs, ok := s.RootModule().Resources[resourceName]
					if !ok {
						return fmt.Errorf("%s not in state", resourceName)
					}
					if rs.Primary.ID == firstID {
						return fmt.Errorf("expected a new policy id after changing mcp_server_id, still %s", firstID)
					}
					return nil
				},
			},
			{
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestDlpFieldControlPolicyResource_existingPolicyBlocksCreate(t *testing.T) {
	fake := setupDlpTest(t)
	fake.seedFieldControlPolicy("srv-crm", "made out of band")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// The API's POST would silently adopt (and overwrite) the
				// existing policy; Create must refuse and direct to import.
				Config:      fieldPolicyConfig("srv-crm", "tf-dlp-field-policy", ""),
				ExpectError: regexp.MustCompile(`(?s)already has field control policy.*terraform import`),
			},
		},
	})
}

func TestDlpFieldControlPolicyResource_outOfBandDeletePlansRecreate(t *testing.T) {
	fake := setupDlpTest(t)
	const resourceName = "barndoor_dlp_field_control_policy.test"

	var firstID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fieldPolicyConfig("srv-crm", "tf-dlp-oob", ""),
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
				PreConfig: func() { fake.markFieldControlPolicyDeleted(t, firstID) },
				Config:    fieldPolicyConfig("srv-crm", "tf-dlp-oob", ""),
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

func TestDlpFieldControlPolicyResource_nonArrayRulesFailAtPlanTime(t *testing.T) {
	setupDlpTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// A JSON object is valid JSON but not a rules array; the
				// provider must reject it before any API call.
				Config:      fieldPolicyConfig("srv-crm", "tf-dlp-bad-rules", "  rules = jsonencode({})"),
				ExpectError: regexp.MustCompile(`must be a JSON array`),
			},
		},
	})
}

func TestDlpFieldControlPolicyResource_malformedRuleIs409(t *testing.T) {
	setupDlpTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Rule contents are validated server-side (and answered with a
				// 409, not a 422); the message must surface verbatim.
				Config: fieldPolicyConfig("srv-crm", "tf-dlp-bad-rule", `
  rules = jsonencode([
    { direction = "output" },
  ])
`),
				// \s+ because the CLI hard-wraps the diagnostic text.
				ExpectError: regexp.MustCompile(`field control rule 0 must\s+specify\s+tool`),
			},
		},
	})
}
