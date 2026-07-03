// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// Ensure the data source satisfies the framework interfaces it relies on.
var (
	_ datasource.DataSource                     = &mcpServerDataSource{}
	_ datasource.DataSourceWithConfigure        = &mcpServerDataSource{}
	_ datasource.DataSourceWithConfigValidators = &mcpServerDataSource{}
)

// NewMcpServerDataSource returns a new barndoor_mcp_server data source.
func NewMcpServerDataSource() datasource.DataSource {
	return &mcpServerDataSource{}
}

// mcpServerDataSource looks up an existing MCP server instance by id, name, or
// slug through the registry-service public REST API, so pre-existing servers
// can be referenced (e.g. from `barndoor_policy.mcp_server_id`) without being
// managed by Terraform.
type mcpServerDataSource struct {
	client *client.Client
}

// mcpServerDataSourceModel maps the data source schema to Go types. It carries
// the server-authoritative subset of the barndoor_mcp_server resource's
// attributes — the write-only credential and metadata attributes have no
// server-side readable value and are deliberately absent.
type mcpServerDataSourceModel struct {
	ID                     types.String `tfsdk:"id"`
	Name                   types.String `tfsdk:"name"`
	Slug                   types.String `tfsdk:"slug"`
	Status                 types.String `tfsdk:"status"`
	McpServerDirectoryID   types.String `tfsdk:"mcp_server_directory_id"`
	OauthBaseURLOverride   types.String `tfsdk:"oauth_base_url_override"`
	UsesManagedCredentials types.Bool   `tfsdk:"uses_managed_credentials"`
	Scopes                 types.List   `tfsdk:"scopes"`
}

func (d *mcpServerDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_mcp_server"
}

func (d *mcpServerDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up an existing MCP server instance in the organization's registry by " +
			"`id`, `name`, or `slug`. Use it to reference a server that is not managed by this Terraform " +
			"configuration — for example to feed `barndoor_policy.mcp_server_id`, or as the lookup step " +
			"before a `terraform import`. Credential attributes are never returned by the API and are " +
			"not part of this data source.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Server UUID. Exactly one of `id`, `name`, or `slug` must be set.",
				Optional:            true,
				Computed:            true,
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Display name of the MCP server. Exactly one of `id`, `name`, or " +
					"`slug` must be set. Matching mirrors the API's uniqueness rule: case- and " +
					"surrounding-whitespace-insensitive among the organization's non-deleted servers.",
				Optional: true,
				Computed: true,
			},
			"slug": schema.StringAttribute{
				MarkdownDescription: "URL-safe identifier generated from the name at create time. Exactly " +
					"one of `id`, `name`, or `slug` must be set.",
				Optional: true,
				Computed: true,
			},
			"status": schema.StringAttribute{
				MarkdownDescription: "Lifecycle status computed by the platform: `pending` (awaiting " +
					"credentials or an OAuth connection), `active`, or `error`.",
				Computed: true,
			},
			"mcp_server_directory_id": schema.StringAttribute{
				MarkdownDescription: "ID of the MCP server directory entry this server instantiates.",
				Computed:            true,
			},
			"oauth_base_url_override": schema.StringAttribute{
				MarkdownDescription: "Tenant-specific OAuth base URL override for the upstream provider, " +
					"when one is set.",
				Computed: true,
			},
			"uses_managed_credentials": schema.BoolAttribute{
				MarkdownDescription: "Whether the server uses Barndoor-managed OAuth credentials instead " +
					"of tenant-supplied ones.",
				Computed: true,
			},
			"scopes": schema.ListAttribute{
				MarkdownDescription: "Server-level OAuth scope override, when one is set (null means the " +
					"directory entry's default scopes apply).",
				ElementType: types.StringType,
				Computed:    true,
			},
		},
	}
}

// ConfigValidators requires exactly one of the three lookup keys.
func (d *mcpServerDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(
			path.MatchRoot("id"),
			path.MatchRoot("name"),
			path.MatchRoot("slug"),
		),
	}
}

