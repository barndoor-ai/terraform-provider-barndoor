// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"regexp"
	"testing"

	frameworkdatasource "github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// --- schema tests --------------------------------------------------------------

func TestDlpDetectionEngineDataSource_Metadata(t *testing.T) {
	var resp frameworkdatasource.MetadataResponse
	NewDlpDetectionEngineDataSource().Metadata(context.Background(),
		frameworkdatasource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_dlp_detection_engine"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestDlpDetectionEngineDataSource_Schema(t *testing.T) {
	var resp frameworkdatasource.SchemaResponse
	NewDlpDetectionEngineDataSource().Schema(context.Background(), frameworkdatasource.SchemaRequest{}, &resp)
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
	if desc := resp.Schema.MarkdownDescription; !regexp.MustCompile(`Protection Profile`).MatchString(desc) {
		t.Error("data source description must mention the app's \"Protection Profile\" alias")
	}
}

// --- lookups (real plan/apply against the fake) -----------------------------------

func TestDlpDetectionEngineDataSource_byID(t *testing.T) {
	setupDlpTest(t)
	const dataName = "data.barndoor_dlp_detection_engine.test"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
data "barndoor_dlp_detection_engine" "test" {
  id = %q
}
`, fakeDlpDetectionEngineID),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(dataName, "id", fakeDlpDetectionEngineID),
					resource.TestCheckResourceAttr(dataName, "name", fakeDlpSeededEngineName),
					resource.TestCheckResourceAttr(dataName, "provider_type", "presidio"),
					resource.TestCheckResourceAttr(dataName, "org_id", fakeDlpOrgID),
					resource.TestCheckResourceAttr(dataName, "provider_class", "span_detection"),
					resource.TestCheckResourceAttr(dataName, "capabilities.span_offsets", "true"),
					resource.TestCheckResourceAttr(dataName, "enabled_detection_types.0", "DETECTION_TYPE_EMAIL_ADDRESS"),
					resource.TestCheckResourceAttr(dataName, "supported_actions.0", "POLICY_ACTION_BLOCK"),
				),
			},
		},
	})
}

func TestDlpDetectionEngineDataSource_byUniqueName(t *testing.T) {
	setupDlpTest(t)
	const dataName = "data.barndoor_dlp_detection_engine.test"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
data "barndoor_dlp_detection_engine" "test" {
  name = %q
}
`, fakeDlpSeededEngineName),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(dataName, "id", fakeDlpDetectionEngineID),
					resource.TestCheckResourceAttr(dataName, "provider_type", "presidio"),
				),
			},
		},
	})
}

func TestDlpDetectionEngineDataSource_ambiguousNameListsCandidates(t *testing.T) {
	fake := setupDlpTest(t)
	// A second engine with the seeded engine's name under a different
	// provider type — one merged "Protection Profile" in the app.
	sibling := fake.seedDetectionEngine(fakeDlpSeededEngineName, "google_dlp")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
data "barndoor_dlp_detection_engine" "test" {
  name = %q
}
`, fakeDlpSeededEngineName),
				// The error must name every candidate id so the practitioner
				// can pick one without another lookup.
				ExpectError: regexp.MustCompile(fmt.Sprintf(`(?s)ambiguous.*%s.*%s|(?s)ambiguous.*%s.*%s`,
					fakeDlpDetectionEngineID, sibling.ID, sibling.ID, fakeDlpDetectionEngineID)),
			},
		},
	})
}

func TestDlpDetectionEngineDataSource_nameWithProviderTypeDisambiguates(t *testing.T) {
	fake := setupDlpTest(t)
	sibling := fake.seedDetectionEngine(fakeDlpSeededEngineName, "google_dlp")
	const dataName = "data.barndoor_dlp_detection_engine.test"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
data "barndoor_dlp_detection_engine" "test" {
  name          = %q
  provider_type = "google_dlp"
}
`, fakeDlpSeededEngineName),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(dataName, "id", sibling.ID),
					resource.TestCheckResourceAttr(dataName, "provider_type", "google_dlp"),
				),
			},
		},
	})
}

func TestDlpDetectionEngineDataSource_notFound(t *testing.T) {
	setupDlpTest(t)

	for name, tc := range map[string]struct {
		config      string
		expectError *regexp.Regexp
	}{
		"by name": {
			config: `
data "barndoor_dlp_detection_engine" "test" {
  name = "no-such-profile"
}
`,
			expectError: regexp.MustCompile(`(?s)Detection engine not found.*no-such-profile`),
		},
		"by name and provider_type": {
			config: fmt.Sprintf(`
data "barndoor_dlp_detection_engine" "test" {
  name          = %q
  provider_type = "azure_content_safety"
}
`, fakeDlpSeededEngineName),
			expectError: regexp.MustCompile(`(?s)Detection engine not found.*azure_content_safety`),
		},
		"by id": {
			config: `
data "barndoor_dlp_detection_engine" "test" {
  id = "dddd0000-0000-0000-0000-000000000099"
}
`,
			expectError: regexp.MustCompile(`Detection engine not found`),
		},
	} {
		t.Run(name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{
					{
						Config:      tc.config,
						ExpectError: tc.expectError,
					},
				},
			})
		})
	}
}

func TestDlpDetectionEngineDataSource_configValidation(t *testing.T) {
	setupDlpTest(t)

	for name, tc := range map[string]struct {
		config      string
		expectError *regexp.Regexp
	}{
		"neither id nor name": {
			config: `
data "barndoor_dlp_detection_engine" "test" {
}
`,
			expectError: regexp.MustCompile(`Missing Attribute Configuration|Invalid Attribute Combination`),
		},
		"both id and name": {
			config: fmt.Sprintf(`
data "barndoor_dlp_detection_engine" "test" {
  id   = %q
  name = %q
}
`, fakeDlpDetectionEngineID, fakeDlpSeededEngineName),
			expectError: regexp.MustCompile(`Invalid Attribute Combination`),
		},
		"provider_type without name": {
			config: fmt.Sprintf(`
data "barndoor_dlp_detection_engine" "test" {
  id            = %q
  provider_type = "presidio"
}
`, fakeDlpDetectionEngineID),
			expectError: regexp.MustCompile(`(?s)provider_type.*name`),
		},
	} {
		t.Run(name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{
					{
						Config:      tc.config,
						ExpectError: tc.expectError,
					},
				},
			})
		})
	}
}
