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
	_ datasource.DataSource                     = &mcpServerDirectoryDataSource{}
	_ datasource.DataSourceWithConfigure        = &mcpServerDirectoryDataSource{}
	_ datasource.DataSourceWithConfigValidators = &mcpServerDirectoryDataSource{}
)

// NewMcpServerDirectoryDataSource returns a new barndoor_mcp_server_directory
// data source.
func NewMcpServerDirectoryDataSource() datasource.DataSource {
	return &mcpServerDirectoryDataSource{}
}

// mcpServerDirectoryDataSource looks up an MCP server directory (catalog)
// entry by id, slug, or name through the registry-service public REST API, so
// `barndoor_mcp_server.mcp_server_directory_id` can be fed from a stable
// human-readable key instead of a hand-copied UUID.
type mcpServerDirectoryDataSource struct {
	client *client.Client
}

// mcpServerDirectoryDataSourceModel maps the data source schema to Go types.
// It deliberately carries only identification and descriptive metadata — the
// directory's OAuth/connection configuration (oauth_metadata, meta,
// credential_schema, provider options, scopes) is connection plumbing the
// lookup use case does not need.
type mcpServerDirectoryDataSourceModel struct {
	ID             types.String `tfsdk:"id"`
	Name           types.String `tfsdk:"name"`
	Slug           types.String `tfsdk:"slug"`
	Description    types.String `tfsdk:"description"`
	OrganizationID types.String `tfsdk:"organization_id"`
	Public         types.Bool   `tfsdk:"public"`
	Source         types.String `tfsdk:"source"`
	URL            types.String `tfsdk:"url"`
}

// mcpServerDirectoryResponse is the subset of the registry's directory read
// model (`MCPServerDirectoryRead`) the data source binds. The wire keys are
// snake_case (the endpoint's by-alias serialization only affects keys nested
// inside `meta`, which is not read here).
type mcpServerDirectoryResponse struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Slug           *string `json:"slug"`
	Description    *string `json:"description"`
	OrganizationID *string `json:"organization_id"`
	Public         bool    `json:"public"`
	Source         string  `json:"source"`
	URL            string  `json:"url"`
}

func (d *mcpServerDirectoryDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_mcp_server_directory"
}

func (d *mcpServerDirectoryDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up an MCP server directory (catalog) entry — a public Barndoor-managed " +
			"connector or an organization-defined one — by `id`, `slug`, or `name`. Use it to feed " +
			"`barndoor_mcp_server.mcp_server_directory_id` without hand-copying a UUID. Neither slugs nor " +
			"names are unique across the visible catalog (an organization-owned entry may reuse a public " +
			"connector's slug), so an ambiguous lookup fails loudly with the candidate ids. The entry's " +
			"OAuth/connection configuration is not exposed — only identification and descriptive metadata.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Directory entry UUID. Exactly one of `id`, `slug`, or `name` must be set.",
				Optional:            true,
				Computed:            true,
			},
			"slug": schema.StringAttribute{
				MarkdownDescription: "Canonical connector identifier (e.g. `github`, `salesforce`), when the " +
					"entry has one — organization-created entries usually do not. Exactly one of `id`, " +
					"`slug`, or `name` must be set. Matching is exact; when a public entry and an " +
					"organization-owned entry share the slug the lookup fails and lists both ids.",
				Optional: true,
				Computed: true,
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Display name of the directory entry. Exactly one of `id`, `slug`, or " +
					"`name` must be set. Names carry no uniqueness rule; the comparison is exact " +
					"(case-sensitive, surrounding whitespace ignored) and the lookup fails when several " +
					"entries share the name.",
				Optional: true,
				Computed: true,
			},
			"description": schema.StringAttribute{
				MarkdownDescription: "Human-readable description of the connector, when one is set.",
				Computed:            true,
			},
			"organization_id": schema.StringAttribute{
				MarkdownDescription: "Owning organization's UUID for an organization-created entry; null for " +
					"a public (Barndoor-managed) catalog entry.",
				Computed: true,
			},
			"public": schema.BoolAttribute{
				MarkdownDescription: "Whether the entry is part of the public catalog (visible to every " +
					"organization) rather than organization-owned.",
				Computed: true,
			},
			"source": schema.StringAttribute{
				MarkdownDescription: "Where the connector definition comes from: `barndoor`, `third_party`, " +
					"`internal`, `custom_third_party`, `custom_internal`, `local`, or `embedded`.",
				Computed: true,
			},
			"url": schema.StringAttribute{
				MarkdownDescription: "MCP endpoint URL of the upstream server (may contain `{{...}}` " +
					"template placeholders resolved per connection).",
				Computed: true,
			},
		},
	}
}

// ConfigValidators requires exactly one of the three lookup keys.
func (d *mcpServerDirectoryDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(
			path.MatchRoot("id"),
			path.MatchRoot("slug"),
			path.MatchRoot("name"),
		),
	}
}

