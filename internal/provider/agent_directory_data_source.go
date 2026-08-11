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
	_ datasource.DataSource                     = &agentDirectoryDataSource{}
	_ datasource.DataSourceWithConfigure        = &agentDirectoryDataSource{}
	_ datasource.DataSourceWithConfigValidators = &agentDirectoryDataSource{}
)

// NewAgentDirectoryDataSource returns a new barndoor_agent_directory data
// source.
func NewAgentDirectoryDataSource() datasource.DataSource {
	return &agentDirectoryDataSource{}
}

// agentDirectoryDataSource looks up an agent directory entry (the OAuth
// client definition an agent registration binds to) by id or name through the
// registry-service public REST API, so
// `barndoor_agent.application_directory_id` can be fed from the entry's name
// instead of a hand-copied UUID.
type agentDirectoryDataSource struct {
	client *client.Client
}

// agentDirectoryDataSourceModel maps the data source schema to Go types. It
// carries identification and ownership metadata only — the entry's OAuth
// client configuration (callbacks, logout URLs, CIMD document URL) is
// connection plumbing the lookup use case does not need, and client secrets
// are never readable.
type agentDirectoryDataSourceModel struct {
	ID             types.String `tfsdk:"id"`
	Name           types.String `tfsdk:"name"`
	Description    types.String `tfsdk:"description"`
	OwnerName      types.String `tfsdk:"owner_name"`
	OwnerContact   types.String `tfsdk:"owner_contact"`
	OrganizationID types.String `tfsdk:"organization_id"`
	ExternalID     types.String `tfsdk:"external_id"`
	Public         types.Bool   `tfsdk:"public"`
	AppType        types.String `tfsdk:"app_type"`
	DCR            types.Bool   `tfsdk:"dcr"`
}

// agentDirectoryResponse is the subset of the registry's
// `ApplicationDirectoryResponse` the data source binds (snake_case wire keys,
// no aliasing). `app_type` and `dcr` are only populated on the GET-by-id
// response — the list rows omit them — which is one reason lookups always
// resolve to an id and re-read.
type agentDirectoryResponse struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Description    *string `json:"description"`
	OwnerName      *string `json:"owner_name"`
	OwnerContact   *string `json:"owner_contact"`
	OrganizationID string  `json:"organization_id"`
	ExternalID     *string `json:"external_id"`
	Public         bool    `json:"public"`
	AppType        *string `json:"app_type"`
	DCR            bool    `json:"dcr"`
}

func (d *agentDirectoryDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_agent_directory"
}

func (d *agentDirectoryDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up an agent directory entry — the OAuth client definition an AI Agent " +
			"registration binds to the organization — by `id` or `name`. Use it to feed " +
			"`barndoor_agent.application_directory_id` without hand-copying a UUID. Names carry no " +
			"uniqueness rule, so an ambiguous lookup fails loudly with the candidate ids. The entry's " +
			"OAuth client configuration (callbacks, logout URLs) is not exposed, and client secrets are " +
			"never readable through the API.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Directory entry UUID. Exactly one of `id` or `name` must be set.",
				Optional:            true,
				Computed:            true,
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Display name of the directory entry. Exactly one of `id` or `name` " +
					"must be set. Names carry no uniqueness rule; the comparison is exact (case-sensitive, " +
					"surrounding whitespace ignored) and the lookup fails when several entries share the " +
					"name — use `id` in that case.",
				Optional: true,
				Computed: true,
			},
			"description": schema.StringAttribute{
				MarkdownDescription: "Human-readable description of the agent, when one is set.",
				Computed:            true,
			},
			"owner_name": schema.StringAttribute{
				MarkdownDescription: "Responsible team or person that manages the agent, when one is set.",
				Computed:            true,
			},
			"owner_contact": schema.StringAttribute{
				MarkdownDescription: "Contact information (email, Slack, etc.) for the agent owner, when " +
					"one is set.",
				Computed: true,
			},
			"organization_id": schema.StringAttribute{
				MarkdownDescription: "Owning organization's UUID.",
				Computed:            true,
			},
			"external_id": schema.StringAttribute{
				MarkdownDescription: "OAuth client identifier of the entry's managed client in the identity " +
					"provider; null for dynamically-registered (DCR/multi-client) agents.",
				Computed: true,
			},
			"public": schema.BoolAttribute{
				MarkdownDescription: "Whether the entry is visible to every organization rather than only " +
					"its owner.",
				Computed: true,
			},
			"app_type": schema.StringAttribute{
				MarkdownDescription: "OAuth application type of the entry's client (e.g. " +
					"`machine_to_machine`), when one is set.",
				Computed: true,
			},
			"dcr": schema.BoolAttribute{
				MarkdownDescription: "Whether the entry's clients are dynamically registered (DCR). " +
					"Registrations of a DCR entry surface as `agent_type = \"external\"` on " +
					"`barndoor_agent`.",
				Computed: true,
			},
		},
	}
}

