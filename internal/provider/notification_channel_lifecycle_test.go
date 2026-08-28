// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"encoding/json"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

const channelResourceName = "barndoor_notification_channel.test"

func webhookChannelConfig(subs string, rotate string) string {
	rotateBlock := ""
	if rotate != "" {
		rotateBlock = fmt.Sprintf("\n  rotate_when_changed = { rotated_at = %q }\n", rotate)
	}
	return fmt.Sprintf(`
resource "barndoor_notification_channel" "test" {
  type          = "webhook"
  url           = "https://hooks.example.com/barndoor"
  subscriptions = %s
%s}
`, subs, rotateBlock)
}

// The central regression test for the signing-secret design.
//
// signing_secret is Computed and can NEVER be refreshed — the platform reveals
// it once. ModifyPlan therefore pins it from prior state on any plan that is
// not a rotation. Whether that actually satisfies Terraform core is decided by
// the framework's plan pipeline, not by reading the mapper: if the pinned value
// and the applied value disagree, core raises "Provider produced inconsistent
// result after apply". This test is what settles it, and what keeps it settled.
func TestNotificationChannelResource_secretStableAcrossUpdates(t *testing.T) {
	fake := setupChannelTest(t)

	var firstSecret string

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Create: the reveal lands in state.
				Config: webhookChannelConfig(`["break_glass_used"]`, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(channelResourceName, "type", channelTypeWebhook),
					resource.TestCheckResourceAttr(channelResourceName, "has_signing_secret", "true"),
					resource.TestCheckResourceAttrSet(channelResourceName, "signing_secret"),
					resource.TestCheckResourceAttrSet(channelResourceName, "id"),
					resource.TestCheckResourceAttr(channelResourceName, "subscriptions.#", "1"),
				),
			},
			{
				// Re-plan with no change must be empty. A refresh cannot return
				// the secret, so an unpinned Computed attribute would show a
				// perpetual "(known after apply)" diff here.
				Config:   webhookChannelConfig(`["break_glass_used"]`, ""),
				PlanOnly: true,
			},
			{
				// Capture the secret so the next step can prove it survived an
				// unrelated update.
				Config: webhookChannelConfig(`["break_glass_used"]`, ""),
				Check: func(s *terraform.State) error {
					rs, ok := s.RootModule().Resources[channelResourceName]
					if !ok {
						return fmt.Errorf("resource not in state")
					}
					firstSecret = rs.Primary.Attributes["signing_secret"]
					if firstSecret == "" {
						return fmt.Errorf("signing_secret empty after create")
					}
					return nil
				},
			},
			{
				// A non-rotating update: subscriptions change, secret must not.
				Config: webhookChannelConfig(`["break_glass_used", "policy_changed"]`, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(channelResourceName, plancheck.ResourceActionUpdate),
						// The assertion that actually exercises ModifyPlan's pin.
						// Without it, a Computed attribute plans as unknown —
						// "(known after apply)" on a sensitive value during an
						// unrelated edit, and unreferenceable in the same plan.
						plancheck.ExpectKnownValue(channelResourceName,
							tfjsonpath.New("signing_secret"), knownvalue.NotNull()),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(channelResourceName, "subscriptions.#", "2"),
					func(s *terraform.State) error {
						rs := s.RootModule().Resources[channelResourceName]
						if got := rs.Primary.Attributes["signing_secret"]; got != firstSecret {
							return fmt.Errorf("signing_secret changed on a non-rotating update: %q -> %q", firstSecret, got)
						}
						return nil
					},
				),
			},
			{
				Config:   webhookChannelConfig(`["break_glass_used", "policy_changed"]`, ""),
				PlanOnly: true,
			},
		},
	})

	if got := fake.rotations(); got != 0 {
		t.Errorf("regenerate-secret called %d times with no rotation configured; want 0", got)
	}
}

