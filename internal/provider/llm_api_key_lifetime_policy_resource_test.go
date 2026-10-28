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

func TestLlmAPIKeyLifetimeResources_MetadataAndSchema(t *testing.T) {
	cases := []struct {
		new      func() frameworkresource.Resource
		typeName string
		subject  string // "" for the default
	}{
		{NewLlmAPIKeyLifetimePolicyResource, "barndoor_llm_api_key_lifetime_policy", ""},
		{NewLlmAPIKeyLifetimeGroupOverrideResource, "barndoor_llm_api_key_lifetime_group_override", "group_name"},
		{NewLlmAPIKeyLifetimeUserOverrideResource, "barndoor_llm_api_key_lifetime_user_override", "user_id"},
	}
	for _, tc := range cases {
		t.Run(tc.typeName, func(t *testing.T) {
			r := tc.new()
			var meta frameworkresource.MetadataResponse
			r.Metadata(context.Background(), frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &meta)
			if meta.TypeName != tc.typeName {
				t.Errorf("TypeName = %q, want %q", meta.TypeName, tc.typeName)
			}

			var resp frameworkresource.SchemaResponse
			r.Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
			}
			if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
				t.Fatalf("schema failed framework validation: %+v", diags)
			}
			for _, attr := range []string{"id", "max_lifetime_days", "require_expiry", "fixed_lifetime"} {
				if _, ok := resp.Schema.Attributes[attr]; !ok {
					t.Errorf("schema missing attribute %q", attr)
				}
			}
			for _, computed := range []string{"id", "require_expiry", "fixed_lifetime"} {
				if !resp.Schema.Attributes[computed].IsComputed() {
					t.Errorf("%s should be Computed", computed)
				}
			}
			if tc.subject != "" && !resp.Schema.Attributes[tc.subject].IsRequired() {
				t.Errorf("%s should be Required", tc.subject)
			}
		})
	}
}

// --- default policy ---------------------------------------------------------------

func TestLlmAPIKeyLifetimePolicyResource_lifecycle(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_api_key_lifetime_policy.org"

	capConfig := `
resource "barndoor_llm_api_key_lifetime_policy" "org" {
  max_lifetime_days = 90
}
`
	fixedConfig := `
resource "barndoor_llm_api_key_lifetime_policy" "org" {
  max_lifetime_days = 60
  fixed_lifetime    = true
}
`
	requireOnlyConfig := `
resource "barndoor_llm_api_key_lifetime_policy" "org" {
  require_expiry = true
}
`

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkLlmLifetimePoliciesCleared(fake),
		Steps: []resource.TestStep{
			{
				Config: capConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "id", fakeLlmOrgID),
					resource.TestCheckResourceAttr(resourceName, "max_lifetime_days", "90"),
					resource.TestCheckResourceAttr(resourceName, "require_expiry", "false"),
					resource.TestCheckResourceAttr(resourceName, "fixed_lifetime", "false"),
					fake.checkLifetimeDefault(ptrInt32(90), false, false),
				),
			},
			{Config: capConfig, PlanOnly: true},
			{
				Config: fixedConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "max_lifetime_days", "60"),
					resource.TestCheckResourceAttr(resourceName, "fixed_lifetime", "true"),
					fake.checkLifetimeDefault(ptrInt32(60), false, true),
				),
			},
			{
				// Dropping the maximum must clear it on the platform, not keep it.
				Config: requireOnlyConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(resourceName, "max_lifetime_days"),
					resource.TestCheckResourceAttr(resourceName, "require_expiry", "true"),
					resource.TestCheckResourceAttr(resourceName, "fixed_lifetime", "false"),
					fake.checkLifetimeDefault(nil, true, false),
				),
			},
			{Config: requireOnlyConfig, PlanOnly: true},
			{
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateId:     fakeLlmOrgID,
				ImportStateVerify: true,
			},
		},
	})
}

// A default cleared in the app shows as drift and the next apply restores it.
func TestLlmAPIKeyLifetimePolicyResource_outOfBandClearIsDrift(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	config := `
resource "barndoor_llm_api_key_lifetime_policy" "org" {
  max_lifetime_days = 30
}
`
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig: func() {
					fake.mu.Lock()
					fake.lifetimeDefault = nil
					fake.mu.Unlock()
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("barndoor_llm_api_key_lifetime_policy.org", plancheck.ResourceActionUpdate),
					},
				},
				Check: fake.checkLifetimeDefault(ptrInt32(30), false, false),
			},
		},
	})
}

