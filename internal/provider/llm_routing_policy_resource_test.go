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
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// --- schema tests --------------------------------------------------------------

func TestLlmRoutingPolicyResource_Metadata(t *testing.T) {
	var resp frameworkresource.MetadataResponse
	NewLlmRoutingPolicyResource().Metadata(context.Background(),
		frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_llm_routing_policy"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestLlmRoutingPolicyResource_Schema(t *testing.T) {
	var resp frameworkresource.SchemaResponse
	NewLlmRoutingPolicyResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{
		"id", "org_id", "model_alias", "description", "enabled", "determiner_model_alias",
		"determiner_prompt", "posture", "slots", "context_breakpoints", "router_input_max_chars",
		"default_slot_on_failure", "created_at", "updated_at",
	} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
	for _, required := range []string{"model_alias", "determiner_model_alias", "slots"} {
		if !resp.Schema.Attributes[required].IsRequired() {
			t.Errorf("%s should be Required", required)
		}
	}
	for _, computed := range []string{
		"id", "org_id", "enabled", "posture", "context_breakpoints", "router_input_max_chars",
		"default_slot_on_failure", "created_at", "updated_at",
	} {
		if !resp.Schema.Attributes[computed].IsComputed() {
			t.Errorf("%s should be Computed", computed)
		}
	}
	for _, optionalOnly := range []string{"description", "determiner_prompt"} {
		if a := resp.Schema.Attributes[optionalOnly]; !a.IsOptional() || a.IsComputed() {
			t.Errorf("%s should be Optional and not Computed (removing it clears it)", optionalOnly)
		}
	}
}

func TestLlmRoutingTargetAlias(t *testing.T) {
	for in, want := range map[string]string{
		"auto":          "auto",
		"openai/auto":   "auto",
		"/auto":         "/auto",
		"openai/":       "openai/",
		"a/b/c":         "b/c",
		"Seeded/gpt-4o": "gpt-4o",
	} {
		if got := llmRoutingTargetAlias(in); got != want {
			t.Errorf("llmRoutingTargetAlias(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- shared fixtures -------------------------------------------------------------

// llmRoutingMappingsHCL enables three models on the seeded provider and gives
// each a bare-callable custom alias (cheap / mid / strong) — the targets a
// routing policy's slots name.
func llmRoutingMappingsHCL(providerID string) string {
	out := ""
	for _, m := range []struct{ name, upstream string }{
		{"cheap", "gpt-4o-mini"}, {"mid", "gpt-4o"}, {"strong", "o3"},
	} {
		out += fmt.Sprintf(`
resource "barndoor_llm_model_mapping" "%[1]s_enable" {
  provider_id    = %[3]q
  model_alias    = %[2]q
  upstream_model = %[2]q
}

resource "barndoor_llm_model_mapping" "%[1]s" {
  provider_id    = %[3]q
  model_alias    = %[1]q
  upstream_model = %[2]q
  depends_on     = [barndoor_llm_model_mapping.%[1]s_enable]
}
`, m.name, m.upstream, providerID)
	}
	return out
}

// llmRoutingPolicyHCL renders a policy named "auto" with the given body.
func llmRoutingPolicyHCL(body string) string {
	return `
resource "barndoor_llm_routing_policy" "auto" {
  model_alias            = "auto"
  determiner_model_alias = barndoor_llm_model_mapping.cheap.model_alias
` + body + `
}
`
}

const llmRoutingThreeSlots = `
  slots = [
    { model_alias = barndoor_llm_model_mapping.cheap.model_alias },
    { model_alias = barndoor_llm_model_mapping.mid.model_alias },
    { model_alias = barndoor_llm_model_mapping.strong.model_alias },
  ]`

// --- lifecycle (real plan/apply against the fake) ---------------------------------

func TestLlmRoutingPolicyResource_lifecycle(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	provider := fake.seedProvider()
	const resourceName = "barndoor_llm_routing_policy.auto"
	mappings := llmRoutingMappingsHCL(provider.ID)

	minimalConfig := mappings + llmRoutingPolicyHCL(llmRoutingThreeSlots)
	expandedConfig := mappings + llmRoutingPolicyHCL(`
  description             = "Cheap by default, strong when it matters"
  enabled                 = false
  determiner_prompt       = "Prefer the cheap slot for chit-chat."
  posture                 = "quality"
  router_input_max_chars  = 8000
  default_slot_on_failure = 0
  context_breakpoints     = [200000]
  slots = [
    { model_alias = barndoor_llm_model_mapping.cheap.model_alias, label = "Cheap" },
    {
      # The provider-scoped form reaches the 1:1 enablement directly.
      model_alias = "Seeded openai/o3"
      label       = "Strong"
      description = "Hard reasoning, long documents"
    },
  ]`)

	var policyID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmRoutingDeleted(fake),
		Steps: []resource.TestStep{
			{
				// Minimal: every optional setting takes the platform default.
				Config: minimalConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckResourceAttr(resourceName, "org_id", fakeLlmOrgID),
					resource.TestCheckResourceAttr(resourceName, "enabled", "true"),
					resource.TestCheckResourceAttr(resourceName, "determiner_model_alias", "cheap"),
					resource.TestCheckResourceAttr(resourceName, "posture", "balanced"),
					resource.TestCheckResourceAttr(resourceName, "slots.#", "3"),
					resource.TestCheckResourceAttr(resourceName, "slots.2.model_alias", "strong"),
					resource.TestCheckNoResourceAttr(resourceName, "slots.0.label"),
					resource.TestCheckResourceAttr(resourceName, "context_breakpoints.#", "2"),
					resource.TestCheckResourceAttr(resourceName, "context_breakpoints.0", "128000"),
					resource.TestCheckResourceAttr(resourceName, "context_breakpoints.1", "512000"),
					resource.TestCheckResourceAttr(resourceName, "router_input_max_chars", "12000"),
					resource.TestCheckResourceAttr(resourceName, "default_slot_on_failure", "1"),
					resource.TestCheckNoResourceAttr(resourceName, "description"),
					resource.TestCheckNoResourceAttr(resourceName, "determiner_prompt"),
					resource.TestCheckResourceAttrSet(resourceName, "created_at"),
					func(s *terraform.State) error {
						policyID = s.RootModule().Resources[resourceName].Primary.ID
						return nil
					},
				),
			},
			{
				Config:   minimalConfig,
				PlanOnly: true,
			},
			{
				// In-place update of every setting, down to two slots.
				Config: expandedConfig,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(resourceName, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "description", "Cheap by default, strong when it matters"),
					resource.TestCheckResourceAttr(resourceName, "enabled", "false"),
					resource.TestCheckResourceAttr(resourceName, "posture", "quality"),
					resource.TestCheckResourceAttr(resourceName, "slots.#", "2"),
					resource.TestCheckResourceAttr(resourceName, "slots.0.label", "Cheap"),
					resource.TestCheckNoResourceAttr(resourceName, "slots.0.description"),
					resource.TestCheckResourceAttr(resourceName, "slots.1.model_alias", "Seeded openai/o3"),
					resource.TestCheckResourceAttr(resourceName, "slots.1.description", "Hard reasoning, long documents"),
					resource.TestCheckResourceAttr(resourceName, "context_breakpoints.#", "1"),
					resource.TestCheckResourceAttr(resourceName, "router_input_max_chars", "8000"),
					resource.TestCheckResourceAttr(resourceName, "default_slot_on_failure", "0"),
					func(*terraform.State) error {
						p := fake.routingPolicySnapshot(policyID)
						if p == nil || p.DeterminerPrompt == nil || *p.DeterminerPrompt != "Prefer the cheap slot for chit-chat." {
							return fmt.Errorf("platform determiner_prompt = %v", p.DeterminerPrompt)
						}
						return nil
					},
				),
			},
			{
				Config:   expandedConfig,
				PlanOnly: true,
			},
			{
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				// Back to minimal: description and determiner_prompt are
				// cleared (sent as "", since the PUT reads null as "keep"),
				// and the rest reset to their defaults.
				Config: minimalConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(resourceName, "description"),
					resource.TestCheckNoResourceAttr(resourceName, "determiner_prompt"),
					resource.TestCheckResourceAttr(resourceName, "posture", "balanced"),
					resource.TestCheckResourceAttr(resourceName, "enabled", "true"),
					resource.TestCheckResourceAttr(resourceName, "context_breakpoints.#", "2"),
					resource.TestCheckResourceAttr(resourceName, "default_slot_on_failure", "1"),
					func(*terraform.State) error {
						p := fake.routingPolicySnapshot(policyID)
						if p == nil {
							return fmt.Errorf("policy %s gone", policyID)
						}
						if p.Description == nil || *p.Description != "" ||
							p.DeterminerPrompt == nil || *p.DeterminerPrompt != "" {
							return fmt.Errorf("expected cleared strings on the platform, got description=%v prompt=%v",
								p.Description, p.DeterminerPrompt)
						}
						return nil
					},
				),
			},
			{
				// The server's "" must settle to null, not plan a perpetual diff.
				Config:   minimalConfig,
				PlanOnly: true,
			},
			{
				// Import of a cleared policy: "" also settles to null.
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestLlmRoutingPolicyResource_outOfBandDeletePlansRecreate(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	provider := fake.seedProvider()
	const resourceName = "barndoor_llm_routing_policy.auto"
	config := llmRoutingMappingsHCL(provider.ID) + llmRoutingPolicyHCL(llmRoutingThreeSlots)

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
				PreConfig: func() { fake.markRoutingPolicyDeleted(t, firstID) },
				Config:    config,
				Check: func(s *terraform.State) error {
					if id := s.RootModule().Resources[resourceName].Primary.ID; id == firstID {
						return fmt.Errorf("expected a new policy id after out-of-band delete, still %s", id)
					}
					return nil
				},
			},
		},
	})
}

// --- plan-time validation ------------------------------------------------------------

func TestLlmRoutingPolicyResource_planTimeValidation(t *testing.T) {
	setupLlmGatewayTest(t)

	policy := func(alias, body string) string {
		return fmt.Sprintf(`
resource "barndoor_llm_routing_policy" "auto" {
  model_alias            = %q
  determiner_model_alias = "cheap"
%s
}
`, alias, body)
	}
	slots := func(aliases ...string) string {
		out := "  slots = ["
		for _, a := range aliases {
			out += fmt.Sprintf("{ model_alias = %q },", a)
		}
		return out + "]\n"
	}

	cases := []struct {
		name   string
		config string
		want   string
	}{
		{"one slot", policy("auto", slots("cheap")), `list must contain at least 2 elements`},
		{
			"two slots without breakpoints",
			policy("auto", slots("cheap", "strong")),
			`(?s)needs exactly 1 context_breakpoints.*default \[128000, 512000\]`,
		},
		{
			"four slots without breakpoints",
			policy("auto", slots("a", "b", "c", "d")),
			`(?s)needs exactly 3 context_breakpoints`,
		},
		{
			"explicit breakpoint count mismatch",
			policy("auto", slots("a", "b", "c")+"  context_breakpoints = [1000]\n"),
			`must have exactly 2 entries for 3 slots`,
		},
		{
			"non-increasing breakpoints",
			policy("auto", slots("a", "b", "c")+"  context_breakpoints = [512000, 128000]\n"),
			`must be strictly increasing`,
		},
		{
			"equal breakpoints",
			policy("auto", slots("a", "b", "c")+"  context_breakpoints = [1000, 1000]\n"),
			`must be strictly increasing`,
		},
		{
			"non-positive breakpoint",
			policy("auto", slots("a", "b")+"  context_breakpoints = [0]\n"),
			`must be between 1 and`,
		},
		{
			"default slot out of range",
			policy("auto", slots("a", "b", "c")+"  default_slot_on_failure = 3\n"),
			`default_slot_on_failure is 3 but the policy has 3 slots`,
		},
		{
			"default slot out of range for two slots",
			policy("auto", slots("a", "b")+"  context_breakpoints = [1000]\n  default_slot_on_failure = 2\n"),
			`default_slot_on_failure is 2 but the policy has 2 slots`,
		},
		{
			"slot targets the policy alias",
			policy("auto", slots("a", "auto", "c")),
			`Slot 1 targets "auto"`,
		},
		{
			"slot targets the policy alias via provider prefix and case",
			policy("Auto", slots("a", "Seeded openai/AUTO", "c")),
			`Slot 1 targets "Seeded openai/AUTO"`,
		},
		{
			"determiner is the policy alias",
			`
resource "barndoor_llm_routing_policy" "auto" {
  model_alias            = "auto"
  determiner_model_alias = "openai/AUTO"
  slots                  = [{ model_alias = "a" }, { model_alias = "b" }, { model_alias = "c" }]
}
`,
			`Determiner cannot be the routing policy itself`,
		},
		{"slash in policy alias", policy("team/auto", slots("a", "b", "c")), `must not contain "/"`},
		{"padded policy alias", policy(" auto", slots("a", "b", "c")), `leading/trailing whitespace`},
		{"unknown posture", policy("auto", slots("a", "b", "c")+"  posture = \"cheap\"\n"), `posture value must be one of`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{
					{
						Config:      c.config,
						PlanOnly:    true,
						ExpectError: regexp.MustCompile(c.want),
					},
				},
			})
		})
	}
}

