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

func TestDlpAllowListEntryResource_Metadata(t *testing.T) {
	var resp frameworkresource.MetadataResponse
	NewDlpAllowListEntryResource().Metadata(context.Background(),
		frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_dlp_allow_list_entry"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestDlpAllowListEntryResource_Schema(t *testing.T) {
	var resp frameworkresource.SchemaResponse
	NewDlpAllowListEntryResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{
		"id", "org_id", "pattern", "pattern_type", "detection_types", "reason", "created_by", "created_at",
	} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
	for _, required := range []string{"pattern", "pattern_type"} {
		if !resp.Schema.Attributes[required].IsRequired() {
			t.Errorf("%s should be Required", required)
		}
	}
	for _, computed := range []string{"id", "org_id", "created_by", "created_at"} {
		if !resp.Schema.Attributes[computed].IsComputed() {
			t.Errorf("%s should be Computed", computed)
		}
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

func dlpAllowListEntryConfig(pattern, extra string) string {
	return fmt.Sprintf(`
resource "barndoor_dlp_allow_list_entry" "test" {
  pattern      = %q
  pattern_type = "PATTERN_TYPE_LITERAL"
%s
}
`, pattern, extra)
}

func TestDlpAllowListEntryResource_lifecycle(t *testing.T) {
	fake := setupDlpTest(t)
	const resourceName = "barndoor_dlp_allow_list_entry.test"

	fullConfig := dlpAllowListEntryConfig("test@example.com", `
  detection_types = ["DETECTION_TYPE_EMAIL"]
  reason          = "Shared support mailbox, not PII"
`)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpAllowListEntriesDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: fullConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckResourceAttr(resourceName, "org_id", "org-123"),
					resource.TestCheckResourceAttr(resourceName, "pattern", "test@example.com"),
					resource.TestCheckResourceAttr(resourceName, "pattern_type", "PATTERN_TYPE_LITERAL"),
					resource.TestCheckResourceAttr(resourceName, "detection_types.#", "1"),
					resource.TestCheckResourceAttr(resourceName, "reason", "Shared support mailbox, not PII"),
				),
			},
			{
				// A second plan over unchanged configuration must be empty
				// (Read walks the paginated list to find the entry).
				Config:   fullConfig,
				PlanOnly: true,
			},
			{
				// The API has no update endpoint: changing the pattern must
				// plan a replacement.
				Config: dlpAllowListEntryConfig("other@example.com", `
  detection_types = ["DETECTION_TYPE_EMAIL"]
  reason          = "Shared support mailbox, not PII"
`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(resourceName, plancheck.ResourceActionReplace),
					},
				},
			},
			{
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestDlpAllowListEntryResource_minimalEntrySettlesNull(t *testing.T) {
	fake := setupDlpTest(t)
	const resourceName = "barndoor_dlp_allow_list_entry.test"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpAllowListEntriesDeleted(fake),
		Steps: []resource.TestStep{
			{
				// detection_types and reason omitted: the API echoes [] and ""
				// respectively, which must settle back to null.
				Config: dlpAllowListEntryConfig("4111 1111 1111 1111", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(resourceName, "detection_types.#"),
					resource.TestCheckNoResourceAttr(resourceName, "reason"),
				),
			},
			{
				Config:   dlpAllowListEntryConfig("4111 1111 1111 1111", ""),
				PlanOnly: true,
			},
		},
	})
}

// TestDlpAllowListEntryResource_readWalksPagination proves Read finds entries
// beyond the first page: the fake serves at most fakeDlpAllowListPageCap (2)
// entries per page, so with three entries the refresh of the last one must
// follow next_page_token. The out-of-band delete then proves a missing entry
// is dropped for recreate rather than erroring.
func TestDlpAllowListEntryResource_readWalksPagination(t *testing.T) {
	fake := setupDlpTest(t)

	config := `
resource "barndoor_dlp_allow_list_entry" "a" {
  pattern      = "pattern-a"
  pattern_type = "PATTERN_TYPE_LITERAL"
}

resource "barndoor_dlp_allow_list_entry" "b" {
  pattern      = "pattern-b"
  pattern_type = "PATTERN_TYPE_LITERAL"
  depends_on   = [barndoor_dlp_allow_list_entry.a]
}

resource "barndoor_dlp_allow_list_entry" "c" {
  pattern      = "pattern-c"
  pattern_type = "PATTERN_TYPE_LITERAL"
  depends_on   = [barndoor_dlp_allow_list_entry.b]
}
`
	const lastResource = "barndoor_dlp_allow_list_entry.c"
	var lastID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpAllowListEntriesDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: func(s *terraform.State) error {
					rs, ok := s.RootModule().Resources[lastResource]
					if !ok {
						return fmt.Errorf("%s not in state", lastResource)
					}
					lastID = rs.Primary.ID
					return nil
				},
			},
			{
				// All three refreshes walk the 2-per-page list cleanly.
				Config:   config,
				PlanOnly: true,
			},
			{
				// Deleting the page-2 entry out-of-band must drop exactly that
				// resource on refresh and recreate it.
				PreConfig: func() { fake.markAllowListEntryDeleted(t, lastID) },
				Config:    config,
				Check: func(s *terraform.State) error {
					rs, ok := s.RootModule().Resources[lastResource]
					if !ok {
						return fmt.Errorf("%s not in state", lastResource)
					}
					if rs.Primary.ID == lastID {
						return fmt.Errorf("expected a new entry id after out-of-band delete, still %s", lastID)
					}
					return nil
				},
			},
		},
	})
}

func TestDlpAllowListEntryResource_unknownDetectionTypeIs422(t *testing.T) {
	setupDlpTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: dlpAllowListEntryConfig("nope", `
  detection_types = ["NOT_A_DETECTION_TYPE"]
`),
				ExpectError: regexp.MustCompile(`unknown detection_type: NOT_A_DETECTION_TYPE`),
			},
		},
	})
}
