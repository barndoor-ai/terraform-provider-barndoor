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

func (f *fakeLlmGatewayServer) onlyProvider(t *testing.T) fakeLlmProvider {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.providers) != 1 {
		t.Fatalf("fake holds %d providers, want 1", len(f.providers))
	}
	return *f.providers[0]
}

// The provider-level timeouts are overrides: set in place, and removing one
// sends an explicit null that clears it.
func TestLlmProviderResource_timeouts(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const name = "barndoor_llm_provider.test"
	config := func(extra string) string {
		return llmConnectionHCL("openai", "openai", "sk-test") + fmt.Sprintf(`
resource "barndoor_llm_provider" "test" {
  name           = "OpenAI"
  model_provider = "openai"
  connection_id  = barndoor_llm_connection.openai.id
%s
}
`, extra)
	}
	inPlace := resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
		plancheck.ExpectResourceAction(name, plancheck.ResourceActionUpdate),
	}}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config(""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(name, "request_timeout_secs"),
					resource.TestCheckResourceAttr(name, "model_sync_mode", "off"),
					resource.TestCheckNoResourceAttr(name, "catalog_id"),
				),
			},
			{
				Config:           config("  request_timeout_secs     = 300\n  stream_idle_timeout_secs = 240"),
				ConfigPlanChecks: inPlace,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(name, "request_timeout_secs", "300"),
					resource.TestCheckResourceAttr(name, "stream_idle_timeout_secs", "240"),
				),
			},
			{Config: config("  request_timeout_secs     = 300\n  stream_idle_timeout_secs = 240"), PlanOnly: true},
			{
				Config:           config("  stream_idle_timeout_secs = 240"),
				ConfigPlanChecks: inPlace,
				Check: func(*terraform.State) error {
					if p := fake.onlyProvider(t); p.RequestTimeout != nil {
						return fmt.Errorf("request_timeout_secs still stored as %d", *p.RequestTimeout)
					}
					return nil
				},
			},
			{
				ResourceName:            name,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"change_note"},
			},
		},
	})
}

// A provider created from a catalog entry takes the entry's endpoint when
// nothing else sets one, can sync its routes from the catalog, and cannot
// change entries in place.
func TestLlmProviderResource_catalog(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const name = "barndoor_llm_provider.test"
	config := func(syncMode string) string {
		return fmt.Sprintf(`
resource "barndoor_llm_provider" "test" {
  name            = "Claude (subscription)"
  model_provider  = "anthropic"
  auth_type       = "claude_oauth"
  catalog_id      = %q
  model_sync_mode = %q
  change_note     = "From the catalog"
}
`, fakeLlmCatalogID, syncMode)
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmProvidersDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: config("additive"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(name, "catalog_id", fakeLlmCatalogID),
					resource.TestCheckResourceAttr(name, "base_url", fakeLlmCatalogBaseURL),
					resource.TestCheckResourceAttr(name, "model_sync_mode", "additive"),
					func(*terraform.State) error {
						if p := fake.onlyProvider(t); p.LastChangeNote == nil || *p.LastChangeNote != "From the catalog" {
							return fmt.Errorf("change_note did not reach the platform: %v", p.LastChangeNote)
						}
						return nil
					},
				),
			},
			{Config: config("additive"), PlanOnly: true},
			{
				Config: config("full"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(name, plancheck.ResourceActionUpdate),
				}},
				Check: resource.TestCheckResourceAttr(name, "model_sync_mode", "full"),
			},
			{
				// An imported catalog provider keeps its catalog_id without the
				// configuration naming it, and may sync.
				ResourceName:            name,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"change_note"},
			},
		},
	})
}

func TestLlmProviderResource_catalogRejected(t *testing.T) {
	t.Run("sync without a catalog entry", func(t *testing.T) {
		fake := setupLlmGatewayTest(t)
		resource.UnitTest(t, resource.TestCase{
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
			Steps: []resource.TestStep{{
				Config: llmConnectionHCL("openai", "openai", "sk-test") + `
resource "barndoor_llm_provider" "test" {
  name            = "OpenAI"
  model_provider  = "openai"
  connection_id   = barndoor_llm_connection.openai.id
  model_sync_mode = "additive"
}
`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`model_sync_mode requires catalog_id`),
			}},
		})
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if len(fake.providers) != 0 {
			t.Fatal("a rejected configuration reached the API")
		}
	})
	t.Run("timeout out of range", func(t *testing.T) {
		setupLlmGatewayTest(t)
		resource.UnitTest(t, resource.TestCase{
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
			Steps: []resource.TestStep{{
				Config: `
resource "barndoor_llm_provider" "test" {
  name                     = "Claude"
  model_provider           = "anthropic"
  auth_type                = "claude_oauth"
  base_url                 = "https://api.anthropic.com"
  stream_idle_timeout_secs = 301
}
`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`stream_idle_timeout_secs value must be between 1 and 300`),
			}},
		})
	})
	t.Run("unknown catalog entry", func(t *testing.T) {
		setupLlmGatewayTest(t)
		resource.UnitTest(t, resource.TestCase{
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
			Steps: []resource.TestStep{{
				Config: `
resource "barndoor_llm_provider" "test" {
  name           = "Claude"
  model_provider = "anthropic"
  auth_type      = "claude_oauth"
  catalog_id     = "c0c0c0c0-0000-0000-0000-0000000000ff"
}
`,
				ExpectError: regexp.MustCompile(`providers_catalog_id_fkey`),
			}},
		})
	})
}
