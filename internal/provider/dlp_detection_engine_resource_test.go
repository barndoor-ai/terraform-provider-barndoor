// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"testing"

	frameworkresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// --- schema tests --------------------------------------------------------------

func TestDlpDetectionEngineResource_Metadata(t *testing.T) {
	var resp frameworkresource.MetadataResponse
	NewDlpDetectionEngineResource().Metadata(context.Background(),
		frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_dlp_detection_engine"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestDlpDetectionEngineResource_Schema(t *testing.T) {
	var resp frameworkresource.SchemaResponse
	NewDlpDetectionEngineResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{
		"id", "org_id", "name", "provider_type", "provider_connection_name",
		"enabled_detection_types", "config", "supported_detection_types",
		"supported_actions", "provider_class", "capabilities", "runtime_stages",
		"created_by", "updated_by", "created_at", "updated_at",
	} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
	for _, required := range []string{"name", "provider_type", "enabled_detection_types"} {
		if !resp.Schema.Attributes[required].IsRequired() {
			t.Errorf("%s should be Required", required)
		}
	}
	for _, computed := range []string{
		"id", "org_id", "supported_detection_types", "supported_actions",
		"provider_class", "capabilities", "runtime_stages", "created_at", "updated_at",
	} {
		if !resp.Schema.Attributes[computed].IsComputed() {
			t.Errorf("%s should be Computed", computed)
		}
	}
	// The alias must be greppable from the schema, so practitioners searching
	// for the app's terminology land here.
	if desc := resp.Schema.MarkdownDescription; !regexp.MustCompile(`Protection Profile`).MatchString(desc) {
		t.Error("resource description must mention the app's \"Protection Profile\" alias")
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

func TestDlpDetectionEngineResource_lifecycle(t *testing.T) {
	fake := setupDlpTest(t)
	const resourceName = "barndoor_dlp_detection_engine.test"

	var engineID string
	fullConfig := `
resource "barndoor_dlp_detection_engine" "test" {
  name                     = "tf-dlp-engine"
  provider_type            = "presidio"
  provider_connection_name = "primary-presidio"
  enabled_detection_types  = ["DETECTION_TYPE_EMAIL_ADDRESS", "DETECTION_TYPE_PHONE_NUMBER"]
  config                   = jsonencode({ language = "en" })
}
`
	updatedConfig := `
resource "barndoor_dlp_detection_engine" "test" {
  name                    = "tf-dlp-engine-renamed"
  provider_type           = "google_dlp"
  enabled_detection_types = ["DETECTION_TYPE_EMAIL_ADDRESS"]
  config                  = jsonencode({ location = "global" })
}
`

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpDetectionEnginesDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: fullConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckResourceAttr(resourceName, "org_id", fakeDlpOrgID),
					resource.TestCheckResourceAttr(resourceName, "name", "tf-dlp-engine"),
					resource.TestCheckResourceAttr(resourceName, "provider_type", "presidio"),
					resource.TestCheckResourceAttr(resourceName, "provider_connection_name", "primary-presidio"),
					resource.TestCheckResourceAttr(resourceName, "enabled_detection_types.#", "2"),
					// Server-derived capability surface.
					resource.TestCheckResourceAttr(resourceName, "provider_class", "span_detection"),
					resource.TestCheckResourceAttr(resourceName, "capabilities.pii_detection", "true"),
					resource.TestCheckResourceAttr(resourceName, "capabilities.provider_class", "span_detection"),
					resource.TestCheckResourceAttr(resourceName, "supported_actions.0", "POLICY_ACTION_BLOCK"),
					resource.TestCheckResourceAttr(resourceName, "runtime_stages.#", "4"),
					// Server-observed state, not just Terraform's echo.
					func(s *terraform.State) error {
						rs, ok := s.RootModule().Resources[resourceName]
						if !ok {
							return fmt.Errorf("%s not in state", resourceName)
						}
						engineID = rs.Primary.ID
						e := fake.getDetectionEngine(engineID)
						if e == nil {
							return fmt.Errorf("fake has no detection engine %s", engineID)
						}
						if e.Name != "tf-dlp-engine" || e.ProviderType != "presidio" {
							return fmt.Errorf("fake stored name=%q provider_type=%q", e.Name, e.ProviderType)
						}
						if e.ProviderConnectionName == nil || *e.ProviderConnectionName != "primary-presidio" {
							return fmt.Errorf("fake stored provider_connection_name=%v", e.ProviderConnectionName)
						}
						return nil
					},
				),
			},
			{
				// A second plan over unchanged configuration must be empty.
				Config:   fullConfig,
				PlanOnly: true,
			},
			{
				// In-place update: rename, switch the provider type (updatable
				// via PUT — no replacement), drop the connection name, shrink
				// the detection types, and swap the config.
				Config: updatedConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "name", "tf-dlp-engine-renamed"),
					resource.TestCheckResourceAttr(resourceName, "provider_type", "google_dlp"),
					resource.TestCheckNoResourceAttr(resourceName, "provider_connection_name"),
					resource.TestCheckResourceAttr(resourceName, "enabled_detection_types.#", "1"),
					// The id must survive: every attribute updates in place.
					func(s *terraform.State) error {
						rs, ok := s.RootModule().Resources[resourceName]
						if !ok {
							return fmt.Errorf("%s not in state", resourceName)
						}
						if rs.Primary.ID != engineID {
							return fmt.Errorf("engine was replaced (id %s -> %s); every attribute must update in place",
								engineID, rs.Primary.ID)
						}
						return nil
					},
					// The fake records the PUT body: dropping the connection
					// name must be sent as an explicit JSON null (the API's
					// double-Option clear), never an absent key (keep).
					func(*terraform.State) error {
						fake.mu.Lock()
						raw := fake.lastEngineUpdate[engineID]
						fake.mu.Unlock()
						if len(raw) == 0 {
							return fmt.Errorf("fake recorded no PUT body for engine %s", engineID)
						}
						var keys map[string]json.RawMessage
						if err := json.Unmarshal(raw, &keys); err != nil {
							return fmt.Errorf("unmarshal recorded PUT body: %w", err)
						}
						v, ok := keys["provider_connection_name"]
						if !ok {
							return fmt.Errorf("provider_connection_name must be serialized even when unset (the API keeps the old value otherwise)")
						}
						if string(v) != "null" {
							return fmt.Errorf("provider_connection_name = %s, want null (explicit clear)", v)
						}
						return nil
					},
					// Server-observed state after the update.
					func(*terraform.State) error {
						e := fake.getDetectionEngine(engineID)
						if e == nil {
							return fmt.Errorf("fake has no detection engine %s", engineID)
						}
						if e.Name != "tf-dlp-engine-renamed" || e.ProviderType != "google_dlp" {
							return fmt.Errorf("fake stored name=%q provider_type=%q", e.Name, e.ProviderType)
						}
						if e.ProviderConnectionName != nil {
							return fmt.Errorf("provider_connection_name was not cleared server-side: %q", *e.ProviderConnectionName)
						}
						var cfg map[string]string
						if err := json.Unmarshal(e.Config, &cfg); err != nil || cfg["location"] != "global" {
							return fmt.Errorf("fake stored config=%s", e.Config)
						}
						return nil
					},
				),
			},
			{
				Config:   updatedConfig,
				PlanOnly: true,
			},
			{
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestDlpDetectionEngineResource_configResetsToServerDefault(t *testing.T) {
	fake := setupDlpTest(t)
	const resourceName = "barndoor_dlp_detection_engine.test"

	withConfig := `
resource "barndoor_dlp_detection_engine" "test" {
  name                    = "tf-dlp-engine-config"
  provider_type           = "builtin_regex"
  enabled_detection_types = ["DETECTION_TYPE_EMAIL_ADDRESS"]
  config                  = jsonencode({ tuning = "strict" })
}
`
	withoutConfig := `
resource "barndoor_dlp_detection_engine" "test" {
  name                    = "tf-dlp-engine-config"
  provider_type           = "builtin_regex"
  enabled_detection_types = ["DETECTION_TYPE_EMAIL_ADDRESS"]
}
`

	var engineID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpDetectionEnginesDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: withConfig,
				Check: func(s *terraform.State) error {
					rs, ok := s.RootModule().Resources[resourceName]
					if !ok {
						return fmt.Errorf("%s not in state", resourceName)
					}
					engineID = rs.Primary.ID
					return nil
				},
			},
			{
				// Removing config must clear it server-side (the update body
				// sends {}, the server default) and settle state back to null
				// — no perpetual diff.
				Config: withoutConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(resourceName, "config"),
					func(*terraform.State) error {
						e := fake.getDetectionEngine(engineID)
						if e == nil {
							return fmt.Errorf("fake has no detection engine %s", engineID)
						}
						if string(e.Config) != "{}" {
							return fmt.Errorf("config was not reset server-side, fake stored %s", e.Config)
						}
						return nil
					},
				),
			},
			{
				Config:   withoutConfig,
				PlanOnly: true,
			},
		},
	})
}

