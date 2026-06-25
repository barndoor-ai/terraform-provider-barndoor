// Copyright (c) Barndoor AI, Inc.
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// Ensure the data source satisfies the framework interfaces it relies on.
var (
	_ datasource.DataSource              = &logExportAWSTrustInfoDataSource{}
	_ datasource.DataSourceWithConfigure = &logExportAWSTrustInfoDataSource{}
)

// NewLogExportAWSTrustInfoDataSource returns a new
// barndoor_log_export_aws_trust_info data source.
func NewLogExportAWSTrustInfoDataSource() datasource.DataSource {
	return &logExportAWSTrustInfoDataSource{}
}

// logExportAWSTrustInfoDataSource exposes the Barndoor AWS principal ARN and the
// per-destination external ID a customer needs to build the IAM role trust
// policy for the export's `iam_role` auth method.
type logExportAWSTrustInfoDataSource struct {
	client *client.Client
}

// logExportAWSTrustInfoModel maps the data source schema to Go types.
type logExportAWSTrustInfoModel struct {
	OrganizationID types.String `tfsdk:"organization_id"`
	ExportType     types.String `tfsdk:"export_type"`
	PrincipalARN   types.String `tfsdk:"principal_arn"`
	ExternalID     types.String `tfsdk:"external_id"`
}

func (d *logExportAWSTrustInfoDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_log_export_aws_trust_info"
}

func (d *logExportAWSTrustInfoDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads Barndoor's AWS principal ARN and the per-destination external ID for a " +
			"log export, so a customer can build the `aws_iam_role` trust policy for the export's `iam_role` " +
			"auth method in the same `terraform apply`. The external ID is minted and stored on first read and " +
			"is then stable for the destination.\n\n" +
			"This endpoint is only available when the `iam_role` auth method is enabled for the organization; " +
			"otherwise the read fails. See the example for the end-to-end `iam_role` wiring.",
		Attributes: map[string]schema.Attribute{
			"organization_id": schema.StringAttribute{
				MarkdownDescription: "Organization ID (Keycloak organization UUID) that owns the export. " +
					"Defaults to the provider's `organization_id`.",
				Optional: true,
				Computed: true,
			},
			"export_type": schema.StringAttribute{
				MarkdownDescription: "Export type whose trust info to read. Defaults to `" + defaultExportType + "`.",
				Optional:            true,
				Computed:            true,
			},
			"principal_arn": schema.StringAttribute{
				MarkdownDescription: "Barndoor's AWS principal ARN. Grant it `sts:AssumeRole` in the IAM role's " +
					"trust policy.",
				Computed: true,
			},
			"external_id": schema.StringAttribute{
				MarkdownDescription: "The `sts:ExternalId` minted for this destination. Require it in the IAM " +
					"role's trust policy condition so only Barndoor's calls for this destination can assume the role.",
				Computed: true,
			},
		},
	}
}

func (d *logExportAWSTrustInfoDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *logExportAWSTrustInfoDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	if d.client == nil {
		resp.Diagnostics.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return
	}

	var data logExportAWSTrustInfoModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	orgID := valueOr(data.OrganizationID, d.client.OrganizationID())
	exportType := valueOr(data.ExportType, defaultExportType)

	info, err := fetchAWSTrustInfo(ctx, d.client, orgID, exportType)
	if err != nil {
		addTrustInfoError(&resp.Diagnostics, orgID, exportType, err)
		return
	}

	data.OrganizationID = types.StringValue(orgID)
	data.ExportType = types.StringValue(exportType)
	data.PrincipalARN = types.StringValue(info.PrincipalARN)
	data.ExternalID = types.StringValue(info.ExternalID)

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// awsTrustInfoResponse mirrors the SMS GET .../destination/aws-trust-info body.
type awsTrustInfoResponse struct {
	PrincipalARN string `json:"principal_arn"`
	ExternalID   string `json:"external_id"`
}

// fetchAWSTrustInfo reads the trust info for an export's destination. The
// caller resolves orgID/exportType (including defaults) before calling.
func fetchAWSTrustInfo(ctx context.Context, c *client.Client, orgID, exportType string) (awsTrustInfoResponse, error) {
	var out awsTrustInfoResponse
	path := exportPath(orgID, exportType, "destination", "aws-trust-info")
	if err := doJSON(ctx, c, http.MethodGet, path, nil, &out); err != nil {
		return awsTrustInfoResponse{}, err
	}
	return out, nil
}

// addTrustInfoError turns the API error into an actionable diagnostic, calling
// out the two failure modes a practitioner is most likely to hit: the feature
// not being enabled (403) and the export not existing (404).
func addTrustInfoError(diags *diag.Diagnostics, orgID, exportType string, err error) {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		switch apiErr.status {
		case http.StatusForbidden:
			diags.AddError(
				"IAM-role auth is not enabled for this organization",
				fmt.Sprintf("The aws-trust-info endpoint for organization %q is only available when the "+
					"`iam_role` auth method is enabled for the organization. Enable it (or use `access_keys` "+
					"auth instead), then re-run.\n\nUnderlying error: %s", orgID, err.Error()),
			)
			return
		case http.StatusNotFound:
			diags.AddError(
				"Export not found",
				fmt.Sprintf("No %q export exists for organization %q. Confirm the organization_id and "+
					"export_type, and that the export has been provisioned for the organization.\n\n"+
					"Underlying error: %s", exportType, orgID, err.Error()),
			)
			return
		}
	}
	diags.AddError("Failed to read AWS trust info", err.Error())
}
