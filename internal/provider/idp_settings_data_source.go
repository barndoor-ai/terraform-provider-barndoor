// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// Ensure the data source satisfies the framework interfaces it relies on.
var (
	_ datasource.DataSource              = &idpSettingsDataSource{}
	_ datasource.DataSourceWithConfigure = &idpSettingsDataSource{}
)

// NewIdpSettingsDataSource returns a new barndoor_idp_settings data source.
func NewIdpSettingsDataSource() datasource.DataSource {
	return &idpSettingsDataSource{}
}

// idpSettingsDataSource reads the organization's IdP-related settings through
// the identity-service public REST API (`/api/identity/public/v1/idp/settings`).
// The settings are read-only on this surface: changing them (SSO enforcement,
// break-glass staging) is an interactive, portal-only ceremony.
type idpSettingsDataSource struct {
	client *client.Client
}

// idpSettingsDataSourceModel maps the data source schema to Go types.
type idpSettingsDataSourceModel struct {
	IdpRoleBindingOnly types.Bool   `tfsdk:"idp_role_binding_only"`
	EnforceSso         types.Bool   `tfsdk:"enforce_sso"`
	BreakGlassEmail    types.String `tfsdk:"break_glass_email"`
	BreakGlassUserID   types.String `tfsdk:"break_glass_user_id"`
}

func (d *idpSettingsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_idp_settings"
}

func (d *idpSettingsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads the organization's IdP-related settings: whether role binding is " +
			"restricted to IdP group mappings, whether SSO is enforced (password login disabled), and the " +
			"break-glass emergency admin account. These settings are **read-only** on the public API — " +
			"enabling SSO enforcement and staging the break-glass account are irreversible, member-impacting " +
			"ceremonies performed in the Barndoor app.",
		Attributes: map[string]schema.Attribute{
			"idp_role_binding_only": schema.BoolAttribute{
				MarkdownDescription: "Whether user roles can only be set via IdP group mappings (customer " +
					"admins cannot change roles manually).",
				Computed: true,
			},
			"enforce_sso": schema.BoolAttribute{
				MarkdownDescription: "Whether SSO is enforced for the organization (password login disabled).",
				Computed:            true,
			},
			"break_glass_email": schema.StringAttribute{
				MarkdownDescription: "Email of the dedicated emergency admin account that bypasses SSO " +
					"enforcement, when one is staged.",
				Computed: true,
			},
			"break_glass_user_id": schema.StringAttribute{
				MarkdownDescription: "Keycloak user ID of the break-glass account, when resolved.",
				Computed:            true,
			},
		},
	}
}

func (d *idpSettingsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		// Configure is called before the provider is configured (e.g. during
		// schema validation); nothing to wire up yet.
		return
	}

	c, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected provider data type",
			fmt.Sprintf("Expected *client.Client, got %T. This is a bug in the provider.", req.ProviderData),
		)
		return
	}
	d.client = c
}

func (d *idpSettingsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	if d.client == nil {
		resp.Diagnostics.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return
	}

	var settings idpSettingsResponse
	if err := doJSON(ctx, d.client, http.MethodGet, identityAPIPrefix+"/idp/settings", nil, &settings); err != nil {
		addIdpAPIError(&resp.Diagnostics, "read the IdP settings", err)
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &idpSettingsDataSourceModel{
		IdpRoleBindingOnly: types.BoolValue(settings.IdpRoleBindingOnly),
		EnforceSso:         types.BoolValue(settings.EnforceSso),
		BreakGlassEmail:    types.StringPointerValue(settings.BreakGlassEmail),
		BreakGlassUserID:   types.StringPointerValue(settings.BreakGlassUserID),
	})...)
}

// idpSettingsResponse mirrors identity-service's IdpSettingsResponse.
type idpSettingsResponse struct {
	IdpRoleBindingOnly bool    `json:"idp_role_binding_only"`
	EnforceSso         bool    `json:"enforce_sso"`
	BreakGlassEmail    *string `json:"break_glass_email"`
	BreakGlassUserID   *string `json:"break_glass_user_id"`
}