func TestDlpDetectionEngineResource_deleteWhileSoleEngineOnPolicyIs409(t *testing.T) {
	fake := setupDlpTest(t)
	const resourceName = "barndoor_dlp_detection_engine.test"

	config := `
resource "barndoor_dlp_detection_engine" "test" {
  name                    = "tf-dlp-engine-referenced"
  provider_type           = "presidio"
  enabled_detection_types = ["DETECTION_TYPE_EMAIL_ADDRESS"]
}
`
	var engineID, policyID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpDetectionEnginesDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: func(s *terraform.State) error {
					rs, ok := s.RootModule().Resources[resourceName]
					if !ok {
						return fmt.Errorf("%s not in state", resourceName)
					}
					engineID = rs.Primary.ID
					return nil
				},
			},
			{
				// An enforcement policy (created out-of-band) references the
				// engine as its only one; the destroy must fail with the
				// actionable pointer at detection_engine_ids.
				PreConfig: func() {
					policyID = fake.seedPolicyReferencingEngine("blocks-engine-delete", engineID).ID
				},
				Config:      config,
				Destroy:     true,
				ExpectError: regexp.MustCompile(`(?s)still referenced by an enforcement policy.*detection_engine_ids`),
			},
			{
				// Detaching the policy unblocks the destroy (the final
				// automatic destroy of the test exercises it).
				PreConfig: func() { fake.markPolicyDeleted(t, policyID) },
				Config:    config,
				Check: func(*terraform.State) error {
					if fake.getDetectionEngine(engineID) == nil {
						return fmt.Errorf("the failed destroy must not have deleted engine %s", engineID)
					}
					return nil
				},
			},
		},
	})
}

