// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"testing"

	frameworkdatasource "github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func TestIdpSettingsDataSource_Metadata(t *testing.T) {
	var resp frameworkdatasource.MetadataResponse
	NewIdpSettingsDataSource().Metadata(context.Background(),
		frameworkdatasource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_idp_settings"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestIdpSettingsDataSource_Schema(t *testing.T) {
	var resp frameworkdatasource.SchemaResponse
	NewIdpSettingsDataSource().Schema(context.Background(), frameworkdatasource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{"idp_role_binding_only", "enforce_sso", "break_glass_email", "break_glass_user_id"} {
		a, ok := resp.Schema.Attributes[attr]
		if !ok {
			t.Errorf("schema missing attribute %q", attr)
			continue
		}
		if !a.IsComputed() {
			t.Errorf("%s should be Computed (the settings are read-only)", attr)
		}
	}
}

const idpSettingsDataSourceConfig = `
data "barndoor_idp_settings" "test" {}
`

func TestIdpSettingsDataSource_read(t *testing.T) {
	fake := setupIdentityTest(t)
	fake.settings = fakeIdpSettings{
		IdpRoleBindingOnly: true,
		EnforceSso:         true,
		BreakGlassEmail:    "break-glass@acme.com",
		BreakGlassUserID:   "kc-user-123",
	}
	const name = "data.barndoor_idp_settings.test"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: idpSettingsDataSourceConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(name, "idp_role_binding_only", "true"),
					resource.TestCheckResourceAttr(name, "enforce_sso", "true"),
					resource.TestCheckResourceAttr(name, "break_glass_email", "break-glass@acme.com"),
					resource.TestCheckResourceAttr(name, "break_glass_user_id", "kc-user-123"),
				),
			},
		},
	})
}

func TestIdpSettingsDataSource_readDefaults(t *testing.T) {
	setupIdentityTest(t)
	const name = "data.barndoor_idp_settings.test"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: idpSettingsDataSourceConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(name, "idp_role_binding_only", "false"),
					resource.TestCheckResourceAttr(name, "enforce_sso", "false"),
					resource.TestCheckNoResourceAttr(name, "break_glass_email"),
					resource.TestCheckNoResourceAttr(name, "break_glass_user_id"),
				),
			},
		},
	})
}
