// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"regexp"
	"testing"
	"time"

	frameworkresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

const llmAPIKeyResourceName = "barndoor_llm_api_key.svc"

func TestLlmAPIKeyResource_Schema(t *testing.T) {
	r := NewLlmAPIKeyResource()
	var meta frameworkresource.MetadataResponse
	r.Metadata(context.Background(), frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &meta)
	if meta.TypeName != "barndoor_llm_api_key" {
		t.Errorf("TypeName = %q", meta.TypeName)
	}
	var resp frameworkresource.SchemaResponse
	r.Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}
	if !resp.Schema.Attributes["key"].IsSensitive() {
		t.Error("key must be Sensitive")
	}
	if !resp.Schema.Attributes["name"].IsRequired() {
		t.Error("name must be Required")
	}
}

// future returns an RFC 3339 timestamp days from now, written with a +02:00
// offset so it never matches the fake's UTC spelling byte for byte.
func future(days int) string {
	loc := time.FixedZone("", 2*60*60)
	return time.Now().Add(time.Duration(days) * 24 * time.Hour).In(loc).Truncate(time.Second).Format(time.RFC3339)
}

func llmAPIKeyConfig(body string) string {
	return fmt.Sprintf(`
resource "barndoor_llm_api_key" "svc" {
%s
}
`, body)
}

// fakeKey returns the fake's copy of the key in state.
func (f *fakeLlmGatewayServer) keyFor(s *terraform.State) (*fakeLlmAPIKey, error) {
	rs, ok := s.RootModule().Resources[llmAPIKeyResourceName]
	if !ok {
		return nil, fmt.Errorf("%s not in state", llmAPIKeyResourceName)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if k := f.findAPIKey(rs.Primary.ID); k != nil {
		return k, nil
	}
	return nil, fmt.Errorf("key %s unknown to the fake", rs.Primary.ID)
}

// checkStateSecretIsLive asserts state holds the key's current secret.
func (f *fakeLlmGatewayServer) checkStateSecretIsLive() resource.TestCheckFunc {
	return func(s *terraform.State) error {
		k, err := f.keyFor(s)
		if err != nil {
			return err
		}
		if got := s.RootModule().Resources[llmAPIKeyResourceName].Primary.Attributes["key"]; got != k.Secret {
			return fmt.Errorf("state key %q is not the live secret %q", got, k.Secret)
		}
		return nil
	}
}

func checkAllAPIKeysRevoked(f *fakeLlmGatewayServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, k := range f.apiKeys {
			if k.RevokedAt == nil {
				return fmt.Errorf("key %s (%s) is still live after destroy", k.ID, k.Name)
			}
		}
		return nil
	}
}

