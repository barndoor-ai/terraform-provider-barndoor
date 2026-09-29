// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const llmBillingResourceName = "barndoor_llm_provider.test"

func llmBillingConfig(name, billing string) string {
	return llmConnectionHCL("anthropic", "anthropic", "sk-test") + fmt.Sprintf(`
resource "barndoor_llm_provider" "test" {
  name           = %q
  model_provider = "anthropic"
  connection_id  = barndoor_llm_connection.anthropic.id
%s
}
`, name, billing)
}

// checkLlmBillingStored asserts the platform's stored billing columns; a nil
// pointer argument means the column must be null.
func checkLlmBillingStored(fake *fakeLlmGatewayServer, mode string, reason, note *string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if len(fake.providers) != 1 {
			return fmt.Errorf("expected 1 provider, have %d", len(fake.providers))
		}
		p := fake.providers[0]
		if p.BillingMode != mode {
			return fmt.Errorf("platform billing_mode = %q, want %q", p.BillingMode, mode)
		}
		if !strPtrEq(p.BillingReason, reason) || (p.BillingReason == nil) != (reason == nil) {
			return fmt.Errorf("platform billing_reason = %v, want %v", p.BillingReason, reason)
		}
		if !strPtrEq(p.BillingNote, note) || (p.BillingNote == nil) != (note == nil) {
			return fmt.Errorf("platform billing_note = %v, want %v", p.BillingNote, note)
		}
		return nil
	}
}

func strPtr(s string) *string { return &s }

func TestLlmProviderResource_billingLifecycle(t *testing.T) {
	fake := setupLlmGatewayTest(t)

	subscription := llmBillingConfig("Claude plan", `
  billing_mode   = "not_metered"
  billing_reason = "subscription"
  billing_note   = "Team plan, renews annually"`)
	// A subscription that bills overages per token is genuinely both: the two
	// attributes are independent, so this must be representable.
	overage := llmBillingConfig("Claude plan", `
  billing_mode   = "per_token"
  billing_reason = "subscription"
  billing_note   = "Team plan, renews annually"`)
	noNote := llmBillingConfig("Claude plan", `
  billing_mode   = "per_token"
  billing_reason = "subscription"`)
	modeOnly := llmBillingConfig("Claude plan", `
  billing_mode = "per_token"`)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmProvidersDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: subscription,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(llmBillingResourceName, "billing_mode", "not_metered"),
					resource.TestCheckResourceAttr(llmBillingResourceName, "billing_reason", "subscription"),
					resource.TestCheckResourceAttr(llmBillingResourceName, "billing_note", "Team plan, renews annually"),
					checkLlmBillingStored(fake, "not_metered", strPtr("subscription"), strPtr("Team plan, renews annually")),
				),
			},
			{
				Config:   subscription,
				PlanOnly: true,
			},
			{
				ResourceName:      llmBillingResourceName,
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				Config: overage,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(llmBillingResourceName, "billing_mode", "per_token"),
					resource.TestCheckResourceAttr(llmBillingResourceName, "billing_reason", "subscription"),
					checkLlmBillingStored(fake, "per_token", strPtr("subscription"), strPtr("Team plan, renews annually")),
				),
			},
			{
				// Removing a note the configuration set clears it on the
				// platform via an explicit null.
				Config: noNote,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(llmBillingResourceName, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(llmBillingResourceName, "billing_note"),
					checkLlmBillingStored(fake, "per_token", strPtr("subscription"), nil),
				),
			},
			{
				Config:   noNote,
				PlanOnly: true,
			},
			{
				Config: modeOnly,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(llmBillingResourceName, "billing_reason"),
					checkLlmBillingStored(fake, "per_token", nil, nil),
				),
			},
			{
				// Removing billing_mode keeps the stored mode: it has a
				// platform default, and a config that stops mentioning it must
				// not reset a provider.
				Config:   llmBillingConfig("Claude plan", ""),
				PlanOnly: true,
			},
		},
	})
}