func TestDlpDetectionEngineResource_uniquenessIsPerNameAndProviderType(t *testing.T) {
	fake := setupDlpTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpDetectionEnginesDeleted(fake),
		Steps: []resource.TestStep{
			{
				// The same name across two provider types is legitimate — the
				// app renders it as one merged Protection Profile.
				Config: `
resource "barndoor_dlp_detection_engine" "presidio" {
  name                    = "tf-dlp-shared-name"
  provider_type           = "presidio"
  enabled_detection_types = ["DETECTION_TYPE_EMAIL_ADDRESS"]
}

resource "barndoor_dlp_detection_engine" "google" {
  name                    = "tf-dlp-shared-name"
  provider_type           = "google_dlp"
  enabled_detection_types = ["DETECTION_TYPE_EMAIL_ADDRESS"]
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("barndoor_dlp_detection_engine.presidio", "name", "tf-dlp-shared-name"),
					resource.TestCheckResourceAttr("barndoor_dlp_detection_engine.google", "name", "tf-dlp-shared-name"),
				),
			},
		},
	})
}

func TestDlpDetectionEngineResource_duplicateNameAndProviderTypeIs409(t *testing.T) {
	setupDlpTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// The pre-seeded engine already holds (name, provider_type);
				// creating the same pair must surface the API's 409 with the
				// uniqueness hint.
				Config: fmt.Sprintf(`
resource "barndoor_dlp_detection_engine" "dupe" {
  name                    = %q
  provider_type           = "presidio"
  enabled_detection_types = ["DETECTION_TYPE_EMAIL_ADDRESS"]
}
`, fakeDlpSeededEngineName),
				ExpectError: regexp.MustCompile(`(?s)already exists.*name.*provider_type`),
			},
		},
	})
}

func TestDlpDetectionEngineResource_unknownProviderTypeIs400(t *testing.T) {
	setupDlpTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// The API's 400 message must surface verbatim.
				Config: `
resource "barndoor_dlp_detection_engine" "test" {
  name                    = "tf-dlp-bad-provider"
  provider_type           = "no_such_provider"
  enabled_detection_types = ["DETECTION_TYPE_EMAIL_ADDRESS"]
}
`,
				ExpectError: regexp.MustCompile(`unsupported detection engine provider_type 'no_such_provider'`),
			},
		},
	})
}

func TestDlpDetectionEngineResource_outOfBandDeletePlansRecreate(t *testing.T) {
	fake := setupDlpTest(t)
	const resourceName = "barndoor_dlp_detection_engine.test"

	config := `
resource "barndoor_dlp_detection_engine" "test" {
  name                    = "tf-dlp-oob"
  provider_type           = "presidio"
  enabled_detection_types = ["DETECTION_TYPE_EMAIL_ADDRESS"]
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
				PreConfig: func() { fake.markDetectionEngineDeleted(t, firstID) },
				Config:    config,
				Check: func(s *terraform.State) error {
					rs, ok := s.RootModule().Resources[resourceName]
					if !ok {
						return fmt.Errorf("%s not in state", resourceName)
					}
					if rs.Primary.ID == firstID {
						return fmt.Errorf("expected a new engine id after out-of-band delete, still %s", firstID)
					}
					return nil
				},
			},
		},
	})
}