func TestLlmAPIKeyResource_lifecycle(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	expiry1, expiry2 := future(30), future(90)

	var id, firstKey, rotatedKey string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllAPIKeysRevoked(fake),
		Steps: []resource.TestStep{
			{
				// A group-bound service key with an expiry.
				Config: llmAPIKeyConfig(`
  name       = "billing-service"
  group_name = "finance"
  expires_at = "` + expiry1 + `"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(llmAPIKeyResourceName, "key"),
					resource.TestCheckResourceAttr(llmAPIKeyResourceName, "kind", "machine"),
					resource.TestCheckResourceAttr(llmAPIKeyResourceName, "expires_at", expiry1),
					resource.TestCheckResourceAttr(llmAPIKeyResourceName, "disabled", "false"),
					resource.TestCheckResourceAttr(llmAPIKeyResourceName, "scopes.#", "0"),
					resource.TestCheckNoResourceAttr(llmAPIKeyResourceName, "rotated_at"),
					fake.checkStateSecretIsLive(),
					func(s *terraform.State) error {
						a := s.RootModule().Resources[llmAPIKeyResourceName].Primary.Attributes
						id, firstKey = a["id"], a["key"]
						if a["key_prefix"] != firstKey[:8] {
							return fmt.Errorf("key_prefix %q does not prefix the key", a["key_prefix"])
						}
						return nil
					},
				),
			},
			{
				// The platform answers expires_at in UTC with microseconds; the
				// same instant must not plan a change.
				Config: llmAPIKeyConfig(`
  name       = "billing-service"
  group_name = "finance"
  expires_at = "` + expiry1 + `"`),
				PlanOnly: true,
			},
			{
				// Extend and disable in place: same key, same secret.
				Config: llmAPIKeyConfig(`
  name       = "billing-service"
  group_name = "finance"
  expires_at = "` + expiry2 + `"
  disabled   = true`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(llmAPIKeyResourceName, plancheck.ResourceActionUpdate),
					plancheck.ExpectKnownValue(llmAPIKeyResourceName, tfjsonpath.New("key"), knownStringValue{want: &firstKey}),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(llmAPIKeyResourceName, "id", &id),
					resource.TestCheckResourceAttrPtr(llmAPIKeyResourceName, "key", &firstKey),
					resource.TestCheckResourceAttr(llmAPIKeyResourceName, "disabled", "true"),
					func(s *terraform.State) error {
						k, err := fake.keyFor(s)
						if err != nil {
							return err
						}
						want, _ := time.Parse(time.RFC3339, expiry2)
						if k.ExpiresAt == nil || !k.ExpiresAt.Equal(want) || k.DisabledAt == nil {
							return fmt.Errorf("platform has expires_at %v, disabled_at %v", k.ExpiresAt, k.DisabledAt)
						}
						return nil
					},
				),
			},
			{
				// Rotate with a grace period, and drop the expiry in the same apply.
				Config: llmAPIKeyConfig(`
  name              = "billing-service"
  group_name        = "finance"
  disabled          = true
  rotation_trigger  = { rotated = "2026-11" }
  grace_period_secs = 3600`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(llmAPIKeyResourceName, plancheck.ResourceActionUpdate),
					plancheck.ExpectUnknownValue(llmAPIKeyResourceName, tfjsonpath.New("key")),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(llmAPIKeyResourceName, "id", &id),
					resource.TestCheckNoResourceAttr(llmAPIKeyResourceName, "expires_at"),
					resource.TestCheckResourceAttrSet(llmAPIKeyResourceName, "rotated_at"),
					resource.TestCheckResourceAttrSet(llmAPIKeyResourceName, "previous_key_expires_at"),
					fake.checkStateSecretIsLive(),
					func(s *terraform.State) error {
						rotatedKey = s.RootModule().Resources[llmAPIKeyResourceName].Primary.Attributes["key"]
						if rotatedKey == firstKey {
							return fmt.Errorf("rotation kept the old secret")
						}
						k, _ := fake.keyFor(s)
						if k.PreviousSecret != firstKey || k.ExpiresAt != nil {
							return fmt.Errorf("want the old secret parked and no expiry; previous %q, expires %v",
								k.PreviousSecret, k.ExpiresAt)
						}
						return nil
					},
				),
			},
			{
				Config: llmAPIKeyConfig(`
  name              = "billing-service"
  group_name        = "finance"
  disabled          = true
  rotation_trigger  = { rotated = "2026-11" }
  grace_period_secs = 3600`),
				PlanOnly: true,
			},
			{
				// Removing the trigger doesn't rotate; re-enabling is in place.
				Config: llmAPIKeyConfig(`
  name       = "billing-service"
  group_name = "finance"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(llmAPIKeyResourceName, "key", &rotatedKey),
					resource.TestCheckResourceAttr(llmAPIKeyResourceName, "disabled", "false"),
					fake.checkStateSecretIsLive(),
				),
			},
			{
				ResourceName:            llmAPIKeyResourceName,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"key"}, // never returned again
			},
			{
				// A new name is a new key; the old one is revoked.
				Config: llmAPIKeyConfig(`
  name       = "billing-service-v2"
  group_name = "finance"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(llmAPIKeyResourceName, plancheck.ResourceActionDestroyBeforeCreate),
				}},
				Check: func(s *terraform.State) error {
					if s.RootModule().Resources[llmAPIKeyResourceName].Primary.ID == id {
						return fmt.Errorf("rename kept the key id")
					}
					fake.mu.Lock()
					defer fake.mu.Unlock()
					if old := fake.findAPIKey(id); old == nil || old.RevokedAt == nil {
						return fmt.Errorf("the replaced key was not revoked")
					}
					return nil
				},
			},
		},
	})
}

func TestLlmAPIKeyResource_userKeyCreatedDisabled(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	config := llmAPIKeyConfig(`
  name     = "laptop"
  user_id  = "AAAAAAAA-0000-0000-0000-000000000001"
  scopes   = ["chat"]
  disabled = true`)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllAPIKeysRevoked(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(llmAPIKeyResourceName, "kind", "user"),
					resource.TestCheckResourceAttr(llmAPIKeyResourceName, "user_id", "AAAAAAAA-0000-0000-0000-000000000001"),
					resource.TestCheckResourceAttr(llmAPIKeyResourceName, "scopes.0", "chat"),
					resource.TestCheckResourceAttr(llmAPIKeyResourceName, "disabled", "true"),
					func(s *terraform.State) error {
						k, err := fake.keyFor(s)
						if err != nil {
							return err
						}
						if k.DisabledAt == nil {
							return fmt.Errorf("the key was not disabled on the platform")
						}
						return nil
					},
				),
			},
			// The platform answers the user id in lowercase.
			{Config: config, PlanOnly: true},
		},
	})
}

// Changes made in the app show as drift and the next apply puts them back,
// without touching the secret.
func TestLlmAPIKeyResource_outOfBandChanges(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	expiry := future(30)
	config := llmAPIKeyConfig(`
  name       = "ci"
  expires_at = "` + expiry + `"`)

	var firstID, secret string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: func(s *terraform.State) error {
					a := s.RootModule().Resources[llmAPIKeyResourceName].Primary.Attributes
					firstID, secret = a["id"], a["key"]
					return nil
				},
			},
			{
				PreConfig: func() {
					fake.mu.Lock()
					defer fake.mu.Unlock()
					k := fake.findAPIKey(firstID)
					k.ExpiresAt = nil
					now := time.Now()
					k.DisabledAt = &now
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(llmAPIKeyResourceName, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(llmAPIKeyResourceName, "key", &secret),
					func(s *terraform.State) error {
						k, _ := fake.keyFor(s)
						if k.ExpiresAt == nil || k.DisabledAt != nil {
							return fmt.Errorf("drift not corrected: expires %v, disabled %v", k.ExpiresAt, k.DisabledAt)
						}
						return nil
					},
				),
			},
			{
				// Revoked in the app: the next plan creates a new key.
				PreConfig: func() {
					fake.mu.Lock()
					defer fake.mu.Unlock()
					now := time.Now()
					fake.findAPIKey(firstID).RevokedAt = &now
				},
				Config: config,
				Check: func(s *terraform.State) error {
					if s.RootModule().Resources[llmAPIKeyResourceName].Primary.ID == firstID {
						return fmt.Errorf("a revoked key must be replaced, still %s", firstID)
					}
					return nil
				},
			},
		},
	})
}

// Renewing an expired key with a grace period must not revive its dead secret.
func TestLlmAPIKeyResource_rotatingAnExpiredKeyParksNothing(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: llmAPIKeyConfig(`  name = "old"`)},
			{
				PreConfig: func() {
					fake.mu.Lock()
					defer fake.mu.Unlock()
					past := time.Now().Add(-time.Hour)
					fake.apiKeys[0].ExpiresAt = &past
				},
				Config: llmAPIKeyConfig(`
  name              = "old"
  rotation_trigger  = { n = "1" }
  grace_period_secs = 86400`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(llmAPIKeyResourceName, "previous_key_expires_at"),
					resource.TestCheckNoResourceAttr(llmAPIKeyResourceName, "expires_at"),
				),
			},
		},
	})
}

func TestLlmAPIKeyResource_rejections(t *testing.T) {
	cases := []struct {
		name, body string
		want       *regexp.Regexp
		planOnly   bool
	}{
		{"user and group", `
  name       = "x"
  user_id    = "aaaaaaaa-0000-0000-0000-000000000001"
  group_name = "eng"`, regexp.MustCompile(`(?s)"group_name".*cannot be configured together|(?s)These attributes cannot be configured together`), true},
		{"bad timestamp", `
  name       = "x"
  expires_at = "next tuesday"`, regexp.MustCompile(`must be an RFC 3339 timestamp`), true},
		{"grace too long", `
  name              = "x"
  grace_period_secs = 604801`, regexp.MustCompile(`(?s)grace_period_secs.*between 0 and 604800`), true},
		{"past expiry", `
  name       = "x"
  expires_at = "2020-01-01T00:00:00Z"`, regexp.MustCompile(`expires_at must be in the future`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupLlmGatewayTest(t)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{{
					Config:      llmAPIKeyConfig(tc.body),
					PlanOnly:    tc.planOnly,
					ExpectError: tc.want,
				}},
			})
		})
	}
}

func TestLlmAPIKeyResource_duplicateNameIs409(t *testing.T) {
	setupLlmGatewayTest(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "barndoor_llm_api_key" "a" {
  name       = "shared"
  group_name = "eng"
}
resource "barndoor_llm_api_key" "b" {
  name       = "shared"
  group_name = "eng"
  depends_on = [barndoor_llm_api_key.a]
}
`,
			ExpectError: regexp.MustCompile(`An API key named 'shared' already exists for this group`),
		}},
	})
}

// knownStringValue is a knownvalue.Check against a string a previous step
// captured: it reads *want when the check runs, not when the test is built.
type knownStringValue struct{ want *string }

func (v knownStringValue) CheckValue(other any) error {
	got, ok := other.(string)
	if !ok {
		return fmt.Errorf("expected a string value, got %T", other)
	}
	if got != *v.want {
		return fmt.Errorf("expected %q, got %q", *v.want, got)
	}
	return nil
}

func (v knownStringValue) String() string { return *v.want }