func TestLlmAPIKeyLifetimeResources_fixedNeedsMaxAtPlanTime(t *testing.T) {
	setupLlmGatewayTest(t)
	for _, config := range []string{
		`resource "barndoor_llm_api_key_lifetime_policy" "org" {
  fixed_lifetime = true
}`,
		`resource "barndoor_llm_api_key_lifetime_group_override" "eng" {
  group_name     = "eng"
  fixed_lifetime = true
}`,
		`resource "barndoor_llm_api_key_lifetime_user_override" "ada" {
  user_id        = "aaaaaaaa-0000-0000-0000-000000000001"
  fixed_lifetime = true
}`,
	} {
		resource.UnitTest(t, resource.TestCase{
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
			Steps: []resource.TestStep{{
				Config:      config,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`fixed_lifetime needs max_lifetime_days`),
			}},
		})
	}
}

func TestLlmAPIKeyLifetimeResources_maxRangeAtPlanTime(t *testing.T) {
	setupLlmGatewayTest(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `resource "barndoor_llm_api_key_lifetime_policy" "org" {
  max_lifetime_days = 3651
}`,
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`(?s)max_lifetime_days.*between 1 and 3650`),
		}},
	})
}

// --- group overrides ----------------------------------------------------------------

func TestLlmAPIKeyLifetimeGroupOverrideResource_lifecycle(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_api_key_lifetime_group_override.contractors"

	exemptConfig := `
resource "barndoor_llm_api_key_lifetime_group_override" "contractors" {
  group_name = "contractors"
}
`
	capConfig := `
resource "barndoor_llm_api_key_lifetime_group_override" "contractors" {
  group_name        = "contractors"
  max_lifetime_days = 7
  require_expiry    = true
}
`
	renamedConfig := `
resource "barndoor_llm_api_key_lifetime_group_override" "contractors" {
  group_name        = "vendors"
  max_lifetime_days = 7
  require_expiry    = true
}
`

	var firstID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkLlmLifetimePoliciesCleared(fake),
		Steps: []resource.TestStep{
			{
				// No limits set: the override exempts the group.
				Config: exemptConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckNoResourceAttr(resourceName, "max_lifetime_days"),
					resource.TestCheckResourceAttr(resourceName, "require_expiry", "false"),
					captureID(resourceName, &firstID),
				),
			},
			{Config: exemptConfig, PlanOnly: true},
			{
				Config: capConfig,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(resourceName, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "max_lifetime_days", "7"),
					resource.TestCheckResourceAttr(resourceName, "require_expiry", "true"),
					checkIDUnchanged(resourceName, &firstID),
				),
			},
			{
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				// A different group is a different override.
				Config: renamedConfig,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(resourceName, plancheck.ResourceActionDestroyBeforeCreate),
					},
				},
				Check: func(*terraform.State) error {
					fake.mu.Lock()
					defer fake.mu.Unlock()
					if len(fake.lifetimeGroups) != 1 || *fake.lifetimeGroups[0].GroupName != "vendors" {
						return fmt.Errorf("want only the vendors override, have %d override(s)", len(fake.lifetimeGroups))
					}
					return nil
				},
			},
		},
	})
}

// The platform trims the group name; whitespace in configuration must not plan
// a replacement on every run.
func TestLlmAPIKeyLifetimeGroupOverrideResource_trimmedNameIsStable(t *testing.T) {
	setupLlmGatewayTest(t)
	config := `
resource "barndoor_llm_api_key_lifetime_group_override" "eng" {
  group_name        = " eng "
  max_lifetime_days = 30
}
`
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: config},
			{Config: config, PlanOnly: true},
		},
	})
}