// Rotation must be an in-place UPDATE that changes the secret — not a
// replacement. Every reference provider (google_service_account_key,
// aws_iam_access_key, azuread_application_password) must destroy and recreate
// to rotate; this API has a dedicated rotate endpoint, so the channel id must
// survive.
func TestNotificationChannelResource_rotateInPlace(t *testing.T) {
	fake := setupChannelTest(t)

	var beforeID, beforeSecret string

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: webhookChannelConfig(`["break_glass_used"]`, "2026-08-01"),
				Check: func(s *terraform.State) error {
					rs := s.RootModule().Resources[channelResourceName]
					beforeID = rs.Primary.Attributes["id"]
					beforeSecret = rs.Primary.Attributes["signing_secret"]
					if beforeID == "" || beforeSecret == "" {
						return fmt.Errorf("id/secret empty after create")
					}
					return nil
				},
			},
			{
				// Bump the keeper: rotate.
				Config: webhookChannelConfig(`["break_glass_used"]`, "2026-09-01"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						// In place — NOT a replacement.
						plancheck.ExpectResourceAction(channelResourceName, plancheck.ResourceActionUpdate),
						// A rotation genuinely cannot know the new secret until
						// apply, so here it MUST plan as unknown. Paired with the
						// known-value assertion on the non-rotating update, this
						// pins both branches of ModifyPlan.
						plancheck.ExpectUnknownValue(channelResourceName,
							tfjsonpath.New("signing_secret")),
					},
				},
				Check: func(s *terraform.State) error {
					rs := s.RootModule().Resources[channelResourceName]
					if got := rs.Primary.Attributes["id"]; got != beforeID {
						return fmt.Errorf("rotation replaced the channel: id %q -> %q", beforeID, got)
					}
					if got := rs.Primary.Attributes["signing_secret"]; got == beforeSecret {
						return fmt.Errorf("rotation did not change the secret (still %q)", got)
					}
					return nil
				},
			},
			{
				Config:   webhookChannelConfig(`["break_glass_used"]`, "2026-09-01"),
				PlanOnly: true,
			},
		},
	})

	if got := fake.rotations(); got != 1 {
		t.Errorf("regenerate-secret called %d times; want exactly 1", got)
	}
}

// Changing `type` has no in-place transition server-side, so it must force a
// replacement rather than attempting an impossible update.
func TestNotificationChannelResource_typeChangeForcesReplace(t *testing.T) {
	setupChannelTest(t)

	emailConfig := `
resource "barndoor_notification_channel" "test" {
  type          = "email"
  email_address = "ops@example.com"
  subscriptions = ["connection_broken"]
}
`

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: webhookChannelConfig(`["break_glass_used"]`, ""),
			},
			{
				Config: emailConfig,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(channelResourceName, plancheck.ResourceActionDestroyBeforeCreate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(channelResourceName, "type", channelTypeEmail),
					resource.TestCheckResourceAttr(channelResourceName, "email_address", "ops@example.com"),
					// An email channel has no signing secret.
					resource.TestCheckResourceAttr(channelResourceName, "has_signing_secret", "false"),
				),
			},
		},
	})
}

// The wire body must carry only the fields belonging to the type, and must
// always carry `subscriptions` — the fake rejects both violations exactly as
// the API does, so a regression fails the apply rather than passing silently.
func TestNotificationChannelResource_wireBodyShape(t *testing.T) {
	fake := setupChannelTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "barndoor_notification_channel" "test" {
  type          = "email"
  email_address = "ops@example.com"
  subscriptions = []
}
`,
			},
		},
	})

	body := fake.lastPutBody(t)
	for _, forbidden := range []string{"url", "label", "slack_channel_id", "teams_workflow_url"} {
		if _, present := body[forbidden]; present {
			t.Errorf("%q must not be sent for an email channel", forbidden)
		}
	}
	raw, present := body["subscriptions"]
	if !present {
		t.Fatal("subscriptions must always be sent")
	}
	var subs []any
	if err := json.Unmarshal(raw, &subs); err != nil {
		t.Fatalf("subscriptions not a list: %v", err)
	}
	if len(subs) != 0 {
		t.Errorf("subscriptions = %v, want an empty list", subs)
	}
}

// A create whose destination already exists hits the API's upsert-dedup path
// and returns the existing channel with NO secret. Adopting it silently would
// leave the practitioner without a secret they may depend on, so it must be an
// actionable error directing them to import.
func TestNotificationChannelResource_existingChannelSurfacesImportError(t *testing.T) {
	fake := setupChannelTest(t)

	// Pre-seed a webhook channel on the same URL.
	url := "https://hooks.example.com/barndoor"
	fake.channels["chan-existing"] = &notificationChannelResponse{
		ID: "chan-existing", Type: channelTypeWebhook, Enabled: true,
		URL: &url, HasSigningSecret: true,
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      webhookChannelConfig(`["break_glass_used"]`, ""),
				ExpectError: regexp.MustCompile(`already exists|terraform import`),
			},
		},
	})
}

// Deleting out-of-band must be detected on refresh and re-created, not error.
func TestNotificationChannelResource_recreatesAfterOutOfBandDelete(t *testing.T) {
	fake := setupChannelTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: webhookChannelConfig(`["break_glass_used"]`, ""),
			},
			{
				PreConfig: func() {
					fake.mu.Lock()
					defer fake.mu.Unlock()
					for id := range fake.channels {
						delete(fake.channels, id)
					}
				},
				Config: webhookChannelConfig(`["break_glass_used"]`, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(channelResourceName, plancheck.ResourceActionCreate),
					},
				},
				Check: resource.TestCheckResourceAttrSet(channelResourceName, "signing_secret"),
			},
		},
	})
}
