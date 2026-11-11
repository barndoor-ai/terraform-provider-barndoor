// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// reasoning_effort_support: `auto` / `supported` / `unsupported` over the
// API's nullable bool, on enablement (kind = "model") rows only.

// reasoningEffortModel is the enablement every test here manages: a name the
// gateway's detection does not recognise, like a self-hosted model.
const reasoningEffortModel = "internal-r1"

func reasoningEffortConfig(providerID, value string) string {
	attr := ""
	if value != "" {
		attr = fmt.Sprintf("reasoning_effort_support = %q", value)
	}
	return fmt.Sprintf(`
resource "barndoor_llm_model_mapping" "enable" {
  provider_id    = %q
  model_alias    = %[2]q
  upstream_model = %[2]q
  %[3]s
}
`, providerID, reasoningEffortModel, attr)
}

// fakeReasoningEffort is the fake's stored value for the enablement of
// upstream, as the attribute value it should read back as.
func fakeReasoningEffort(fake *fakeLlmGatewayServer, upstream string) string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, m := range fake.mappings {
		if m.Kind == "model" && m.UpstreamModel == upstream {
			return reasoningEffortValue(m.ReasoningEffortSupport)
		}
	}
	return "<missing>"
}

func checkFakeReasoningEffort(fake *fakeLlmGatewayServer, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if got := fakeReasoningEffort(fake, reasoningEffortModel); got != want {
			return fmt.Errorf("platform reasoning_effort_support for %s = %q, want %q", reasoningEffortModel, got, want)
		}
		return nil
	}
}

// setFakeReasoningEffort changes the stored value out-of-band, as an admin in
// the Barndoor app would.
func setFakeReasoningEffort(t *testing.T, fake *fakeLlmGatewayServer, upstream string, v *bool) {
	t.Helper()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, m := range fake.mappings {
		if m.Kind == "model" && m.UpstreamModel == upstream {
			m.ReasoningEffortSupport = v
			return
		}
	}
	t.Fatalf("fake has no enablement for %q", upstream)
}

func TestLlmModelMappingResource_reasoningEffortLifecycle(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	provider := fake.seedProvider()
	const name = "barndoor_llm_model_mapping.enable"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmMappingsDeleted(fake),
		Steps: []resource.TestStep{
			{
				// Set on create, in the create call itself.
				Config: reasoningEffortConfig(provider.ID, "supported"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(name, "reasoning_effort_support", "supported"),
					checkFakeReasoningEffort(fake, "supported"),
				),
			},
			{
				Config:   reasoningEffortConfig(provider.ID, "supported"),
				PlanOnly: true,
			},
			{
				Config: reasoningEffortConfig(provider.ID, "unsupported"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(name, "reasoning_effort_support", "unsupported"),
					checkFakeReasoningEffort(fake, "unsupported"),
				),
			},
			{
				// `auto` sends null, returning the model to detection.
				Config: reasoningEffortConfig(provider.ID, "auto"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(name, "reasoning_effort_support", "auto"),
					checkFakeReasoningEffort(fake, "auto"),
				),
			},
			{
				// Removing the attribute stops managing it: no diff.
				Config:   reasoningEffortConfig(provider.ID, ""),
				PlanOnly: true,
			},
		},
	})
}

// Unset means "not managed": a value an admin sets in the Barndoor app is
// read back and kept, never reverted by the next apply.
func TestLlmModelMappingResource_reasoningEffortUnsetKeepsAppValue(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	provider := fake.seedProvider()
	const name = "barndoor_llm_model_mapping.enable"
	supported := true
	config := reasoningEffortConfig(provider.ID, "")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmMappingsDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  resource.TestCheckResourceAttr(name, "reasoning_effort_support", "auto"),
			},
			{
				PreConfig: func() { setFakeReasoningEffort(t, fake, "internal-r1", &supported) },
				Config:    config,
				PlanOnly:  true,
			},
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(name, "reasoning_effort_support", "supported"),
					checkFakeReasoningEffort(fake, "supported"),
				),
			},
		},
	})
}

