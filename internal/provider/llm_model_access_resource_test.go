// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	frameworkresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// --- schema tests --------------------------------------------------------------

func TestLlmModelAccessResource_Metadata(t *testing.T) {
	var resp frameworkresource.MetadataResponse
	NewLlmModelAccessResource().Metadata(context.Background(),
		frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_llm_model_access"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestLlmModelAccessResource_Schema(t *testing.T) {
	var resp frameworkresource.SchemaResponse
	NewLlmModelAccessResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{
		"id", "org_id", "name", "scope_type", "scope_id", "scope_value",
		"policy_type", "targets", "traffic_type", "enabled",
	} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
	for _, required := range []string{"name", "scope_type", "policy_type", "targets"} {
		if !resp.Schema.Attributes[required].IsRequired() {
			t.Errorf("%s should be Required", required)
		}
	}
	for _, computed := range []string{"id", "org_id", "traffic_type", "enabled"} {
		if !resp.Schema.Attributes[computed].IsComputed() {
			t.Errorf("%s should be Computed", computed)
		}
	}
}

// --- conversion tests ------------------------------------------------------------

func TestBuildLlmModelAccessTargets_RejectsMismatchedShapes(t *testing.T) {
	cases := []struct {
		name   string
		target llmModelAccessTargetModel
	}{
		{"model_alias without alias", llmModelAccessTargetModel{
			Kind: types.StringValue("model_alias"),
		}},
		{"model_alias with stray provider_id", llmModelAccessTargetModel{
			Kind:       types.StringValue("model_alias"),
			Alias:      types.StringValue("gpt-*"),
			ProviderID: types.StringValue("aaaa0000-0000-0000-0000-000000000001"),
		}},
		{"provider without provider_id", llmModelAccessTargetModel{
			Kind:  types.StringValue("provider"),
			Model: types.StringValue("gpt-4o"),
		}},
		{"provider_model without model", llmModelAccessTargetModel{
			Kind:       types.StringValue("provider_model"),
			ProviderID: types.StringValue("aaaa0000-0000-0000-0000-000000000001"),
		}},
		{"route_group without group_id", llmModelAccessTargetModel{
			Kind: types.StringValue("route_group"),
		}},
		{"model with stray group_id", llmModelAccessTargetModel{
			Kind:    types.StringValue("model"),
			Model:   types.StringValue("gpt-4o"),
			GroupID: types.StringValue("abab0000-0000-0000-0000-000000000001"),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			buildLlmModelAccessTargets([]llmModelAccessTargetModel{tc.target}, &diags)
			if !diags.HasError() {
				t.Errorf("expected a shape diagnostic for %s", tc.name)
			}
		})
	}
}

func TestBuildLlmModelAccessTargets_EmitsTaggedWireShape(t *testing.T) {
	var diags diag.Diagnostics
	targets := buildLlmModelAccessTargets([]llmModelAccessTargetModel{
		{Kind: types.StringValue("model_alias"), Alias: types.StringValue("gpt-*")},
		{
			Kind:       types.StringValue("provider_model"),
			ProviderID: types.StringValue("aaaa0000-0000-0000-0000-000000000001"),
			Model:      types.StringValue("fast"),
		},
	}, &diags)
	if diags.HasError() {
		t.Fatalf("diagnostics: %+v", diags)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(targets))
	}
	if targets[0].Kind != "model_alias" || targets[0].Alias == nil || *targets[0].Alias != "gpt-*" {
		t.Errorf("target[0] = %+v, want kind=model_alias alias=gpt-*", targets[0])
	}
	if targets[1].ProviderID == nil || targets[1].Model == nil {
		t.Errorf("target[1] = %+v, want provider_id and model set", targets[1])
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

func TestLlmModelAccessResource_lifecycle(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_model_access.test"

	minimalConfig := `
resource "barndoor_llm_model_access" "test" {
  name        = "Frontier models only"
  scope_type  = "org"
  policy_type = "allowlist"

  targets = [
    { kind = "model_alias", alias = "gpt-*" },
  ]
}
`
	expandedConfig := `
resource "barndoor_llm_model_access" "test" {
  name        = "Engineering denylist"
  scope_type  = "group"
  scope_value = "engineering"
  policy_type = "denylist"

  targets = [
    { kind = "model", model = "gpt-4o-mini" },
    { kind = "provider", provider_id = "aaaa0000-0000-0000-0000-000000000099" },
    { kind = "provider_model", provider_id = "aaaa0000-0000-0000-0000-000000000098", model = "fast" },
  ]

  traffic_type = "all"
  enabled      = false
}
`

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmModelAccessDeleted(fake),
		Steps: []resource.TestStep{
			{
				// Minimal policy: traffic_type defaults to llm, enabled to
				// true (via the create-then-listing round trip).
				Config: minimalConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckResourceAttr(resourceName, "org_id", fakeLlmOrgID),
					resource.TestCheckResourceAttr(resourceName, "traffic_type", "llm"),
					resource.TestCheckResourceAttr(resourceName, "enabled", "true"),
					resource.TestCheckResourceAttr(resourceName, "targets.#", "1"),
					resource.TestCheckResourceAttr(resourceName, "targets.0.kind", "model_alias"),
					resource.TestCheckResourceAttr(resourceName, "targets.0.alias", "gpt-*"),
					resource.TestCheckNoResourceAttr(resourceName, "scope_id"),
					resource.TestCheckNoResourceAttr(resourceName, "scope_value"),
				),
			},
			{
				// A second plan over unchanged configuration must be empty.
				Config:   minimalConfig,
				PlanOnly: true,
			},
			{
				// In-place update: rescope to an IdP group, flip to denylist,
				// swap the target set, widen traffic, disable.
				Config: expandedConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "name", "Engineering denylist"),
					resource.TestCheckResourceAttr(resourceName, "scope_type", "group"),
					resource.TestCheckResourceAttr(resourceName, "scope_value", "engineering"),
					resource.TestCheckResourceAttr(resourceName, "policy_type", "denylist"),
					resource.TestCheckResourceAttr(resourceName, "targets.#", "3"),
					resource.TestCheckResourceAttr(resourceName, "targets.1.kind", "provider"),
					resource.TestCheckResourceAttr(resourceName, "traffic_type", "all"),
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

func TestLlmModelAccessResource_outOfBandDeletePlansRecreate(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_model_access.test"

	config := `
resource "barndoor_llm_model_access" "test" {
  name        = "tf-llm-access-oob"
  scope_type  = "org"
  policy_type = "denylist"

  targets = [
    { kind = "model", model = "gpt-4o-mini" },
  ]
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
				PreConfig: func() { fake.markPolicyDeletedLlm(t, firstID) },
				Config:    config,
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

func TestLlmModelAccessResource_malformedProviderIDIs422(t *testing.T) {
	setupLlmGatewayTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// provider_id is deserialized as a UUID server-side; a
				// malformed one is rejected by the API (the schema does not
				// duplicate UUID validation).
				Config: `
resource "barndoor_llm_model_access" "test" {
  name        = "tf-llm-access-bad-uuid"
  scope_type  = "org"
  policy_type = "denylist"

  targets = [
    { kind = "provider", provider_id = "not-a-uuid" },
  ]
}
`,
				ExpectError: regexp.MustCompile(`UUID\s+parsing\s+failed`),
			},
		},
	})
}

// --- multi-entity scope ----------------------------------------------

func TestLlmModelAccessBindingState(t *testing.T) {
	strList := func(vals ...string) types.List {
		elems := make([]attr.Value, 0, len(vals))
		for _, v := range vals {
			elems = append(elems, types.StringValue(v))
		}
		return types.ListValueMust(types.StringType, elems)
	}
	nullList := types.ListNull(types.StringType)
	nullStr := types.StringNull()

	cases := []struct {
		name       string
		bound      []string
		priorList  types.List
		priorOne   types.String
		wantList   types.List
		wantSingle types.String
	}{
		{"list form keeps every entity", []string{"a", "b"}, strList("a", "b"), nullStr,
			strList("a", "b"), nullStr},
		{"list form with no binding stays an empty list", nil, strList(), nullStr,
			strList(), nullStr},
		{"deprecated form reads the first entity", []string{"a"}, nullList, types.StringValue("a"),
			nullList, types.StringValue("a")},
		{"deprecated form hides the rest of a multi-entity binding", []string{"a", "b"}, nullList,
			types.StringValue("a"), nullList, types.StringValue("a")},
		{"deprecated form with the binding gone reads null", nil, nullList, types.StringValue("a"),
			nullList, nullStr},
		{"no prior value: several entities map to the list", []string{"a", "b"}, nullList, nullStr,
			strList("a", "b"), nullStr},
		{"no prior value: one entity maps to the deprecated field", []string{"a"}, nullList, nullStr,
			nullList, types.StringValue("a")},
		{"no prior value: no entity leaves both null", nil, nullList, nullStr, nullList, nullStr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotList, gotSingle := llmBindingState(tc.bound, tc.priorList, tc.priorOne)
			if !gotList.Equal(tc.wantList) {
				t.Errorf("list = %s, want %s", gotList, tc.wantList)
			}
			if !gotSingle.Equal(tc.wantSingle) {
				t.Errorf("single = %s, want %s", gotSingle, tc.wantSingle)
			}
		})
	}
}

func TestLlmModelAccessResource_multiEntityScope(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_model_access.test"
	const userA = "11111111-1111-1111-1111-111111111111"
	const userB = "22222222-2222-2222-2222-222222222222"

	bound := fmt.Sprintf(`
resource "barndoor_llm_model_access" "test" {
  name        = "Two users"
  scope_type  = "user"
  scope_ids   = ["%s", "%s"]
  policy_type = "allowlist"

  targets = [
    { kind = "model_alias", alias = "gpt-*" },
  ]
}
`, userA, userB)

	unbound := `
resource "barndoor_llm_model_access" "test" {
  name        = "Two users"
  scope_type  = "org"
  policy_type = "allowlist"

  targets = [
    { kind = "model_alias", alias = "gpt-*" },
  ]
}
`

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmModelAccessDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: bound,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "scope_ids.#", "2"),
					resource.TestCheckResourceAttr(resourceName, "scope_ids.0", userA),
					resource.TestCheckResourceAttr(resourceName, "scope_ids.1", userB),
					resource.TestCheckNoResourceAttr(resourceName, "scope_id"),
				),
			},
			{
				// Read back from the gateway's list, not the first-element mirror.
				Config:   bound,
				PlanOnly: true,
			},
			{
				// Dropping the binding unbinds the policy in place, without replacement.
				Config: unbound,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(resourceName, "scope_ids"),
					resource.TestCheckNoResourceAttr(resourceName, "scope_id"),
				),
			},
			{
				Config:   unbound,
				PlanOnly: true,
			},
			{
				// Import reassembles every bound entity from the listing.
				Config:            bound,
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestLlmModelAccessResource_rejectsBothScopeForms(t *testing.T) {
	fake := setupLlmGatewayTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmModelAccessDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: `
resource "barndoor_llm_model_access" "test" {
  name        = "Both forms"
  scope_type  = "user"
  scope_id    = "11111111-1111-1111-1111-111111111111"
  scope_ids   = ["11111111-1111-1111-1111-111111111111"]
  policy_type = "allowlist"

  targets = [
    { kind = "model_alias", alias = "gpt-*" },
  ]
}
`,
				ExpectError: regexp.MustCompile(`Conflicting scope attributes`),
			},
		},
	})
}