// --- server-side rejections ---------------------------------------------------------

func TestLlmRoutingPolicyResource_unknownSlotTargetIs400(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	provider := fake.seedProvider()

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmRoutingDeleted(fake),
		Steps: []resource.TestStep{
			{
				// A slot naming a model nobody routes is rejected with the
				// handler's message verbatim.
				Config: llmRoutingMappingsHCL(provider.ID) + llmRoutingPolicyHCL(`
  slots = [
    { model_alias = barndoor_llm_model_mapping.cheap.model_alias },
    { model_alias = "nope" },
    { model_alias = barndoor_llm_model_mapping.strong.model_alias },
  ]`),
				ExpectError: regexp.MustCompile(`(?s)rejected by the LLM Gateway API.*target 'nope' has no enabled routes`),
			},
			{
				// A 1:1 enablement is not bare-callable by default, so its bare
				// name is not a valid target either (use "<provider>/<model>").
				Config: llmRoutingMappingsHCL(provider.ID) + llmRoutingPolicyHCL(`
  slots = [
    { model_alias = "gpt-4o-mini" },
    { model_alias = barndoor_llm_model_mapping.mid.model_alias },
    { model_alias = barndoor_llm_model_mapping.strong.model_alias },
  ]`),
				ExpectError: regexp.MustCompile(`target 'gpt-4o-mini' has no enabled routes`),
			},
		},
	})
}

func TestLlmRoutingPolicyResource_aliasShadowingMappingIs400(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	provider := fake.seedProvider()

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmRoutingDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: llmRoutingMappingsHCL(provider.ID) + `
resource "barndoor_llm_routing_policy" "shadow" {
  model_alias            = barndoor_llm_model_mapping.cheap.model_alias
  determiner_model_alias = "mid"
  context_breakpoints    = [100000]
  slots = [
    { model_alias = barndoor_llm_model_mapping.mid.model_alias },
    { model_alias = barndoor_llm_model_mapping.strong.model_alias },
  ]
}
`,
				ExpectError: regexp.MustCompile(`smart aliases must\s+be\s+distinct`),
			},
		},
	})
}

func TestLlmRoutingPolicyResource_duplicateAliasIs409(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	provider := fake.seedProvider()
	// Created in the app with different casing; the unique index is on
	// lower(model_alias).
	fake.seedRoutingPolicy("AUTO", "cheap", "mid", "strong")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      llmRoutingMappingsHCL(provider.ID) + llmRoutingPolicyHCL(llmRoutingThreeSlots),
				ExpectError: regexp.MustCompile(`(?s)conflicts with an existing one.*A resource with that name already exists`),
			},
		},
	})
}