func TestLlmAPIKeyLifetimeGroupOverrideResource_duplicateIs409(t *testing.T) {
	setupLlmGatewayTest(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "barndoor_llm_api_key_lifetime_group_override" "a" {
  group_name        = "eng"
  max_lifetime_days = 30
}

resource "barndoor_llm_api_key_lifetime_group_override" "b" {
  group_name = "eng"
  depends_on = [barndoor_llm_api_key_lifetime_group_override.a]
}
`,
			ExpectError: regexp.MustCompile(`The eng group already has a key lifetime override`),
		}},
	})
}

func TestLlmAPIKeyLifetimeGroupOverrideResource_outOfBandDeletePlansRecreate(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_api_key_lifetime_group_override.eng"
	config := `
resource "barndoor_llm_api_key_lifetime_group_override" "eng" {
  group_name        = "eng"
  max_lifetime_days = 30
}
`
	var firstID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: config, Check: captureID(resourceName, &firstID)},
			{
				PreConfig: func() {
					fake.mu.Lock()
					fake.lifetimeGroups = nil
					fake.mu.Unlock()
				},
				Config: config,
				Check: func(s *terraform.State) error {
					if id := s.RootModule().Resources[resourceName].Primary.ID; id == firstID {
						return fmt.Errorf("expected a new override after an out-of-band delete, still %s", id)
					}
					return nil
				},
			},
		},
	})
}

// --- user overrides -----------------------------------------------------------------

func TestLlmAPIKeyLifetimeUserOverrideResource_lifecycle(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_api_key_lifetime_user_override.ada"

	exemptConfig := `
resource "barndoor_llm_api_key_lifetime_user_override" "ada" {
  user_id = "AAAAAAAA-0000-0000-0000-000000000001"
}
`
	fixedConfig := `
resource "barndoor_llm_api_key_lifetime_user_override" "ada" {
  user_id           = "AAAAAAAA-0000-0000-0000-000000000001"
  max_lifetime_days = 1
  fixed_lifetime    = true
}
`
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkLlmLifetimePoliciesCleared(fake),
		Steps: []resource.TestStep{
			{
				Config: exemptConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckResourceAttr(resourceName, "user_id", "AAAAAAAA-0000-0000-0000-000000000001"),
					resource.TestCheckNoResourceAttr(resourceName, "max_lifetime_days"),
				),
			},
			// The platform answers the id in lowercase; that must not plan a replacement.
			{Config: exemptConfig, PlanOnly: true},
			{
				Config: fixedConfig,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(resourceName, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "max_lifetime_days", "1"),
					resource.TestCheckResourceAttr(resourceName, "fixed_lifetime", "true"),
				),
			},
			{
				ResourceName:            resourceName,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"user_id"}, // import reads the canonical lowercase id
			},
		},
	})
}

func TestLlmAPIKeyLifetimeUserOverrideResource_rejections(t *testing.T) {
	setupLlmGatewayTest(t)
	cases := []struct {
		name, config string
		want         *regexp.Regexp
		planOnly     bool
	}{
		{
			name: "not a uuid",
			config: `resource "barndoor_llm_api_key_lifetime_user_override" "x" {
  user_id = "ada@example.com"
}`,
			want:     regexp.MustCompile(`must be a user ID \(a UUID\)`),
			planOnly: true,
		},
		{
			name: "outside the org",
			config: `resource "barndoor_llm_api_key_lifetime_user_override" "x" {
  user_id = "` + fakeLlmOutsiderUserID + `"
}`,
			want: regexp.MustCompile(`user_id is not a member of this organization`),
		},
		{
			name: "duplicate",
			config: `
resource "barndoor_llm_api_key_lifetime_user_override" "a" {
  user_id = "aaaaaaaa-0000-0000-0000-000000000002"
}
resource "barndoor_llm_api_key_lifetime_user_override" "b" {
  user_id    = "aaaaaaaa-0000-0000-0000-000000000002"
  depends_on = [barndoor_llm_api_key_lifetime_user_override.a]
}`,
			want: regexp.MustCompile(`This user already has a key lifetime override`),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{{
					Config:      tc.config,
					PlanOnly:    tc.planOnly,
					ExpectError: tc.want,
				}},
			})
		})
	}
}

// --- helpers --------------------------------------------------------------------------

func ptrInt32(v int32) *int32 { return &v }

// checkLifetimeDefault asserts the platform's stored default.
func (f *fakeLlmGatewayServer) checkLifetimeDefault(maxDays *int32, require, fixed bool) resource.TestCheckFunc {
	return func(*terraform.State) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		d := f.lifetimeDefault
		if d == nil {
			return fmt.Errorf("no default lifetime policy stored")
		}
		if (d.MaxLifetimeDays == nil) != (maxDays == nil) ||
			(maxDays != nil && *d.MaxLifetimeDays != *maxDays) ||
			d.RequireExpiry != require || d.FixedLifetime != fixed {
			return fmt.Errorf("stored default = {max %v, require %t, fixed %t}, want {max %v, require %t, fixed %t}",
				d.MaxLifetimeDays, d.RequireExpiry, d.FixedLifetime, maxDays, require, fixed)
		}
		return nil
	}
}

func captureID(name string, into *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[name]
		if !ok {
			return fmt.Errorf("%s not in state", name)
		}
		*into = rs.Primary.ID
		return nil
	}
}

func checkIDUnchanged(name string, want *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		if id := s.RootModule().Resources[name].Primary.ID; id != *want {
			return fmt.Errorf("%s id changed from %s to %s; want an in-place update", name, *want, id)
		}
		return nil
	}
}
