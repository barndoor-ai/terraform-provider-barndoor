// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-jsontypes/jsontypes"
	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// Ensure the data source satisfies the framework interfaces it relies on.
var (
	_ datasource.DataSource                     = &llmProviderDataSource{}
	_ datasource.DataSourceWithConfigure        = &llmProviderDataSource{}
	_ datasource.DataSourceWithConfigValidators = &llmProviderDataSource{}
)

// NewLlmProviderDataSource returns a new barndoor_llm_provider data source.
func NewLlmProviderDataSource() datasource.DataSource {
	return &llmProviderDataSource{}
}

// llmProviderDataSource looks up an existing LLM Gateway upstream provider by
// id or name through the llm-gateway-service admin REST API, so a provider
// created in the Barndoor app can be referenced (e.g. from
// `barndoor_llm_model_mapping.provider_id`) without being managed by
// Terraform.
type llmProviderDataSource struct {
	client *client.Client
}

// llmProviderDataSourceModel maps the data source schema to Go types. It
// mirrors the barndoor_llm_provider resource's server-authoritative
// attributes; the write-only `api_key` credential is never returned by the
// API (in any form) and is deliberately absent from this model and schema.
type llmProviderDataSourceModel struct {
	ID                 types.String         `tfsdk:"id"`
	OrgID              types.String         `tfsdk:"org_id"`
	Name               types.String         `tfsdk:"name"`
	ModelProvider      types.String         `tfsdk:"model_provider"`
	BaseURL            types.String         `tfsdk:"base_url"`
	AuthType           types.String         `tfsdk:"auth_type"`
	Settings           jsontypes.Normalized `tfsdk:"settings"`
	Enabled            types.Bool           `tfsdk:"enabled"`
	EnforceHealthCheck types.Bool           `tfsdk:"enforce_health_check"`
	HealthStatus       types.String         `tfsdk:"health_status"`
	HealthDetail       types.String         `tfsdk:"health_detail"`
	HealthCheckedAt    types.String         `tfsdk:"health_checked_at"`
	CreatedAt          types.String         `tfsdk:"created_at"`
	UpdatedAt          types.String         `tfsdk:"updated_at"`
}

func (d *llmProviderDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_llm_provider"
}

func (d *llmProviderDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up an existing LLM Gateway upstream provider by `id` or by `name`. Use it " +
			"to reference a provider that is not managed by this Terraform configuration — for example one " +
			"created in the Barndoor app — to attach model mappings, model-access policies, or pricing " +
			"rules, or as the lookup step before a `terraform import`. The provider's credential is never " +
			"returned by the API and is not part of this data source.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Provider UUID. Exactly one of `id` or `name` must be set.",
				Optional:            true,
				Computed:            true,
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Human-readable display name of the provider. Exactly one of `id` or " +
					"`name` must be set. Matching mirrors the API's uniqueness rule: case-insensitive " +
					"among the organization's providers.",
				Optional: true,
				Computed: true,
			},
			"org_id": schema.StringAttribute{
				MarkdownDescription: "Organization the provider belongs to.",
				Computed:            true,
			},
			"model_provider": schema.StringAttribute{
				MarkdownDescription: "Upstream model-provider family, deciding the wire protocol the " +
					"gateway speaks: `openai`, `anthropic`, `azure_openai`, `google_ai`, `bedrock`, " +
					"`vertex`, `groq`, `together`, `mistral`, `cohere`, `xai`, `fireworks`, " +
					"`perplexity`, `openrouter`, `deepseek`, or `custom`.",
				Computed: true,
			},
			"base_url": schema.StringAttribute{
				MarkdownDescription: "Upstream API base URL, e.g. `https://api.openai.com/v1`.",
				Computed:            true,
			},
			"auth_type": schema.StringAttribute{
				MarkdownDescription: "How the gateway authenticates upstream (e.g. `bearer_api_key`, " +
					"`x_api_key`, `azure_api_key`).",
				Computed: true,
			},
			"settings": schema.StringAttribute{
				MarkdownDescription: "Provider-specific settings as a JSON object, e.g. `region` for " +
					"Bedrock or `api_version` for Azure OpenAI; null when the provider has none.",
				CustomType: jsontypes.NormalizedType{},
				Computed:   true,
			},
			"enabled": schema.BoolAttribute{
				MarkdownDescription: "Operator intent: whether the provider may serve traffic. Distinct " +
					"from `health_status`, which the platform records from connectivity probes.",
				Computed: true,
			},
			"enforce_health_check": schema.BoolAttribute{
				MarkdownDescription: "Whether routing gates on the connectivity health probe.",
				Computed:            true,
			},
			"health_status": schema.StringAttribute{
				MarkdownDescription: "Observed upstream reachability recorded by the platform's " +
					"connectivity probes: `unverified`, `healthy`, or `unhealthy`.",
				Computed: true,
			},
			"health_detail": schema.StringAttribute{
				MarkdownDescription: "Human-readable reason for the last `unhealthy` probe; null otherwise.",
				Computed:            true,
			},
			"health_checked_at": schema.StringAttribute{
				MarkdownDescription: "When the last connectivity probe ran (RFC 3339); null until the " +
					"first probe.",
				Computed: true,
			},
			"created_at": schema.StringAttribute{
				MarkdownDescription: "When the provider was created (RFC 3339).",
				Computed:            true,
			},
			"updated_at": schema.StringAttribute{
				MarkdownDescription: "When the provider was last updated (RFC 3339).",
				Computed:            true,
			},
		},
	}
}

