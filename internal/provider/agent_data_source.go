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
	_ datasource.DataSource                     = &agentDataSource{}
	_ datasource.DataSourceWithConfigure        = &agentDataSource{}
	_ datasource.DataSourceWithConfigValidators = &agentDataSource{}
)

// NewAgentDataSource returns a new barndoor_agent data source.
func NewAgentDataSource() datasource.DataSource {
	return &agentDataSource{}
}

// agentDataSource looks up an existing AI Agent registration by id or display
// name through the registry-service public REST API, so pre-existing agents
// can be referenced (e.g. from `barndoor_policy.application_ids`) without
// being managed by Terraform.
type agentDataSource struct {
	client *client.Client
}

// agentDataSourceModel maps the data source schema to Go types. It carries the
// same attributes as the barndoor_agent resource — every one is authoritative
// server-side.
type agentDataSourceModel struct {
	ID                         types.String `tfsdk:"id"`
	Name                       types.String `tfsdk:"name"`
	ApplicationDirectoryID     types.String `tfsdk:"application_directory_id"`
	WriteConfirmationsRequired types.Bool   `tfsdk:"write_confirmations_required"`
	LlmGatewayEnabled          types.Bool   `tfsdk:"llm_gateway_enabled"`
	AgentType                  types.String `tfsdk:"agent_type"`
}

func (d *agentDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_agent"
}

func (d *agentDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up an existing AI Agent registration in the organization by `id` or by " +
			"display `name`. Use it to reference an agent that is not managed by this Terraform " +
			"configuration — for example to feed `barndoor_policy.application_ids`, or as the " +
			"lookup step before a `terraform import`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Agent registration UUID. Exactly one of `id` or `name` must be set.",
				Optional:            true,
				Computed:            true,
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Display name of the AI Agent (from its directory entry). Exactly one " +
					"of `id` or `name` must be set. Display names are not guaranteed unique; the lookup " +
					"fails when several live registrations share the name — use `id` in that case.",
				Optional: true,
				Computed: true,
			},
			"application_directory_id": schema.StringAttribute{
				MarkdownDescription: "ID of the agent directory entry (the OAuth client definition) this " +
					"registration binds to the organization.",
				Computed: true,
			},
			"write_confirmations_required": schema.BoolAttribute{
				MarkdownDescription: "Whether tool calls that write require an end-user confirmation.",
				Computed:            true,
			},
			"llm_gateway_enabled": schema.BoolAttribute{
				MarkdownDescription: "Whether the agent may use the LLM Gateway.",
				Computed:            true,
			},
			"agent_type": schema.StringAttribute{
				MarkdownDescription: "Agent classification derived from the directory entry: `internal` " +
					"(organization-defined client) or `external` (dynamically-registered client).",
				Computed: true,
			},
		},
	}
}

// ConfigValidators requires exactly one of the two lookup keys.
func (d *agentDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(
			path.MatchRoot("id"),
			path.MatchRoot("name"),
		),
	}
}

func (d *agentDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *agentDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	if d.client == nil {
		resp.Diagnostics.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return
	}

	var data agentDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id := data.ID.ValueString()
	if id == "" {
		var ok bool
		id, ok = d.resolveAgentIDByName(ctx, data.Name.ValueString(), &resp.Diagnostics)
		if !ok {
			return
		}
	}

	var agent agentResponse
	if err := doJSON(ctx, d.client, http.MethodGet, registryAPIPrefix+"/agents/"+id, nil, &agent); err != nil {
		if isNotFound(err) {
			resp.Diagnostics.AddError(
				"AI Agent not found",
				fmt.Sprintf("No live AI Agent registration with id %q exists in the organization (the API "+
					"404s unregistered agents). Confirm the id, or look the agent up by `name`.", id),
			)
			return
		}
		addAgentAPIError(&resp.Diagnostics, "read the AI Agent", err)
		return
	}

	m := applyAgentResponse(&agent)
	resp.Diagnostics.Append(resp.State.Set(ctx, &agentDataSourceModel{
		ID:                         m.ID,
		Name:                       m.Name,
		ApplicationDirectoryID:     m.ApplicationDirectoryID,
		WriteConfirmationsRequired: m.WriteConfirmationsRequired,
		LlmGatewayEnabled:          m.LlmGatewayEnabled,
		AgentType:                  m.AgentType,
	})...)
}

// resolveAgentIDByName finds the single live registration whose directory
// display name matches name exactly (after trimming surrounding whitespace).
// The list endpoint's `search` narrows server-side by substring; the exact
// comparison happens here because display names are not a unique key.
func (d *agentDataSource) resolveAgentIDByName(ctx context.Context, name string, diags *diag.Diagnostics) (string, bool) {
	want := strings.TrimSpace(name)

	matches, err := searchRegistry(ctx, d.client, registryAPIPrefix+"/agents",
		url.Values{"search": []string{want}},
		func(a agentResponse) bool {
			return a.DeletedAt == nil &&
				a.ApplicationDirectory != nil &&
				strings.TrimSpace(a.ApplicationDirectory.Name) == want
		})
	if err != nil {
		addAgentAPIError(diags, "search AI Agents by name", err)
		return "", false
	}

	switch len(matches) {
	case 0:
		diags.AddError(
			"AI Agent not found",
			fmt.Sprintf("No live AI Agent registration named %q exists in the organization. The comparison "+
				"is exact (case-sensitive); confirm the display name, or look the agent up by `id`.", want),
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
			"AI Agent name is ambiguous",
			fmt.Sprintf("%d live AI Agent registrations are named %q (ids: %s). Display names are not "+
				"unique; use `id` to disambiguate.", len(matches), want, strings.Join(ids, ", ")),
		)
		return "", false
	}
}