func (d *mcpServerDirectoryDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *mcpServerDirectoryDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	if d.client == nil {
		resp.Diagnostics.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return
	}

	var data mcpServerDirectoryDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Resolve slug/name lookups to an id first, then read the canonical GET
	// endpoint, so field mapping has a single source shape.
	id := data.ID.ValueString()
	if id == "" {
		var ok bool
		if !data.Slug.IsNull() {
			id, ok = d.resolveDirectoryIDBySlug(ctx, data.Slug.ValueString(), &resp.Diagnostics)
		} else {
			id, ok = d.resolveDirectoryIDByName(ctx, data.Name.ValueString(), &resp.Diagnostics)
		}
		if !ok {
			return
		}
	}

	var dir mcpServerDirectoryResponse
	if err := doJSON(ctx, d.client, http.MethodGet, registryAPIPrefix+"/server-directory/"+id, nil, &dir); err != nil {
		if isNotFound(err) {
			resp.Diagnostics.AddError(
				"MCP server directory entry not found",
				fmt.Sprintf("No MCP server directory entry with id %q is visible to the organization (the "+
					"API 404s deleted entries and other organizations' private entries). Confirm the id, or "+
					"look the entry up by `slug` or `name`.", id),
			)
			return
		}
		resp.Diagnostics.AddError("Failed to read the MCP server directory entry", err.Error())
		return
	}

	data.ID = types.StringValue(dir.ID)
	data.Name = types.StringValue(dir.Name)
	data.Slug = optionalStringFromPtr(dir.Slug, types.StringNull())
	data.Description = optionalStringFromPtr(dir.Description, types.StringNull())
	data.OrganizationID = optionalStringFromPtr(dir.OrganizationID, types.StringNull())
	data.Public = types.BoolValue(dir.Public)
	data.Source = types.StringValue(dir.Source)
	data.URL = types.StringValue(dir.URL)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// resolveDirectoryIDBySlug finds the single visible directory entry carrying
// slug. The list endpoint has no slug filter (its `search` matches name and
// description), so this walks the unfiltered list and compares exactly
// client-side. Slugs are only unique per (organization, slug) — a public
// entry and an organization-owned one may share a slug, and that ambiguity is
// an error rather than a silent preference.
func (d *mcpServerDirectoryDataSource) resolveDirectoryIDBySlug(ctx context.Context, slug string, diags *diag.Diagnostics) (string, bool) {
	matches, err := searchRegistry(ctx, d.client, registryAPIPrefix+"/server-directory",
		url.Values{},
		func(dir mcpServerDirectoryResponse) bool {
			return dir.Slug != nil && *dir.Slug == slug
		})
	if err != nil {
		diags.AddError("Failed to search MCP server directory entries by slug", err.Error())
		return "", false
	}

	switch len(matches) {
	case 0:
		diags.AddError(
			"MCP server directory entry not found",
			fmt.Sprintf("No MCP server directory entry with slug %q is visible to the organization "+
				"(matching is exact, and organization-created entries usually carry no slug). Confirm the "+
				"slug, or look the entry up by `id` or `name`.", slug),
		)
		return "", false
	case 1:
		return matches[0].ID, true
	default:
		diags.AddError(
			"MCP server directory slug is ambiguous",
			fmt.Sprintf("%d MCP server directory entries visible to the organization carry the slug %q "+
				"(%s). Slugs are only unique within an owner — an organization-owned entry may reuse a "+
				"public connector's slug. Use `id` to disambiguate.",
				len(matches), slug, describeDirectoryCandidates(matches)),
		)
		return "", false
	}
}

// resolveDirectoryIDByName finds the single visible directory entry whose name
// matches name exactly (after trimming surrounding whitespace). The list
// endpoint's `search` narrows server-side by name/description substring; the
// exact comparison happens here because names carry no uniqueness rule.
func (d *mcpServerDirectoryDataSource) resolveDirectoryIDByName(ctx context.Context, name string, diags *diag.Diagnostics) (string, bool) {
	want := strings.TrimSpace(name)

	matches, err := searchRegistry(ctx, d.client, registryAPIPrefix+"/server-directory",
		url.Values{"search": []string{want}},
		func(dir mcpServerDirectoryResponse) bool {
			return strings.TrimSpace(dir.Name) == want
		})
	if err != nil {
		diags.AddError("Failed to search MCP server directory entries by name", err.Error())
		return "", false
	}

	switch len(matches) {
	case 0:
		diags.AddError(
			"MCP server directory entry not found",
			fmt.Sprintf("No MCP server directory entry named %q is visible to the organization. The "+
				"comparison is exact (case-sensitive); confirm the display name, or look the entry up by "+
				"`id` or `slug`.", want),
		)
		return "", false
	case 1:
		return matches[0].ID, true
	default:
		diags.AddError(
			"MCP server directory name is ambiguous",
			fmt.Sprintf("%d MCP server directory entries visible to the organization are named %q (%s). "+
				"Names are not unique; use `id` or `slug` to disambiguate.",
				len(matches), want, describeDirectoryCandidates(matches)),
		)
		return "", false
	}
}

// describeDirectoryCandidates renders an ambiguous match set for an error
// message, distinguishing public catalog entries from organization-owned ones.
func describeDirectoryCandidates(matches []mcpServerDirectoryResponse) string {
	parts := make([]string, 0, len(matches))
	for _, m := range matches {
		kind := "organization-owned"
		if m.Public {
			kind = "public catalog entry"
		}
		parts = append(parts, fmt.Sprintf("id %s: %s", m.ID, kind))
	}
	return strings.Join(parts, "; ")
}