// A configuration that never mentions billing must not disturb billing set in
// the app: an unrelated update leaves all three columns alone, and sends no
// clearing null.
func TestLlmProviderResource_billingSetOutOfBandIsLeftAlone(t *testing.T) {
	fake := setupLlmGatewayTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmProvidersDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: llmBillingConfig("Local models", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(llmBillingResourceName, "billing_mode", "per_token"),
					resource.TestCheckNoResourceAttr(llmBillingResourceName, "billing_reason"),
					resource.TestCheckNoResourceAttr(llmBillingResourceName, "billing_note"),
				),
			},
			{
				PreConfig: func() {
					fake.mu.Lock()
					defer fake.mu.Unlock()
					p := fake.providers[0]
					p.BillingMode = "not_metered"
					p.BillingReason = strPtr("local")
					p.BillingNote = strPtr("vLLM on our own GPUs")
				},
				// The refresh picks the app's values up; the plan is empty.
				Config:   llmBillingConfig("Local models", ""),
				PlanOnly: true,
			},
			{
				// An unrelated in-place change must carry the stored billing
				// through, not reset the mode or clear the reason and note.
				Config: llmBillingConfig("Local models (renamed)", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(llmBillingResourceName, "billing_mode", "not_metered"),
					resource.TestCheckResourceAttr(llmBillingResourceName, "billing_reason", "local"),
					resource.TestCheckResourceAttr(llmBillingResourceName, "billing_note", "vLLM on our own GPUs"),
					checkLlmBillingStored(fake, "not_metered", strPtr("local"), strPtr("vLLM on our own GPUs")),
				),
			},
			{
				// Taking ownership of one attribute, then dropping it,
				// clears only that one. Ownership is taken by an apply, so
				// the adopted value differs from the stored one here.
				Config: llmBillingConfig("Local models (renamed)", `
  billing_note = "vLLM on our own A100s"`),
				Check: checkLlmBillingStored(fake, "not_metered", strPtr("local"), strPtr("vLLM on our own A100s")),
			},
			{
				Config: llmBillingConfig("Local models (renamed)", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(llmBillingResourceName, "billing_reason", "local"),
					resource.TestCheckNoResourceAttr(llmBillingResourceName, "billing_note"),
					checkLlmBillingStored(fake, "not_metered", strPtr("local"), nil),
				),
			},
		},
	})
}

func TestLlmProviderResource_billingRejectedAtPlan(t *testing.T) {
	cases := []struct {
		name    string
		billing string
		want    *regexp.Regexp
	}{
		{
			name:    "not_metered without a reason",
			billing: `billing_mode = "not_metered"`,
			want:    regexp.MustCompile(`billing_reason is required when billing_mode is\s+"not_metered"`),
		},
		{
			name:    "unknown mode",
			billing: `billing_mode = "flat_rate"`,
			want:    regexp.MustCompile(`value must be one of`),
		},
		{
			name:    "unknown reason",
			billing: `billing_reason = "prepaid"`,
			want:    regexp.MustCompile(`value must be one of`),
		},
		{
			name:    "note too long",
			billing: fmt.Sprintf(`billing_note = %q`, strings.Repeat("é", llmBillingNoteMaxLen+1)),
			want:    regexp.MustCompile(`character count must be at most 200`),
		},
		{
			name:    "padded note",
			billing: `billing_note = " padded "`,
			want:    regexp.MustCompile(`must not be empty or have leading/trailing\s+whitespace`),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := setupLlmGatewayTest(t)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{
					{
						Config:      llmBillingConfig("Rejected", c.billing),
						PlanOnly:    true,
						ExpectError: c.want,
					},
				},
			})
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if len(fake.providers) != 0 {
				t.Fatalf("a rejected billing shape reached the API")
			}
		})
	}

	// 200 characters (not bytes) is the limit, as on the platform.
	t.Run("note at the limit", func(t *testing.T) {
		setupLlmGatewayTest(t)
		note := strings.Repeat("é", llmBillingNoteMaxLen)
		resource.UnitTest(t, resource.TestCase{
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
			Steps: []resource.TestStep{
				{
					Config: llmBillingConfig("At the limit", fmt.Sprintf(`billing_note = %q`, note)),
					Check:  resource.TestCheckResourceAttr(llmBillingResourceName, "billing_note", note),
				},
			},
		})
	})
}

func TestLlmProviderDataSource_billing(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	seeded := fake.seedProvider()
	fake.mu.Lock()
	seeded.BillingMode = "not_metered"
	seeded.BillingReason = strPtr("external")
	fake.mu.Unlock()

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
data "barndoor_llm_provider" "test" {
  id = %q
}
`, seeded.ID),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.barndoor_llm_provider.test", "billing_mode", "not_metered"),
					resource.TestCheckResourceAttr("data.barndoor_llm_provider.test", "billing_reason", "external"),
					resource.TestCheckNoResourceAttr("data.barndoor_llm_provider.test", "billing_note"),
				),
			},
		},
	})
}

func TestLlmProviderResource_azureFoundry(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmProvidersDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: `
resource "barndoor_llm_connection" "foundry" {
  name           = "Foundry key"
  model_provider = "azure_foundry"
  base_url       = "https://example.services.ai.azure.com"
  api_key        = "foundry-key"
}

resource "barndoor_llm_provider" "test" {
  name           = "Foundry"
  model_provider = "azure_foundry"
  connection_id  = barndoor_llm_connection.foundry.id
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(llmBillingResourceName, "model_provider", "azure_foundry"),
					resource.TestCheckResourceAttr(llmBillingResourceName, "auth_type", "azure_foundry_api_key"),
				),
			},
		},
	})
}