// A create that folds into an existing enablement keeps its stored setting
// unless told otherwise, and the create endpoint cannot clear it: `auto` has
// to be applied with the update endpoint, without losing change_note.
func TestLlmModelMappingResource_reasoningEffortAutoOnFoldClears(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	provider := fake.seedProvider()
	const name = "barndoor_llm_model_mapping.enable"
	supported := true
	fake.mu.Lock()
	fake.mappings = append(fake.mappings, &fakeLlmModelMapping{
		Kind: "model", ID: fake.newID("bbbb"), ProviderID: provider.ID,
		ModelAlias: "internal-r1", UpstreamModel: "internal-r1", Enabled: true,
		StreamIdleTimeoutSecs: fakeLlmStreamIdleDefault, RequestTimeoutSecs: fakeLlmRequestDefault,
		ReasoningEffortSupport: &supported, Cooldown: fakeLlmCooldownDefaults,
	})
	fake.mu.Unlock()

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmMappingsDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "barndoor_llm_model_mapping" "enable" {
  provider_id              = %q
  model_alias              = "internal-r1"
  upstream_model           = "internal-r1"
  reasoning_effort_support = "auto"
  change_note              = "adopt the self-hosted model"
}
`, provider.ID),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(name, "reasoning_effort_support", "auto"),
					checkFakeReasoningEffort(fake, "auto"),
					func(*terraform.State) error {
						fake.mu.Lock()
						defer fake.mu.Unlock()
						for _, m := range fake.mappings {
							if m.UpstreamModel == "internal-r1" {
								if m.LastChangeNote == nil || *m.LastChangeNote != "adopt the self-hosted model" {
									return fmt.Errorf("change_note lost by the reconcile write: %v", m.LastChangeNote)
								}
								return nil
							}
						}
						return fmt.Errorf("enablement missing")
					},
				),
			},
		},
	})
}

func TestLlmModelMappingResource_reasoningEffortRejectedOnRoute(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	provider := fake.seedProvider()

	for _, tc := range []struct{ name, extra string }{
		{"custom alias", `model_alias = "fast"`},
		{"route named after the model", "model_alias = \"internal-r1\"\n  kind = \"route\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{
					{
						Config: fmt.Sprintf(`
resource "barndoor_llm_model_mapping" "route" {
  provider_id              = %q
  upstream_model           = "internal-r1"
  %s
  reasoning_effort_support = "supported"
}
`, provider.ID, tc.extra),
						PlanOnly:    true,
						ExpectError: regexp.MustCompile(`reasoning_effort_support needs kind = "model"`),
					},
				},
			})
		})
	}
}

// A platform release that predates the field ignores it; that must surface as
// a clear error, not Terraform's generic inconsistent-result one.
func TestLlmModelMappingResource_reasoningEffortOnOlderPlatform(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	fake.predatesReasoningEffort = true
	provider := fake.seedProvider()

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      reasoningEffortConfig(provider.ID, "supported"),
				ExpectError: regexp.MustCompile(`may not support reasoning_effort_support`),
			},
		},
	})
}

func TestReasoningEffortWireValues(t *testing.T) {
	for value, want := range map[string]string{"auto": "null", "supported": "true", "unsupported": "false"} {
		got := string(reasoningEffortWire(types.StringValue(value)))
		if got != want {
			t.Errorf("update wire for %q = %s, want %s", value, got, want)
		}
		if back := reasoningEffortValue(reasoningEffortBool(value)); back != value {
			t.Errorf("round trip %q = %q", value, back)
		}
	}
	if got := reasoningEffortWire(types.StringUnknown()); got != nil {
		t.Errorf("unknown must be omitted, got %s", got)
	}
	if got := reasoningEffortCreateWire(types.StringValue("auto")); got != nil {
		t.Errorf("create must omit auto, got %v", *got)
	}
}
