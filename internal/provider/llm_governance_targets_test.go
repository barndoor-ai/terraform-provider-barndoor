// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const (
	llmTargetProviderA = "aaaa0000-0000-0000-0000-00000000000a"
	llmTargetProviderB = "aaaa0000-0000-0000-0000-00000000000b"
	llmTargetMcpServer = "cccc0000-0000-0000-0000-00000000000c"
)

func (f *fakeLlmGatewayServer) onlyBudget(t *testing.T) *fakeLlmTokenBudget {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.budgets) != 1 {
		t.Fatalf("fake holds %d budgets, want 1", len(f.budgets))
	}
	b := *f.budgets[0]
	return &b
}

func (f *fakeLlmGatewayServer) onlyRateLimit(t *testing.T) *fakeLlmRateLimit {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.rateLimits) != 1 {
		t.Fatalf("fake holds %d rate limits, want 1", len(f.rateLimits))
	}
	p := *f.rateLimits[0]
	return &p
}

// A cost-only budget sends token_limit 0 and reads back with token_limit
// unset; limits are then added and cleared in place, while a currency change
// replaces.
func TestLlmTokenBudgetResource_costLimit(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const name = "barndoor_llm_token_budget.test"
	config := func(limits string) string {
		return fmt.Sprintf(`
resource "barndoor_llm_token_budget" "test" {
  name       = "Org spend cap"
  scope_type = "org"
  period     = "monthly"
%s
}
`, limits)
	}

	var firstID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmBudgetsDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: config(`  cost_limit = 1250.5`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(name, "token_limit"),
					resource.TestCheckResourceAttr(name, "cost_limit", "1250.5"),
					resource.TestCheckResourceAttr(name, "currency", "USD"),
					func(s *terraform.State) error {
						firstID = s.RootModule().Resources[name].Primary.ID
						if b := fake.onlyBudget(t); b.TokenLimit != 0 || b.CostLimit == nil || *b.CostLimit != 1250.5 {
							return fmt.Errorf("stored token_limit=%d cost_limit=%v, want the cost-only shape",
								b.TokenLimit, b.CostLimit)
						}
						return nil
					},
				),
			},
			{Config: config(`  cost_limit = 1250.5`), PlanOnly: true},
			{
				// Adding a token limit and then dropping the cost limit are
				// both in place.
				Config: config("  cost_limit  = 1250.5\n  token_limit = 1000000"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(name, plancheck.ResourceActionUpdate),
				}},
				Check: resource.TestCheckResourceAttr(name, "token_limit", "1000000"),
			},
			{
				Config: config(`  token_limit = 1000000`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(name, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(name, "cost_limit"),
					func(*terraform.State) error {
						if b := fake.onlyBudget(t); b.CostLimit != nil {
							return fmt.Errorf("cost_limit still stored as %v; removing it must clear it", *b.CostLimit)
						}
						return nil
					},
				),
			},
			{Config: config(`  token_limit = 1000000`), PlanOnly: true},
			{
				// Back to cost-only: token_limit is cleared with a 0.
				Config: config(`  cost_limit = 99.9999`),
				Check: func(*terraform.State) error {
					if b := fake.onlyBudget(t); b.TokenLimit != 0 {
						return fmt.Errorf("token_limit still stored as %d", b.TokenLimit)
					}
					return nil
				},
			},
			{
				Config: config("  cost_limit = 99.9999\n  currency   = \"EUR\""),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(name, plancheck.ResourceActionDestroyBeforeCreate),
				}},
				Check: func(s *terraform.State) error {
					if s.RootModule().Resources[name].Primary.ID == firstID {
						return fmt.Errorf("a currency change must replace the budget")
					}
					if b := fake.onlyBudget(t); b.Currency != "EUR" {
						return fmt.Errorf("stored currency %q", b.Currency)
					}
					return nil
				},
			},
			{
				ResourceName:      name,
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestLlmTokenBudgetResource_limitsRejectedAtPlan(t *testing.T) {
	cases := map[string]struct {
		limits string
		want   *regexp.Regexp
	}{
		"no limit":            {``, regexp.MustCompile(`(?s)Missing Attribute Configuration.*token_limit.*cost_limit`)},
		"five decimal places": {`cost_limit = 10.12345`, regexp.MustCompile(`at\s+most\s+four\s+decimal\s+places`)},
		"zero cost":           {`cost_limit = 0`, regexp.MustCompile(`must\s+be\s+greater\s+than\s+0`)},
		"lower-case currency": {"token_limit = 10\n  currency = \"usd\"", regexp.MustCompile(`ISO 4217`)},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			setupLlmGatewayTest(t)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{{
					Config: fmt.Sprintf(`
resource "barndoor_llm_token_budget" "test" {
  name       = "Rejected"
  scope_type = "org"
  period     = "daily"
  %s
}
`, c.limits),
					PlanOnly:    true,
					ExpectError: c.want,
				}},
			})
		})
	}
}