func TestDlpDetectionEngineResource_planTimeValidation(t *testing.T) {
	setupDlpTest(t)

	for name, tc := range map[string]struct {
		config      string
		expectError *regexp.Regexp
	}{
		"whitespace-padded name": {
			config: `
resource "barndoor_dlp_detection_engine" "test" {
  name                    = " padded "
  provider_type           = "presidio"
  enabled_detection_types = ["DETECTION_TYPE_EMAIL_ADDRESS"]
}
`,
			expectError: regexp.MustCompile(`must not be empty or have leading/trailing\s+whitespace`),
		},
		"empty detection types": {
			config: `
resource "barndoor_dlp_detection_engine" "test" {
  name                    = "tf-dlp-invalid"
  provider_type           = "presidio"
  enabled_detection_types = []
}
`,
			expectError: regexp.MustCompile(`at least 1`),
		},
		"duplicate detection types": {
			config: `
resource "barndoor_dlp_detection_engine" "test" {
  name                    = "tf-dlp-invalid"
  provider_type           = "presidio"
  enabled_detection_types = ["DETECTION_TYPE_EMAIL_ADDRESS", "DETECTION_TYPE_EMAIL_ADDRESS"]
}
`,
			expectError: regexp.MustCompile(`(?i)duplicate`),
		},
	} {
		t.Run(name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{
					{
						Config:      tc.config,
						PlanOnly:    true,
						ExpectError: tc.expectError,
					},
				},
			})
		})
	}
}