// ConfigValidators requires exactly one of the two lookup keys.
func (d *llmProviderDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(
			path.MatchRoot("id"),
			path.MatchRoot("name"),
		),
	}
}

func (d *llmProviderDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *llmProviderDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	if d.client == nil {
		resp.Diagnostics.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return
	}

	var data llmProviderDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// An id lookup uses the canonical GET; a name lookup resolves over the
	// (unpaginated) listing, which already carries the full provider objects.
	var provider llmProviderResponse
	if !data.ID.IsNull() {
		err := doJSON(ctx, d.client, http.MethodGet,
			llmGatewayAPIPrefix+"/providers/"+data.ID.ValueString(), nil, &provider)
		if err != nil {
			if isNotFound(err) {
				resp.Diagnostics.AddError(
					"LLM provider not found",
					fmt.Sprintf("No LLM provider with id %q exists in the organization. Confirm the id, or "+
						"look the provider up by `name`.", data.ID.ValueString()),
				)
				return
			}
			addLlmGatewayAPIError(&resp.Diagnostics, "LLM provider", "read the LLM provider", err)
			return
		}
	} else {
		var ok bool
		provider, ok = d.resolveProviderByName(ctx, data.Name.ValueString(), resp)
		if !ok {
			return
		}
	}

	// Map through the resource's response mapping so the two shapes can never
	// drift apart. A zero-value prior model is all-null, so empty optional
	// fields (an empty settings object, absent health fields) settle to null —
	// a data source has no configuration to disambiguate against. The prior's
	// null api_key is discarded with the rest of the resource-only fields.
	state := applyLlmProviderResponse(&provider, &llmProviderResourceModel{})
	resp.Diagnostics.Append(resp.State.Set(ctx, &llmProviderDataSourceModel{
		ID:                 state.ID,
		OrgID:              state.OrgID,
		Name:               state.Name,
		ModelProvider:      state.ModelProvider,
		BaseURL:            state.BaseURL,
		AuthType:           state.AuthType,
		Settings:           state.Settings,
		Enabled:            state.Enabled,
		EnforceHealthCheck: state.EnforceHealthCheck,
		HealthStatus:       state.HealthStatus,
		HealthDetail:       state.HealthDetail,
		HealthCheckedAt:    state.HealthCheckedAt,
		CreatedAt:          state.CreatedAt,
		UpdatedAt:          state.UpdatedAt,
	})...)
}

// resolveProviderByName finds the single provider whose name matches name
// under the API's uniqueness semantics (case-insensitive, per the
// providers_org_id_name_unique index on `lower(name)`). The list endpoint has
// no search parameter and no pagination — it returns the organization's full
// provider set — so the comparison happens entirely client-side.
func (d *llmProviderDataSource) resolveProviderByName(ctx context.Context, name string, resp *datasource.ReadResponse) (llmProviderResponse, bool) {
	var providers []llmProviderResponse
	if err := doJSON(ctx, d.client, http.MethodGet, llmGatewayAPIPrefix+"/providers", nil, &providers); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM provider", "list LLM providers to resolve the name", err)
		return llmProviderResponse{}, false
	}

	var matches []llmProviderResponse
	for _, p := range providers {
		if strings.EqualFold(p.Name, name) {
			matches = append(matches, p)
		}
	}

	switch len(matches) {
	case 0:
		resp.Diagnostics.AddError(
			"LLM provider not found",
			fmt.Sprintf("No LLM provider named %q exists in the organization (matching is "+
				"case-insensitive, mirroring the API's uniqueness rule). Confirm the name, or look the "+
				"provider up by `id`.", name),
		)
		return llmProviderResponse{}, false
	case 1:
		return matches[0], true
	default:
		// Unreachable while the API enforces name uniqueness among the
		// organization's providers; guard anyway rather than silently picking
		// one.
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, m.ID)
		}
		resp.Diagnostics.AddError(
			"LLM provider name is ambiguous",
			fmt.Sprintf("%d LLM providers matched the name %q (ids: %s). This should not happen — provider "+
				"names are unique (case-insensitively) within the organization. Use `id` to disambiguate.",
				len(matches), name, strings.Join(ids, ", ")),
		)
		return llmProviderResponse{}, false
	}
}
