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
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// --- schema tests --------------------------------------------------------------

func TestDlpCustomDetectionTypeResource_Metadata(t *testing.T) {
	var resp frameworkresource.MetadataResponse
	NewDlpCustomDetectionTypeResource().Metadata(context.Background(),
		frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_dlp_custom_detection_type"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestDlpCustomDetectionTypeResource_Schema(t *testing.T) {
	var resp frameworkresource.SchemaResponse
	NewDlpCustomDetectionTypeResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{
		"id", "org_id", "detection_type", "name", "description", "patterns", "detection_group",
		"default_severity", "default_confidence", "created_at", "updated_at",
	} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
	for _, required := range []string{"name", "patterns"} {
		if !resp.Schema.Attributes[required].IsRequired() {
			t.Errorf("%s should be Required", required)
		}
	}
	for _, computed := range []string{
		"id", "org_id", "detection_type", "detection_group", "default_severity",
		"default_confidence", "created_at", "updated_at",
	} {
		if !resp.Schema.Attributes[computed].IsComputed() {
			t.Errorf("%s should be Computed", computed)
		}
	}
}

// --- conversion tests ------------------------------------------------------------

func TestBuildDlpCustomDetectionTypeUpdateRequest_ExplicitClears(t *testing.T) {
	// Convergence contract: the update body always carries description (as ""
	// when unset), so removing it from configuration clears it server-side
	// under the API's presence-based partial-update semantics.
	plan := &dlpCustomDetectionTypeResourceModel{
		ID:   types.StringValue("cccc0000-0000-0000-0000-000000000001"),
		Name: types.StringValue("Codenames"),
		Patterns: []dlpCustomDetectionPatternModel{
			{Pattern: types.StringValue("AURORA"), PatternType: types.StringValue("PATTERN_TYPE_LITERAL")},
		},
		DefaultSeverity:   types.StringValue("DETECTION_SEVERITY_MEDIUM"),
		DefaultConfidence: types.StringValue("DETECTION_CONFIDENCE_MEDIUM"),
	}
	body := buildDlpCustomDetectionTypeUpdateRequest(plan)

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if v, ok := keys["description"]; !ok || string(v) != `""` {
		t.Errorf("description = %s, want \"\" (the API keeps the old value when the key is absent)", v)
	}
	if v, ok := keys["patterns"]; !ok || string(v) != `[{"pattern":"AURORA","pattern_type":"PATTERN_TYPE_LITERAL"}]` {
		t.Errorf("patterns = %s, want the configured pattern list", v)
	}
}

func TestApplyDlpCustomDetectionTypeResponse_SettlesEmptyDescriptionToNull(t *testing.T) {
	prior := &dlpCustomDetectionTypeResourceModel{Description: types.StringNull()}
	detectionType := &dlpCustomDetectionTypeResponse{
		ID:            "cccc0000-0000-0000-0000-000000000001",
		OrgID:         fakeDlpOrgID,
		DetectionType: "DETECTION_TYPE_CUSTOM_1",
		Name:          "Codenames",
		Description:   "",
		Patterns: []dlpCustomDetectionPatternPayload{
			{Pattern: "AURORA", PatternType: "PATTERN_TYPE_LITERAL"},
		},
		Group:             "DETECTION_GROUP_CUSTOM",
		DefaultSeverity:   "DETECTION_SEVERITY_MEDIUM",
		DefaultConfidence: "DETECTION_CONFIDENCE_MEDIUM",
	}

	state := applyDlpCustomDetectionTypeResponse(detectionType, prior)

	if !state.Description.IsNull() {
		t.Error("empty description with a null prior must settle to null, not \"\"")
	}
	if got, want := state.DetectionGroup.ValueString(), "DETECTION_GROUP_CUSTOM"; got != want {
		t.Errorf("detection_group = %q, want %q (mapped from the wire key `group`)", got, want)
	}
	if len(state.Patterns) != 1 || state.Patterns[0].Pattern.ValueString() != "AURORA" {
		t.Errorf("patterns not mapped from the response: %+v", state.Patterns)
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

func TestDlpCustomDetectionTypeResource_lifecycle(t *testing.T) {
	fake := setupDlpTest(t)
	const resourceName = "barndoor_dlp_custom_detection_type.test"

	minimalConfig := `
resource "barndoor_dlp_custom_detection_type" "test" {
  name = "Project codenames"
  patterns = [
    { pattern = "(?i)project\\s+aurora", pattern_type = "PATTERN_TYPE_REGEX" },
  ]
}
`
	expandedConfig := `
resource "barndoor_dlp_custom_detection_type" "test" {
  name        = "Project codenames (renamed)"
  description = "Internal project codenames"
  patterns = [
    { pattern = "(?i)project\\s+aurora", pattern_type = "PATTERN_TYPE_REGEX" },
    { pattern = "AURORA-CLASSIFIED", pattern_type = "PATTERN_TYPE_LITERAL" },
  ]
  default_severity   = "DETECTION_SEVERITY_HIGH"
  default_confidence = "DETECTION_CONFIDENCE_HIGH"
}
`
	descriptionDroppedConfig := `
resource "barndoor_dlp_custom_detection_type" "test" {
  name = "Project codenames (renamed)"
  patterns = [
    { pattern = "(?i)project\\s+aurora", pattern_type = "PATTERN_TYPE_REGEX" },
    { pattern = "AURORA-CLASSIFIED", pattern_type = "PATTERN_TYPE_LITERAL" },
  ]
  default_severity   = "DETECTION_SEVERITY_HIGH"
  default_confidence = "DETECTION_CONFIDENCE_HIGH"
}
`

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpDetectionTypesDeleted(fake),
		Steps: []resource.TestStep{
			{
				// Minimal detection type: severity, confidence, group, and the
				// wire name all come from the server.
				Config: minimalConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckResourceAttr(resourceName, "org_id", fakeDlpOrgID),
					resource.TestMatchResourceAttr(resourceName, "detection_type",
						regexp.MustCompile(`^DETECTION_TYPE_CUSTOM_`)),
					resource.TestCheckResourceAttr(resourceName, "detection_group", "DETECTION_GROUP_CUSTOM"),
					resource.TestCheckResourceAttr(resourceName, "default_severity", "DETECTION_SEVERITY_MEDIUM"),
					resource.TestCheckResourceAttr(resourceName, "default_confidence", "DETECTION_CONFIDENCE_MEDIUM"),
					resource.TestCheckResourceAttr(resourceName, "patterns.#", "1"),
					resource.TestCheckResourceAttr(resourceName, "patterns.0.pattern_type", "PATTERN_TYPE_REGEX"),
					resource.TestCheckNoResourceAttr(resourceName, "description"),
				),
			},
			{
				// A second plan over unchanged configuration must be empty.
				Config:   minimalConfig,
				PlanOnly: true,
			},
			{
				// In-place update: rename, add a description and a second
				// pattern, raise severity and confidence.
				Config: expandedConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "name", "Project codenames (renamed)"),
					resource.TestCheckResourceAttr(resourceName, "description", "Internal project codenames"),
					resource.TestCheckResourceAttr(resourceName, "patterns.#", "2"),
					resource.TestCheckResourceAttr(resourceName, "patterns.1.pattern", "AURORA-CLASSIFIED"),
					resource.TestCheckResourceAttr(resourceName, "patterns.1.pattern_type", "PATTERN_TYPE_LITERAL"),
					resource.TestCheckResourceAttr(resourceName, "default_severity", "DETECTION_SEVERITY_HIGH"),
					resource.TestCheckResourceAttr(resourceName, "default_confidence", "DETECTION_CONFIDENCE_HIGH"),
				),
			},
			{
				// Dropping description must clear it server-side (the update
				// body sends "") and settle state back to null — no perpetual
				// diff.
				Config: descriptionDroppedConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(resourceName, "description"),
				),
			},
			{
				Config:   descriptionDroppedConfig,
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

func TestDlpCustomDetectionTypeResource_outOfBandDeletePlansRecreate(t *testing.T) {
	fake := setupDlpTest(t)
	const resourceName = "barndoor_dlp_custom_detection_type.test"

	config := `
resource "barndoor_dlp_custom_detection_type" "test" {
  name = "tf-dlp-oob"
  patterns = [
    { pattern = "SECRET", pattern_type = "PATTERN_TYPE_LITERAL" },
  ]
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
				PreConfig: func() { fake.markDetectionTypeDeleted(t, firstID) },
				Config:    config,
				Check: func(s *terraform.State) error {
					rs, ok := s.RootModule().Resources[resourceName]
					if !ok {
						return fmt.Errorf("%s not in state", resourceName)
					}
					if rs.Primary.ID == firstID {
						return fmt.Errorf("expected a new detection type id after out-of-band delete, still %s", firstID)
					}
					return nil
				},
			},
		},
	})
}

func TestDlpCustomDetectionTypeResource_invalidRegexIs422(t *testing.T) {
	setupDlpTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// A non-compiling regex is rejected by the API with a 422; the
				// message must surface verbatim.
				Config: `
resource "barndoor_dlp_custom_detection_type" "test" {
  name = "tf-dlp-bad-regex"
  patterns = [
    { pattern = "([unclosed", pattern_type = "PATTERN_TYPE_REGEX" },
  ]
}
`,
				ExpectError: regexp.MustCompile(`invalid regex pattern`),
			},
		},
	})
}
