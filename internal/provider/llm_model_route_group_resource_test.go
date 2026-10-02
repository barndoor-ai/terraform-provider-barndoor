// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	frameworkresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const llmRouteGroupResourceName = "barndoor_llm_model_route_group.test"

// --- schema tests --------------------------------------------------------------

func TestLlmModelRouteGroupResource_Schema(t *testing.T) {
	var meta frameworkresource.MetadataResponse
	NewLlmModelRouteGroupResource().Metadata(context.Background(),
		frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &meta)
	if got, want := meta.TypeName, "barndoor_llm_model_route_group"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}

	var resp frameworkresource.SchemaResponse
	NewLlmModelRouteGroupResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{"id", "org_id", "name", "description", "model_aliases", "change_note"} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
	if !resp.Schema.Attributes["name"].IsRequired() {
		t.Error("name should be Required")
	}
	for _, computed := range []string{"id", "org_id", "description", "model_aliases"} {
		if !resp.Schema.Attributes[computed].IsComputed() {
			t.Errorf("%s should be Computed", computed)
		}
	}
	// change_note is never read back, so a Computed flag would let the
	// framework carry a stale note forward as if the server had stored it.
	if attr := resp.Schema.Attributes["change_note"]; attr.IsComputed() || !attr.IsOptional() {
		t.Error("change_note should be Optional and not Computed")
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

func TestLlmModelRouteGroupResource_lifecycle(t *testing.T) {
	fake := setupLlmGatewayTest(t)

	minimalConfig := `
resource "barndoor_llm_model_route_group" "test" {
  name = "Frontier"
}
`
	fullConfig := `
resource "barndoor_llm_model_route_group" "test" {
  name          = "Frontier"
  description   = "Routes cleared for production traffic"
  model_aliases = ["gpt-4o", "claude-sonnet"]
  change_note   = "Initial frontier set"
}
`
	updatedConfig := `
resource "barndoor_llm_model_route_group" "test" {
  name          = "Frontier (prod)"
  description   = "Cleared routes"
  model_aliases = ["claude-sonnet", "gemini-pro"]
}
`

	var groupID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmRouteGroupsDeleted(fake),
		Steps: []resource.TestStep{
			{
				// Unset description and model_aliases settle to the server
				// defaults ("" and an empty set) without a diff.
				Config: minimalConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(llmRouteGroupResourceName, "id"),
					resource.TestCheckResourceAttr(llmRouteGroupResourceName, "org_id", fakeLlmOrgID),
					resource.TestCheckResourceAttr(llmRouteGroupResourceName, "name", "Frontier"),
					resource.TestCheckResourceAttr(llmRouteGroupResourceName, "description", ""),
					resource.TestCheckResourceAttr(llmRouteGroupResourceName, "model_aliases.#", "0"),
					resource.TestCheckNoResourceAttr(llmRouteGroupResourceName, "change_note"),
					func(s *terraform.State) error {
						groupID = s.RootModule().Resources[llmRouteGroupResourceName].Primary.ID
						return nil
					},
				),
			},
			{
				Config:   minimalConfig,
				PlanOnly: true,
			},
			{
				// Adding a description, members and a note is in place, and
				// the note reaches the platform's audit trail.
				Config: fullConfig,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(llmRouteGroupResourceName, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(llmRouteGroupResourceName, "description",
						"Routes cleared for production traffic"),
					resource.TestCheckResourceAttr(llmRouteGroupResourceName, "model_aliases.#", "2"),
					resource.TestCheckTypeSetElemAttr(llmRouteGroupResourceName, "model_aliases.*", "gpt-4o"),
					resource.TestCheckTypeSetElemAttr(llmRouteGroupResourceName, "model_aliases.*", "claude-sonnet"),
					resource.TestCheckResourceAttr(llmRouteGroupResourceName, "change_note", "Initial frontier set"),
					checkFakeRouteGroup(fake, &groupID, []string{"claude-sonnet", "gpt-4o"}, "Initial frontier set"),
				),
			},
			{
				Config:   fullConfig,
				PlanOnly: true,
			},
			{
				// Rename, reword, and swap a member, all in place. Dropping
				// change_note sends an update without one, which clears the
				// stored note.
				Config: updatedConfig,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(llmRouteGroupResourceName, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(llmRouteGroupResourceName, "id", &groupID),
					resource.TestCheckResourceAttr(llmRouteGroupResourceName, "name", "Frontier (prod)"),
					resource.TestCheckResourceAttr(llmRouteGroupResourceName, "description", "Cleared routes"),
					resource.TestCheckResourceAttr(llmRouteGroupResourceName, "model_aliases.#", "2"),
					resource.TestCheckTypeSetElemAttr(llmRouteGroupResourceName, "model_aliases.*", "gemini-pro"),
					resource.TestCheckNoResourceAttr(llmRouteGroupResourceName, "change_note"),
					checkFakeRouteGroup(fake, &groupID, []string{"claude-sonnet", "gemini-pro"}, ""),
				),
			},
			{
				Config:   updatedConfig,
				PlanOnly: true,
			},
			{
				// Reads walk the org-wide listing (no get-by-id endpoint).
				// change_note is never read back.
				ResourceName:            llmRouteGroupResourceName,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"change_note"},
			},
			{
				// Emptying the membership is an explicit [] in the PUT.
				Config: `
resource "barndoor_llm_model_route_group" "test" {
  name          = "Frontier (prod)"
  description   = "Cleared routes"
  model_aliases = []
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(llmRouteGroupResourceName, "model_aliases.#", "0"),
					checkFakeRouteGroup(fake, &groupID, []string{}, ""),
				),
			},
		},
	})
}

// A route rename or a route's last mapping being deleted changes membership
// on the platform without an edit to the group. Read must surface both as
// drift, and applying must restore the configured set.
func TestLlmModelRouteGroupResource_memberSweepShowsDrift(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	fastMapping := fake.seedRouteGroupMapping("fast")
	slowMapping := fake.seedRouteGroupMapping("slow")

	config := `
resource "barndoor_llm_model_route_group" "test" {
  name          = "Sweepable"
  model_aliases = ["fast", "slow"]
}
`
	var groupID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: func(s *terraform.State) error {
					groupID = s.RootModule().Resources[llmRouteGroupResourceName].Primary.ID
					return nil
				},
			},
			{
				// Renaming the `fast` route moves its membership.
				PreConfig: func() {
					fake.renameMappingOutOfBand(t, fastMapping, "fast-v2")
					if g := fake.routeGroupSnapshot(groupID); !slices.Equal(g.ModelAliases, []string{"fast-v2", "slow"}) {
						t.Fatalf("sweep did not move the membership: %v", g.ModelAliases)
					}
				},
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// Re-applying puts the configured set back.
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(llmRouteGroupResourceName, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(llmRouteGroupResourceName, "model_aliases.#", "2"),
					resource.TestCheckTypeSetElemAttr(llmRouteGroupResourceName, "model_aliases.*", "fast"),
					checkFakeRouteGroup(fake, &groupID, []string{"fast", "slow"}, ""),
				),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
			{
				// Deleting the `slow` route's only mapping drops it from the
				// group; the refreshed state shows the shrunken set.
				PreConfig:    func() { fake.markMappingDeleted(t, slowMapping) },
				RefreshState: true,
				// The refreshed membership no longer matches configuration.
				ExpectNonEmptyPlan: true,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(llmRouteGroupResourceName, "model_aliases.#", "1"),
					resource.TestCheckTypeSetElemAttr(llmRouteGroupResourceName, "model_aliases.*", "fast"),
				),
			},
			{
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

func TestLlmModelRouteGroupResource_nameConflictIs409(t *testing.T) {
	setupLlmGatewayTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Names are unique per organization ignoring case.
				Config: `
resource "barndoor_llm_model_route_group" "first" {
  name = "Frontier"
}

resource "barndoor_llm_model_route_group" "second" {
  name       = "frontier"
  depends_on = [barndoor_llm_model_route_group.first]
}
`,
				ExpectError: regexp.MustCompile(`(?s)conflicts with an existing one.*a route group named\s+'frontier'\s+already\s+exists`),
			},
		},
	})
}

func TestLlmModelRouteGroupResource_outOfBandDeletePlansRecreate(t *testing.T) {
	fake := setupLlmGatewayTest(t)

	config := `
resource "barndoor_llm_model_route_group" "test" {
  name          = "tf-route-group-oob"
  model_aliases = ["fast"]
}
`
	var firstID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmRouteGroupsDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: func(s *terraform.State) error {
					firstID = s.RootModule().Resources[llmRouteGroupResourceName].Primary.ID
					return nil
				},
			},
			{
				PreConfig: func() { fake.markRouteGroupDeleted(t, firstID) },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(llmRouteGroupResourceName, plancheck.ResourceActionCreate),
					},
				},
				Check: func(s *terraform.State) error {
					if id := s.RootModule().Resources[llmRouteGroupResourceName].Primary.ID; id == firstID {
						return fmt.Errorf("expected a new route group id after out-of-band delete, still %s", firstID)
					}
					return nil
				},
			},
		},
	})
}

func TestLlmModelRouteGroupResource_planTimeRejections(t *testing.T) {
	setupLlmGatewayTest(t)

	cases := []struct {
		name   string
		config string
		want   string
	}{
		{"blank name", `name = "   "`, `must not be empty or have\s+leading/trailing\s+whitespace`},
		{"padded name", `name = " Frontier"`, `must not be empty or have\s+leading/trailing\s+whitespace`},
		{"padded description", "name = \"Frontier\"\n  description = \"trailing \"",
			`must not have\s+leading/trailing\s+whitespace`},
		{"whitespace alias", "name = \"Frontier\"\n  model_aliases = [\"fast \"]",
			`must not be empty or have\s+leading/trailing\s+whitespace`},
		{"empty alias", "name = \"Frontier\"\n  model_aliases = [\"\"]",
			`must not be empty or have\s+leading/trailing\s+whitespace`},
		{"long change_note", fmt.Sprintf("name = \"Frontier\"\n  change_note = %q", strings.Repeat("n", 501)),
			`string length must be at\s+most\s+500`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{
					{
						Config: fmt.Sprintf(`
resource "barndoor_llm_model_route_group" "test" {
  %s
}
`, tc.config),
						PlanOnly:    true,
						ExpectError: regexp.MustCompile(tc.want),
					},
				},
			})
		})
	}
}

// --- model access targeting a route group -------------------------------------------

func TestLlmModelAccessResource_routeGroupTarget(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const accessName = "barndoor_llm_model_access.test"

	config := `
resource "barndoor_llm_model_route_group" "test" {
  name          = "Frontier"
  model_aliases = ["gpt-4o", "claude-sonnet"]
}

resource "barndoor_llm_model_access" "test" {
  name        = "Frontier routes only"
  scope_type  = "org"
  policy_type = "allowlist"

  targets = [
    { kind = "route_group", group_id = barndoor_llm_model_route_group.test.id },
    { kind = "model_alias", alias = "internal-*" },
  ]
}
`

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkAllLlmModelAccessDeleted(fake),
			checkAllLlmRouteGroupsDeleted(fake),
		),
		Steps: []resource.TestStep{
			{
				// group_id is unknown at plan time here; the plan-time shape
				// check must treat it as set.
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(accessName, "targets.#", "2"),
					resource.TestCheckResourceAttr(accessName, "targets.0.kind", "route_group"),
					resource.TestCheckResourceAttrPair(accessName, "targets.0.group_id",
						llmRouteGroupResourceName, "id"),
					resource.TestCheckNoResourceAttr(accessName, "targets.0.alias"),
					resource.TestCheckNoResourceAttr(accessName, "targets.1.group_id"),
				),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
			{
				ResourceName:      accessName,
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestLlmModelAccessResource_routeGroupTargetShapeRejectedAtPlan(t *testing.T) {
	setupLlmGatewayTest(t)

	cases := []struct {
		name   string
		target string
	}{
		{"route_group without group_id", `{ kind = "route_group" }`},
		{"route_group with alias", `{ kind = "route_group", group_id = "abab0000-0000-0000-0000-000000000001", alias = "gpt-*" }`},
		{"model_alias with group_id", `{ kind = "model_alias", alias = "gpt-*", group_id = "abab0000-0000-0000-0000-000000000001" }`},
		{"provider with group_id", `{ kind = "provider", provider_id = "aaaa0000-0000-0000-0000-000000000001", group_id = "abab0000-0000-0000-0000-000000000001" }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{
					{
						Config: fmt.Sprintf(`
resource "barndoor_llm_model_access" "test" {
  name        = "bad shape"
  scope_type  = "org"
  policy_type = "allowlist"
  targets     = [%s]
}
`, tc.target),
						PlanOnly:    true,
						ExpectError: regexp.MustCompile(`Invalid model-access target shape`),
					},
				},
			})
		})
	}
}

// checkFakeRouteGroup asserts the platform-side membership and stored change
// note of the group whose id *id holds (wantNote "" means no note).
func checkFakeRouteGroup(fake *fakeLlmGatewayServer, id *string, wantAliases []string, wantNote string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		g := fake.routeGroupSnapshot(*id)
		if g == nil {
			return fmt.Errorf("fake has no route group %s", *id)
		}
		if !slices.Equal(g.ModelAliases, wantAliases) {
			return fmt.Errorf("stored model_aliases = %v, want %v", g.ModelAliases, wantAliases)
		}
		gotNote := ""
		if g.LastChangeNote != nil {
			gotNote = *g.LastChangeNote
		}
		if gotNote != wantNote {
			return fmt.Errorf("stored last_change_note = %q, want %q", gotNote, wantNote)
		}
		return nil
	}
}