// Budget targets are create-only: changing one replaces the budget, and the
// target is part of the uniqueness key, so an otherwise identical untargeted
// budget can coexist.
func TestLlmTokenBudgetResource_targets(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const name = "barndoor_llm_token_budget.targeted"
	config := func(target string) string {
		return fmt.Sprintf(`
resource "barndoor_llm_token_budget" "broad" {
  name        = "Org cap"
  scope_type  = "org"
  period      = "monthly"
  token_limit = 5000000
}

resource "barndoor_llm_token_budget" "targeted" {
  name        = "Org cap on one model"
  scope_type  = "org"
  period      = "monthly"
  token_limit = 1000000
  traffic_type = "llm"
%s
}
`, target)
	}

	var firstID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmBudgetsDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: config(fmt.Sprintf("  target_provider_id    = %q\n  target_upstream_model = \"gpt-5.5\"",
					llmTargetProviderA)),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(name, "target_provider_id", llmTargetProviderA),
					resource.TestCheckResourceAttr(name, "target_upstream_model", "gpt-5.5"),
					resource.TestCheckNoResourceAttr(name, "target_model_alias"),
					resource.TestCheckNoResourceAttr("barndoor_llm_token_budget.broad", "target_provider_id"),
					func(s *terraform.State) error {
						firstID = s.RootModule().Resources[name].Primary.ID
						return nil
					},
				),
			},
			{
				Config: config(fmt.Sprintf("  target_provider_id    = %q\n  target_upstream_model = \"gpt-5.5\"",
					llmTargetProviderA)),
				PlanOnly: true,
			},
			{
				Config: config(`  target_model_alias = "fast"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(name, plancheck.ResourceActionDestroyBeforeCreate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(name, "target_model_alias", "fast"),
					resource.TestCheckNoResourceAttr(name, "target_provider_id"),
					func(s *terraform.State) error {
						if s.RootModule().Resources[name].Primary.ID == firstID {
							return fmt.Errorf("a target change must replace the budget")
						}
						return nil
					},
				),
			},
			{
				ResourceName:      name,
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

// A second budget whose scope and targets match an existing one is the
// platform's 409; the target is what makes two otherwise equal rules
// distinct.
func TestLlmTokenBudgetResource_targetConflict(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	budget := func(label, target string) string {
		return fmt.Sprintf(`
resource "barndoor_llm_token_budget" %q {
  name        = %q
  scope_type  = "org"
  period      = "daily"
  token_limit = 100
  target_model_alias = %q
}
`, label, label, target)
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmBudgetsDeleted(fake),
		Steps: []resource.TestStep{
			{Config: budget("fast", "fast") + budget("smart", "smart")},
			{
				Config:      budget("fast", "fast") + budget("smart", "smart") + budget("fast_again", "fast"),
				ExpectError: regexp.MustCompile(`(?s)conflicts with an existing one.*scope, target`),
			},
		},
	})
}

func TestLlmGovernanceTargets_rejectedAtPlan(t *testing.T) {
	cases := map[string]struct {
		body string
		want *regexp.Regexp
	}{
		"alias with provider": {
			fmt.Sprintf("target_model_alias = \"fast\"\n  target_provider_id = %q", llmTargetProviderA),
			regexp.MustCompile(`target_model_alias cannot be combined with a provider target`),
		},
		"upstream without provider": {
			`target_upstream_model = "gpt-5.5"`,
			regexp.MustCompile(`target_upstream_model requires target_provider_id`),
		},
		"mcp server with model target": {
			fmt.Sprintf("target_mcp_server_id = %q\n  target_model_alias = \"fast\"", llmTargetMcpServer),
			regexp.MustCompile(`target_mcp_server_id cannot be combined with a model target`),
		},
		"model target on mcp traffic": {
			"target_model_alias = \"fast\"\n  traffic_type = \"mcp\"",
			regexp.MustCompile(`Model targets need model traffic`),
		},
		"mcp target on llm traffic": {
			fmt.Sprintf("target_mcp_server_id = %q\n  traffic_type = \"llm\"", llmTargetMcpServer),
			regexp.MustCompile(`target_mcp_server_id needs MCP traffic`),
		},
	}
	for name, c := range cases {
		for _, kind := range []string{"budget", "rate_limit"} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				fake := setupLlmGatewayTest(t)
				config := fmt.Sprintf(`
resource "barndoor_llm_token_budget" "test" {
  name        = "Rejected"
  scope_type  = "org"
  period      = "daily"
  token_limit = 100
  %s
}
`, c.body)
				if kind == "rate_limit" {
					config = fmt.Sprintf(`
resource "barndoor_llm_rate_limit" "test" {
  name                = "Rejected"
  scope_type          = "org"
  requests_per_minute = 10
  %s
}
`, c.body)
				}
				resource.UnitTest(t, resource.TestCase{
					ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
					Steps:                    []resource.TestStep{{Config: config, PlanOnly: true, ExpectError: c.want}},
				})
				fake.mu.Lock()
				defer fake.mu.Unlock()
				if len(fake.budgets)+len(fake.rateLimits) != 0 {
					t.Fatal("a rejected configuration reached the API")
				}
			})
		}
	}
}

// Rate-limit targets update in place, including switching target kinds and
// clearing them, because the update API takes explicit nulls.
func TestLlmRateLimitResource_targets(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const name = "barndoor_llm_rate_limit.test"
	config := func(target string) string {
		return fmt.Sprintf(`
resource "barndoor_llm_rate_limit" "test" {
  name              = "Fast alias ceiling"
  scope_type        = "org"
  tokens_per_minute = 50000
%s
}
`, target)
	}
	inPlace := resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
		plancheck.ExpectResourceAction(name, plancheck.ResourceActionUpdate),
	}}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmRateLimitsDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: config(`  target_model_alias = "fast"`),
				Check:  resource.TestCheckResourceAttr(name, "target_model_alias", "fast"),
			},
			{Config: config(`  target_model_alias = "fast"`), PlanOnly: true},
			{
				Config: config(fmt.Sprintf(
					"  target_provider_id    = %q\n  target_upstream_model = \"claude-haiku-4-5\"", llmTargetProviderB)),
				ConfigPlanChecks: inPlace,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(name, "target_model_alias"),
					resource.TestCheckResourceAttr(name, "target_provider_id", llmTargetProviderB),
					func(*terraform.State) error {
						if p := fake.onlyRateLimit(t); p.Targets.ModelAlias != nil {
							return fmt.Errorf("target_model_alias still stored as %q", *p.Targets.ModelAlias)
						}
						return nil
					},
				),
			},
			{
				Config:           config(""),
				ConfigPlanChecks: inPlace,
				Check: func(*terraform.State) error {
					if p := fake.onlyRateLimit(t); !p.Targets.eq(fakeLlmTargets{}) {
						return fmt.Errorf("targets still stored: %+v", p.Targets)
					}
					return nil
				},
			},
			{Config: config(""), PlanOnly: true},
			{
				Config:           config(fmt.Sprintf("  target_mcp_server_id = %q\n  traffic_type = \"mcp\"", llmTargetMcpServer)),
				ConfigPlanChecks: inPlace,
				Check:            resource.TestCheckResourceAttr(name, "target_mcp_server_id", llmTargetMcpServer),
			},
			{
				ResourceName:      name,
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

// change_note is sent on every write and recorded by the platform, an update
// without one clears the platform's note, and a note left by an edit in the
// app is not drift.
func TestLlmGovernance_changeNote(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	config := func(rpm int, note string) string {
		noteLine := ""
		if note != "" {
			noteLine = fmt.Sprintf("  change_note = %q", note)
		}
		return fmt.Sprintf(`
resource "barndoor_llm_rate_limit" "test" {
  name                = "Org ceiling"
  scope_type          = "org"
  requests_per_minute = %d
%s
}

resource "barndoor_llm_token_budget" "test" {
  name        = "Org cap"
  scope_type  = "org"
  period      = "monthly"
  token_limit = %d
%s
}
`, rpm, noteLine, rpm*1000, noteLine)
	}
	notes := func(want string) resource.TestCheckFunc {
		return func(*terraform.State) error {
			for kind, got := range map[string]*string{
				"rate limit": fake.onlyRateLimit(t).LastChangeNote,
				"budget":     fake.onlyBudget(t).LastChangeNote,
			} {
				if (want == "") != (got == nil) || (got != nil && *got != want) {
					return fmt.Errorf("%s last_change_note = %v, want %q", kind, got, want)
				}
			}
			return nil
		}
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkAllLlmRateLimitsDeleted(fake), checkAllLlmBudgetsDeleted(fake)),
		Steps: []resource.TestStep{
			{Config: config(100, "Initial ceiling"), Check: notes("Initial ceiling")},
			{
				// A note recorded by someone else is not drift.
				PreConfig: func() {
					fake.mu.Lock()
					defer fake.mu.Unlock()
					other := "Edited in the app"
					fake.rateLimits[0].LastChangeNote = &other
					fake.budgets[0].LastChangeNote = &other
				},
				Config:   config(100, "Initial ceiling"),
				PlanOnly: true,
			},
			{Config: config(200, "Raise for launch"), Check: notes("Raise for launch")},
			{Config: config(300, ""), Check: notes("")},
		},
	})

	// Over-long notes fail at plan time, before anything is written.
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config:      config(300, fmt.Sprintf("%0501d", 0)),
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`character count must be at most 500`),
		}},
	})
}

// Connections and model routes record change notes the same way.
func TestLlmChangeNote_connectionAndMapping(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	config := func(note string) string {
		return fmt.Sprintf(`
resource "barndoor_llm_connection" "openai" {
  name           = "OpenAI key"
  model_provider = "openai"
  base_url       = "https://api.openai.com"
  api_key        = "sk-note"
  change_note    = %[1]q
}

resource "barndoor_llm_provider" "openai" {
  name           = "OpenAI"
  model_provider = "openai"
  connection_id  = barndoor_llm_connection.openai.id
}

resource "barndoor_llm_model_mapping" "gpt" {
  provider_id    = barndoor_llm_provider.openai.id
  model_alias    = "gpt-5.5"
  upstream_model = "gpt-5.5"
  change_note    = %[1]q
}
`, note)
	}
	check := func(want string) resource.TestCheckFunc {
		return func(*terraform.State) error {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			for _, c := range fake.connections {
				if c.LastChangeNote == nil || *c.LastChangeNote != want {
					return fmt.Errorf("connection note = %v, want %q", c.LastChangeNote, want)
				}
			}
			for _, m := range fake.mappings {
				if m.LastChangeNote == nil || *m.LastChangeNote != want {
					return fmt.Errorf("mapping note = %v, want %q", m.LastChangeNote, want)
				}
			}
			return nil
		}
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: config("Initial setup"), Check: check("Initial setup")},
			{Config: config("Initial setup"), PlanOnly: true},
			{Config: config("Rotate key"), Check: check("Rotate key")},
		},
	})
}