// ConfigValidators requires exactly one of the two lookup keys.
func (d *agentDirectoryDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(
			path.MatchRoot("id"),
			path.MatchRoot("name"),
		),
	}
}

func (d *agentDirectoryDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *agentDirectoryDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	if d.client == nil {
		resp.Diagnostics.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return
	}

	var data agentDirectoryDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Resolve a name lookup to an id first, then read the canonical GET
	// endpoint: list rows omit app_type/dcr, so the by-id read is the only
	// complete source shape.
	id := data.ID.ValueString()
	if id == "" {
		var ok bool
		id, ok = d.resolveDirectoryIDByName(ctx, data.Name.ValueString(), &resp.Diagnostics)
		if !ok {
			return
		}
	}

	var dir agentDirectoryResponse
	if err := doJSON(ctx, d.client, http.MethodGet, registryAPIPrefix+"/agent-directory/"+id, nil, &dir); err != nil {
		if isNotFound(err) {
			resp.Diagnostics.AddError(
				"Agent directory entry not found",
				fmt.Sprintf("No agent directory entry with id %q is visible to the organization (the API "+
					"404s deleted entries and other organizations' private entries). Confirm the id, or "+
					"look the entry up by `name`.", id),
			)
			return
		}
		resp.Diagnostics.AddError("Failed to read the agent directory entry", err.Error())
		return
	}

	data.ID = types.StringValue(dir.ID)
	data.Name = types.StringValue(dir.Name)
	data.Description = optionalStringFromPtr(dir.Description, types.StringNull())
	data.OwnerName = optionalStringFromPtr(dir.OwnerName, types.StringNull())
	data.OwnerContact = optionalStringFromPtr(dir.OwnerContact, types.StringNull())
	data.OrganizationID = types.StringValue(dir.OrganizationID)
	data.ExternalID = optionalStringFromPtr(dir.ExternalID, types.StringNull())
	data.Public = types.BoolValue(dir.Public)
	data.AppType = optionalStringFromPtr(dir.AppType, types.StringNull())
	data.DCR = types.BoolValue(dir.DCR)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// resolveDirectoryIDByName finds the single visible directory entry whose name
// matches name exactly (after trimming surrounding whitespace). The list
// endpoint's `search` narrows server-side by name/description substring; the
// exact comparison happens here because names carry no uniqueness rule.
func (d *agentDirectoryDataSource) resolveDirectoryIDByName(ctx context.Context, name string, diags *diag.Diagnostics) (string, bool) {
	want := strings.TrimSpace(name)

	matches, err := searchRegistry(ctx, d.client, registryAPIPrefix+"/agent-directory",
		url.Values{"search": []string{want}},
		func(dir agentDirectoryResponse) bool {
			return strings.TrimSpace(dir.Name) == want
		})
	if err != nil {
		diags.AddError("Failed to search agent directory entries by name", err.Error())
		return "", false
	}

	switch len(matches) {
	case 0:
		diags.AddError(
			"Agent directory entry not found",
			fmt.Sprintf("No agent directory entry named %q is visible to the organization. The comparison "+
				"is exact (case-sensitive); confirm the display name, or look the entry up by `id`.", want),
		)
		return "", false
	case 1:
		return matches[0].ID, true
	default:
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, m.ID)
		}
		diags.AddError(
			"Agent directory name is ambiguous",
			fmt.Sprintf("%d agent directory entries visible to the organization are named %q (ids: %s). "+
				"Names are not unique; use `id` to disambiguate.", len(matches), want, strings.Join(ids, ", ")),
		)
		return "", false
	}
}