func (d *mcpServerDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		// ProviderData is nil during the framework's early lifecycle phases
		// (schema/validation), before the provider's Configure has populated it.
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

func (d *mcpServerDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	if d.client == nil {
		resp.Diagnostics.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return
	}

	var data mcpServerDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Resolve the lookup key to a canonical GET path. A slug lookup uses the
	// dedicated by-slug endpoint; a name lookup resolves to an id first.
	getPath := ""
	switch {
	case !data.ID.IsNull():
		getPath = registryAPIPrefix + "/servers/" + data.ID.ValueString()
	case !data.Slug.IsNull():
		getPath = registryAPIPrefix + "/servers/by-slug/" + url.PathEscape(data.Slug.ValueString())
	default:
		id, ok := d.resolveServerIDByName(ctx, data.Name.ValueString(), &resp.Diagnostics)
		if !ok {
			return
		}
		getPath = registryAPIPrefix + "/servers/" + id
	}

	var server mcpServerResponse
	if err := doJSON(ctx, d.client, http.MethodGet, getPath, nil, &server); err != nil {
		if isNotFound(err) {
			resp.Diagnostics.AddError(
				"MCP server not found",
				fmt.Sprintf("No MCP server matched %s in the organization (the API 404s deleted servers). "+
					"Confirm the value, or use one of the other lookup keys.", describeMcpServerLookup(&data)),
			)
			return
		}
		addMcpServerAPIError(&resp.Diagnostics, "read the MCP server", err)
		return
	}

	if err := applyMcpServerDataSource(ctx, &server, &data); err != nil {
		resp.Diagnostics.AddError("Failed to map the registry API response", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// applyMcpServerDataSource maps the server's view onto the data source model.
// Empty optional fields settle to null — a data source has no configuration to
// disambiguate against, so absent-on-the-server simply means null.
func applyMcpServerDataSource(ctx context.Context, server *mcpServerResponse, data *mcpServerDataSourceModel) error {
	data.ID = types.StringValue(server.ID)
	data.Name = types.StringValue(server.Name)
	data.Slug = types.StringValue(server.Slug)
	data.Status = types.StringValue(server.Status)
	data.McpServerDirectoryID = types.StringValue(server.McpServerDirectoryID)
	data.OauthBaseURLOverride = optionalStringFromPtr(server.OauthBaseURLOverride, types.StringNull())
	if server.UsesManagedCredentials == nil {
		data.UsesManagedCredentials = types.BoolNull()
	} else {
		data.UsesManagedCredentials = types.BoolValue(*server.UsesManagedCredentials)
	}

	scopes, err := listFromStrings(ctx, server.Scopes, types.ListNull(types.StringType))
	if err != nil {
		return fmt.Errorf("scopes: %w", err)
	}
	data.Scopes = scopes
	return nil
}

// resolveServerIDByName finds the single non-deleted server whose name matches
// name under the API's uniqueness semantics (case- and surrounding-whitespace-
// insensitive). The list endpoint's `search` narrows server-side by substring;
// the authoritative comparison happens here.
func (d *mcpServerDataSource) resolveServerIDByName(ctx context.Context, name string, diags *diag.Diagnostics) (string, bool) {
	want := strings.TrimSpace(name)

	matches, err := searchRegistry(ctx, d.client, registryAPIPrefix+"/servers",
		url.Values{"search": []string{want}},
		func(s mcpServerListRow) bool {
			return s.DeletedAt == nil && strings.EqualFold(strings.TrimSpace(s.Name), want)
		})
	if err != nil {
		addMcpServerAPIError(diags, "search MCP servers by name", err)
		return "", false
	}

	switch len(matches) {
	case 0:
		diags.AddError(
			"MCP server not found",
			fmt.Sprintf("No MCP server named %q exists in the organization (matching is case- and "+
				"surrounding-whitespace-insensitive, and deleted servers are excluded). Confirm the name, "+
				"or look the server up by `id` or `slug`.", want),
		)
		return "", false
	case 1:
		return matches[0].ID, true
	default:
		// Unreachable while the API enforces name uniqueness among non-deleted
		// servers; guard anyway rather than silently picking one.
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, m.ID)
		}
		diags.AddError(
			"MCP server name is ambiguous",
			fmt.Sprintf("%d MCP servers matched the name %q (ids: %s). Use `id` or `slug` to "+
				"disambiguate.", len(matches), want, strings.Join(ids, ", ")),
		)
		return "", false
	}
}

// mcpServerListRow is the subset of the registry list-row shape the by-name
// lookup needs.
type mcpServerListRow struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	DeletedAt *string `json:"deleted_at"`
}

// describeMcpServerLookup renders the configured lookup key for an error
// message, e.g. `id "..."`.
func describeMcpServerLookup(data *mcpServerDataSourceModel) string {
	switch {
	case !data.ID.IsNull():
		return fmt.Sprintf("id %q", data.ID.ValueString())
	case !data.Slug.IsNull():
		return fmt.Sprintf("slug %q", data.Slug.ValueString())
	default:
		return fmt.Sprintf("name %q", data.Name.ValueString())
	}
}
