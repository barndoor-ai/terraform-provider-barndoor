// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"testing"

	frameworkresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// --- schema tests --------------------------------------------------------------

func TestDlpOrgConfigResource_Metadata(t *testing.T) {
	var resp frameworkresource.MetadataResponse
	NewDlpOrgConfigResource().Metadata(context.Background(),
		frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_dlp_org_config"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestDlpOrgConfigResource_Schema(t *testing.T) {
	var resp frameworkresource.SchemaResponse
	NewDlpOrgConfigResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{"id", "org_id", "enabled", "global_dry_run", "created_at", "updated_at"} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
	for _, computed := range []string{"id", "org_id", "created_at", "updated_at"} {
		if !resp.Schema.Attributes[computed].IsComputed() {
			t.Errorf("%s should be Computed", computed)
		}
	}
	for _, optional := range []string{"enabled", "global_dry_run"} {
		if !resp.Schema.Attributes[optional].IsOptional() {
			t.Errorf("%s should be Optional", optional)
		}
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

func dlpOrgConfigConfig(attrs string) string {
	return `
resource "barndoor_dlp_org_config" "test" {
` + attrs + `
}
`
}

func TestDlpOrgConfigResource_lifecycle(t *testing.T) {
	fake := setupDlpTest(t)
	const resourceName = "barndoor_dlp_org_config.test"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		// The singleton cannot be deleted: destroy must have written the
		// platform defaults back (the last step leaves non-default values).
		CheckDestroy: checkDlpOrgConfigReset(fake),
		Steps: []resource.TestStep{
			{
				// Only global_dry_run configured: enabled adopts the current
				// server-side value (the platform default).
				Config: dlpOrgConfigConfig("  global_dry_run = true"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "id", "org-123"),
					resource.TestCheckResourceAttr(resourceName, "org_id", "org-123"),
					resource.TestCheckResourceAttr(resourceName, "enabled", "true"),
					resource.TestCheckResourceAttr(resourceName, "global_dry_run", "true"),
				),
			},
			{
				// A second plan over unchanged configuration must be empty.
				Config:   dlpOrgConfigConfig("  global_dry_run = true"),
				PlanOnly: true,
			},
			{
				// In-place update of both settings.
				Config: dlpOrgConfigConfig("  enabled        = false\n  global_dry_run = false"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "enabled", "false"),
					resource.TestCheckResourceAttr(resourceName, "global_dry_run", "false"),
				),
			},
			{
				Config:   dlpOrgConfigConfig("  enabled        = false\n  global_dry_run = false"),
				PlanOnly: true,
			},
			{
				// Import the singleton by the organization id.
				Config:            dlpOrgConfigConfig("  enabled        = false\n  global_dry_run = false"),
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateId:     "org-123",
				ImportStateVerify: true,
			},
		},
	})
}

func TestDlpOrgConfigResource_adoptsServerDefaults(t *testing.T) {
	setupDlpTest(t)
	const resourceName = "barndoor_dlp_org_config.test"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Nothing configured: both settings track the platform
				// defaults without drifting.
				Config: dlpOrgConfigConfig(""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "enabled", "true"),
					resource.TestCheckResourceAttr(resourceName, "global_dry_run", "false"),
				),
			},
			{
				Config:   dlpOrgConfigConfig(""),
				PlanOnly: true,
			},
		},
	})
}

// TestDlpOrgConfigResource_destroyResetsDefaults drives an explicit destroy
// (empty config) and asserts the reset-to-defaults semantic directly, not
// just via CheckDestroy.
func TestDlpOrgConfigResource_destroyResetsDefaults(t *testing.T) {
	fake := setupDlpTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: dlpOrgConfigConfig("  enabled        = false\n  global_dry_run = true"),
			},
			{
				// Removing the resource destroys it: the fake must be back on
				// the platform defaults afterwards.
				Config: `# resource removed`,
				Check: func(*terraform.State) error {
					fake.mu.Lock()
					defer fake.mu.Unlock()
					cfg := fake.currentOrgConfig()
					if !cfg.Enabled || cfg.GlobalDryRun {
						t.Errorf("destroy left enabled=%t global_dry_run=%t, want the platform defaults",
							cfg.Enabled, cfg.GlobalDryRun)
					}
					return nil
				},
			},
		},
	})
}
