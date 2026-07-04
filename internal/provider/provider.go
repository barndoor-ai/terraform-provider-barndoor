// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/url"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// Ensure BarndoorProvider satisfies the provider.Provider interface.
var _ provider.Provider = &BarndoorProvider{}

// testGRPCConfigHook is a test-only seam: when non-nil, Configure invokes it
// with the assembled client.Config before constructing the client, letting
// unit tests inject an in-process gRPC target and dial options (bufconn).
// It must never be set outside tests.
var testGRPCConfigHook func(*client.Config)

// BarndoorProvider is the Barndoor Terraform provider.
type BarndoorProvider struct {
	// version is "dev" for local builds, "test" under acceptance tests, and the
	// release version when built by GoReleaser.
	version string
}

// BarndoorProviderModel maps the provider block schema to Go types.
type BarndoorProviderModel struct {
	BaseURL        types.String `tfsdk:"base_url"`
	TokenURL       types.String `tfsdk:"token_url"`
	ClientID       types.String `tfsdk:"client_id"`
	ClientSecret   types.String `tfsdk:"client_secret"`
	OrganizationID types.String `tfsdk:"organization_id"`
}

func (p *BarndoorProvider) Metadata(ctx context.Context, req provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "barndoor"
	resp.Version = p.version
}

func (p *BarndoorProvider) Schema(ctx context.Context, req provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The Barndoor provider manages Barndoor AI platform resources as code. It " +
			"authenticates with a Keycloak `client_credentials` service account and talks to the platform's " +
			"public APIs under `base_url`.",
		Attributes: map[string]schema.Attribute{
			"base_url": schema.StringAttribute{
				MarkdownDescription: "Base URL of the Barndoor platform: the host root with **no path**, e.g. " +
					"`https://platform.barndoor.ai`. The provider appends each service's API prefix itself. " +
					"May also be set via the `BARNDOOR_BASE_URL` environment variable.",
				Optional: true,
			},
			"token_url": schema.StringAttribute{
				MarkdownDescription: "OIDC token endpoint for the `client_credentials` grant, e.g. `https://auth.barndoor.ai/realms/barndoor/protocol/openid-connect/token`. May also be set via `BARNDOOR_TOKEN_URL`.",
				Optional:            true,
			},
			"client_id": schema.StringAttribute{
				MarkdownDescription: "Client ID of the Barndoor service-account credential. May also be set via `BARNDOOR_CLIENT_ID`.",
				Optional:            true,
			},
			"client_secret": schema.StringAttribute{
				MarkdownDescription: "Client secret of the Barndoor service-account credential. May also be set via `BARNDOOR_CLIENT_SECRET`. Prefer the environment variable over committing the secret to configuration.",
				Optional:            true,
				Sensitive:           true,
			},
			"organization_id": schema.StringAttribute{
				MarkdownDescription: "Barndoor organization ID (the Keycloak organization UUID) the credential is scoped to; it must match the credential's token claims. May also be set via `BARNDOOR_ORGANIZATION_ID`.",
				Optional:            true,
			},
		},
	}
}

func (p *BarndoorProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var data BarndoorProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Explicit configuration wins; fall back to the environment variable.
	baseURL := resolve(data.BaseURL, "BARNDOOR_BASE_URL")
	tokenURL := resolve(data.TokenURL, "BARNDOOR_TOKEN_URL")
	clientID := resolve(data.ClientID, "BARNDOOR_CLIENT_ID")
	clientSecret := resolve(data.ClientSecret, "BARNDOOR_CLIENT_SECRET")
	organizationID := resolve(data.OrganizationID, "BARNDOOR_ORGANIZATION_ID")

	requireConfig(resp, "base_url", baseURL, "BARNDOOR_BASE_URL")
	requireConfig(resp, "token_url", tokenURL, "BARNDOOR_TOKEN_URL")
	requireConfig(resp, "client_id", clientID, "BARNDOOR_CLIENT_ID")
	requireConfig(resp, "client_secret", clientSecret, "BARNDOOR_CLIENT_SECRET")
	requireConfig(resp, "organization_id", organizationID, "BARNDOOR_ORGANIZATION_ID")
	if resp.Diagnostics.HasError() {
		return
	}

	// Fail loudly on the pre-v0.2.0 base_url form rather than issuing requests
	// against paths that no longer exist.
	if detail := invalidBaseURLDetail(baseURL); detail != "" {
		resp.Diagnostics.AddAttributeError(
			path.Root("base_url"),
			"base_url must be the platform host root",
			detail,
		)
		return
	}

	cfg := client.Config{
		BaseURL:        baseURL,
		TokenURL:       tokenURL,
		ClientID:       clientID,
		ClientSecret:   clientSecret,
		OrganizationID: organizationID,
	}
	if testGRPCConfigHook != nil {
		testGRPCConfigHook(&cfg)
	}
	c := client.New(cfg)

	// Make the authenticated client available to resources and data sources.
	resp.ResourceData = c
	resp.DataSourceData = c
}

// Resources are registered as they are implemented.
func (p *BarndoorProvider) Resources(ctx context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewAgentResource,
		NewConnectionResource,
		NewDlpAllowListEntryResource,
		NewDlpCustomDetectionTypeResource,
		NewDlpEnforcementPolicyResource,
		NewDlpFieldControlPolicyResource,
		NewDlpOrgConfigResource,
		NewLlmModelAccessResource,
		NewLlmModelMappingResource,
		NewLlmProviderResource,
		NewLlmRateLimitResource,
		NewLlmTokenBudgetResource,
		NewLogExportResource,
		NewMcpServerResource,
		NewPolicyResource,
	}
}

// DataSources are registered as they are implemented.
func (p *BarndoorProvider) DataSources(ctx context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewAgentDataSource,
		NewLogExportAWSTrustInfoDataSource,
		NewMcpServerDataSource,
		NewPolicyDataSource,
	}
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &BarndoorProvider{version: version}
	}
}

// resolve returns the configured value when set, otherwise the environment
// variable named by envKey.
func resolve(v types.String, envKey string) string {
	if !v.IsNull() && !v.IsUnknown() {
		return v.ValueString()
	}
	return os.Getenv(envKey)
}

// invalidBaseURLDetail returns a non-empty diagnostic detail when baseURL is
// not a bare host root. Before v0.2.0 `base_url` was the system-management
// public API URL (host + /api/system-management/public/v1); the provider now
// derives each service's path itself, so a lingering path suffix means the
// practitioner is still on the old form and every request would 404. This is
// a deliberate breaking change with no compatibility shim — fail loudly with
// the migration instead.
func invalidBaseURLDetail(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Sprintf("base_url %q is not a valid URL: %s.", baseURL, err)
	}
	if p := u.EscapedPath(); p != "" && p != "/" {
		return fmt.Sprintf("As of provider v0.2.0, `base_url` is the Barndoor platform host root and the provider "+
			"appends each service's API prefix itself. Remove the path suffix %q — if you were using the old "+
			"form `https://platform.barndoor.ai/api/system-management/public/v1`, set "+
			"`base_url = \"https://platform.barndoor.ai\"` (or the equivalent `BARNDOOR_BASE_URL`) instead.", p)
	}
	return ""
}

// requireConfig records a diagnostic when a required setting is empty.
func requireConfig(resp *provider.ConfigureResponse, attr, value, envKey string) {
	if value == "" {
		resp.Diagnostics.AddAttributeError(
			path.Root(attr),
			"Missing Barndoor provider configuration",
			"The provider requires `"+attr+"` to be set in the provider block or via the `"+envKey+"` environment variable.",
		)
	}
}
