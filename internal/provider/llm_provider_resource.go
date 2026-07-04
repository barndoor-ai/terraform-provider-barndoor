// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework-jsontypes/jsontypes"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// llmModelProviders are the upstream model-provider families the gateway can
// speak to (the `ModelProvider` enum's wire values).
var llmModelProviders = []string{
	"openai", "anthropic", "azure_openai", "google_ai", "bedrock", "vertex",
	"groq", "together", "mistral", "cohere", "xai", "fireworks", "perplexity",
	"openrouter", "deepseek", "custom",
}

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                = &llmProviderResource{}
	_ resource.ResourceWithConfigure   = &llmProviderResource{}
	_ resource.ResourceWithImportState = &llmProviderResource{}
)

// NewLlmProviderResource returns a new barndoor_llm_provider resource.
func NewLlmProviderResource() resource.Resource {
	return &llmProviderResource{}
}

// llmProviderResource manages an LLM Gateway upstream provider through the
// llm-gateway-service admin REST API (`/api/llm-gateway/admin/providers`).
type llmProviderResource struct {
	client *client.Client
}

// llmProviderResourceModel maps the resource schema to Go types.
type llmProviderResourceModel struct {
	ID                 types.String         `tfsdk:"id"`
	OrgID              types.String         `tfsdk:"org_id"`
	Name               types.String         `tfsdk:"name"`
	ModelProvider      types.String         `tfsdk:"model_provider"`
	BaseURL            types.String         `tfsdk:"base_url"`
	AuthType           types.String         `tfsdk:"auth_type"`
	APIKey             types.String         `tfsdk:"api_key"`
	Settings           jsontypes.Normalized `tfsdk:"settings"`
	Enabled            types.Bool           `tfsdk:"enabled"`
	EnforceHealthCheck types.Bool           `tfsdk:"enforce_health_check"`
	HealthStatus       types.String         `tfsdk:"health_status"`
	HealthDetail       types.String         `tfsdk:"health_detail"`
	HealthCheckedAt    types.String         `tfsdk:"health_checked_at"`
	CreatedAt          types.String         `tfsdk:"created_at"`
	UpdatedAt          types.String         `tfsdk:"updated_at"`
}

func (r *llmProviderResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_llm_provider"
}

func (r *llmProviderResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an LLM Gateway upstream provider: a named connection to a model " +
			"vendor (OpenAI, Anthropic, Bedrock, …) that model mappings route traffic to.\n\n" +
			"The credential (`api_key`) is **write-only**: the platform stores it in its secret store and " +
			"never returns it in any form, so Terraform tracks the configured value and cannot detect " +
			"out-of-band rotation. Changing it rotates the credential in place and re-probes connectivity.\n\n" +
			"Providers backed by a **shared connection** (`connection_id` on the platform API) and " +
			"structured non-API-key credentials (AWS role / static credentials, Google ADC) are not yet " +
			"supported by this resource — create those in the Barndoor app instead.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Provider UUID assigned by the API; also the `terraform import` key.",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"org_id": schema.StringAttribute{
				MarkdownDescription: "Organization the provider belongs to, resolved from the provider " +
					"credential's token claims.",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Human-readable display name of the provider.",
				Required:            true,
			},
			"model_provider": schema.StringAttribute{
				MarkdownDescription: "Upstream model-provider family, deciding the wire protocol the " +
					"gateway speaks: `openai`, `anthropic`, `azure_openai`, `google_ai`, `bedrock`, " +
					"`vertex`, `groq`, `together`, `mistral`, `cohere`, `xai`, `fireworks`, " +
					"`perplexity`, `openrouter`, `deepseek`, or `custom`. Changing it forces a new " +
					"provider (the API has no update for it).",
				Required: true,
				Validators: []validator.String{
					stringvalidator.OneOf(llmModelProviders...),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"base_url": schema.StringAttribute{
				MarkdownDescription: "Upstream API base URL, e.g. `https://api.openai.com/v1`.",
				Required:            true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"auth_type": schema.StringAttribute{
				MarkdownDescription: "How the gateway authenticates upstream (e.g. `bearer_api_key`, " +
					"`x_api_key`, `azure_api_key`). Defaults per `model_provider` when unset " +
					"(`anthropic` → `x_api_key`, `azure_openai` → `azure_api_key`, most others → " +
					"`bearer_api_key`).",
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"api_key": schema.StringAttribute{
				MarkdownDescription: "Upstream API key. Write-only — the platform stores it in its secret " +
					"store and never echoes it back; changing it rotates the credential in place.",
				Optional:  true,
				Sensitive: true,
			},
			"settings": schema.StringAttribute{
				MarkdownDescription: "Provider-specific settings as a JSON object " +
					"(`jsonencode({ … })`), e.g. `region` for Bedrock or `api_version` for Azure " +
					"OpenAI. The API normalizes some shapes (it may add derived keys), and the " +
					"normalized form is what Terraform tracks — author settings in their normalized " +
					"form to avoid perpetual diffs.",
				CustomType: jsontypes.NormalizedType{},
				Optional:   true,
				Computed:   true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"enabled": schema.BoolAttribute{
				MarkdownDescription: "Operator intent: whether the provider may serve traffic. Defaults " +
					"to `true`. Distinct from `health_status`, which the platform records from " +
					"connectivity probes.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
			},
			"enforce_health_check": schema.BoolAttribute{
				MarkdownDescription: "Whether routing gates on the connectivity health probe. Defaults to " +
					"`true`; set `false` to serve the provider even while its probe fails.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
			},
			"health_status": schema.StringAttribute{
				MarkdownDescription: "Observed upstream reachability recorded by the platform's " +
					"connectivity probes: `unverified`, `healthy`, or `unhealthy`. Refreshed on every " +
					"read; not configurable.",
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
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"updated_at": schema.StringAttribute{
				MarkdownDescription: "When the provider was last updated (RFC 3339).",
				Computed:            true,
			},
		},
	}
}

func (r *llmProviderResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
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
	r.client = c
}

func (r *llmProviderResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan llmProviderResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, err := buildLlmProviderCreateRequest(&plan)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("settings"), "Invalid settings JSON", err.Error())
		return
	}

	var provider llmProviderResponse
	if err := doJSON(ctx, r.client, http.MethodPost, llmGatewayAPIPrefix+"/providers", body, &provider); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM provider", "create the LLM provider", err)
		return
	}
	if provider.ID == "" {
		resp.Diagnostics.AddError("Malformed LLM Gateway API response", "Create returned no provider id.")
		return
	}

	// Record the created provider before any follow-up call so a failure
	// below still leaves it tracked rather than orphaned.
	state := applyLlmProviderResponse(&provider, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// The create endpoint has no `enabled` field (new providers are always
	// enabled); converge a `enabled = false` plan with an immediate update.
	if enabled, ok := knownBool(plan.Enabled); ok && !enabled {
		disable := false
		if err := doJSON(ctx, r.client, http.MethodPut, llmGatewayAPIPrefix+"/providers/"+provider.ID,
			&llmProviderUpdateRequest{Enabled: &disable}, &provider); err != nil {
			addLlmGatewayAPIError(&resp.Diagnostics, "LLM provider", "disable the LLM provider after create", err)
			return
		}
		state = applyLlmProviderResponse(&provider, &plan)
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
	}
}

func (r *llmProviderResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state llmProviderResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var provider llmProviderResponse
	err := doJSON(ctx, r.client, http.MethodGet,
		llmGatewayAPIPrefix+"/providers/"+state.ID.ValueString(), nil, &provider)
	if err != nil {
		if isNotFound(err) {
			// Deleted out-of-band; drop the resource so Terraform plans a
			// recreate.
			resp.State.RemoveResource(ctx)
			return
		}
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM provider", "read the LLM provider", err)
		return
	}

	newState := applyLlmProviderResponse(&provider, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *llmProviderResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan llmProviderResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, err := buildLlmProviderUpdateRequest(&plan)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("settings"), "Invalid settings JSON", err.Error())
		return
	}

	var provider llmProviderResponse
	if err := doJSON(ctx, r.client, http.MethodPut,
		llmGatewayAPIPrefix+"/providers/"+plan.ID.ValueString(), body, &provider); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM provider", "update the LLM provider", err)
		return
	}

	newState := applyLlmProviderResponse(&provider, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Delete removes the provider (the platform also deletes its stored
// credential). A 404 means it is already gone — success for a destroy.
func (r *llmProviderResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state llmProviderResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := doJSON(ctx, r.client, http.MethodDelete,
		llmGatewayAPIPrefix+"/providers/"+state.ID.ValueString(), nil, nil)
	if err != nil && !isNotFound(err) {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM provider", "delete the LLM provider", err)
	}
}

func (r *llmProviderResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *llmProviderResource) requireClient(diags *diag.Diagnostics) bool {
	if r.client == nil {
		diags.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return false
	}
	return true
}

// --- request/response DTOs ---------------------------------------------------

// llmProviderCreateRequest mirrors the llm-gateway CreateProviderRequest body
// (the subset this resource manages — catalog_id, connection_id, models, and
// structured credentials are out of scope).
type llmProviderCreateRequest struct {
	Name               string          `json:"name"`
	ModelProvider      string          `json:"model_provider"`
	BaseURL            string          `json:"base_url"`
	AuthType           *string         `json:"auth_type,omitempty"`
	APIKey             *string         `json:"api_key,omitempty"`
	Settings           json.RawMessage `json:"settings,omitempty"`
	EnforceHealthCheck *bool           `json:"enforce_health_check,omitempty"`
}

// llmProviderUpdateRequest mirrors the llm-gateway UpdateProviderRequest body.
// Omitted keys leave the corresponding column unchanged, so Update sends every
// managed field — Terraform's plan is the full desired state.
type llmProviderUpdateRequest struct {
	Name               *string         `json:"name,omitempty"`
	BaseURL            *string         `json:"base_url,omitempty"`
	AuthType           *string         `json:"auth_type,omitempty"`
	APIKey             *string         `json:"api_key,omitempty"`
	Settings           json.RawMessage `json:"settings,omitempty"`
	Enabled            *bool           `json:"enabled,omitempty"`
	EnforceHealthCheck *bool           `json:"enforce_health_check,omitempty"`
}

// llmProviderResponse mirrors the llm-gateway Provider response. The stored
// credential is never part of it (in any form — not even masked): the API
// writes it to the platform secret store and returns only the opaque
// `secret_path`, which this resource does not track. `health_detail`,
// `health_checked_at`, and `catalog_slug` are omitted from the JSON when
// null.
type llmProviderResponse struct {
	ID                 string          `json:"id"`
	OrgID              string          `json:"org_id"`
	Name               string          `json:"name"`
	ModelProvider      string          `json:"model_provider"`
	AuthType           string          `json:"auth_type"`
	BaseURL            string          `json:"base_url"`
	Enabled            bool            `json:"enabled"`
	Settings           json.RawMessage `json:"settings"`
	EnforceHealthCheck bool            `json:"enforce_health_check"`
	HealthStatus       string          `json:"health_status"`
	HealthDetail       *string         `json:"health_detail"`
	HealthCheckedAt    *string         `json:"health_checked_at"`
	CreatedAt          string          `json:"created_at"`
	UpdatedAt          string          `json:"updated_at"`
}

// plannedSettings converts the planned settings attribute to the wire JSON;
// nil when unset (the API then defaults to `{}`).
func plannedSettings(settings jsontypes.Normalized) (json.RawMessage, error) {
	s, ok := knownNormalized(settings)
	if !ok {
		return nil, nil
	}
	if !json.Valid([]byte(s)) {
		return nil, fmt.Errorf("settings is not valid JSON: %q", s)
	}
	return json.RawMessage(s), nil
}

// buildLlmProviderCreateRequest converts the planned model to the create body.
func buildLlmProviderCreateRequest(plan *llmProviderResourceModel) (*llmProviderCreateRequest, error) {
	settings, err := plannedSettings(plan.Settings)
	if err != nil {
		return nil, err
	}
	body := &llmProviderCreateRequest{
		Name:          plan.Name.ValueString(),
		ModelProvider: plan.ModelProvider.ValueString(),
		BaseURL:       plan.BaseURL.ValueString(),
		Settings:      settings,
	}
	if v, ok := knownString(plan.AuthType); ok {
		body.AuthType = &v
	}
	if v, ok := knownString(plan.APIKey); ok {
		body.APIKey = &v
	}
	if v, ok := knownBool(plan.EnforceHealthCheck); ok {
		body.EnforceHealthCheck = &v
	}
	return body, nil
}

// buildLlmProviderUpdateRequest converts the planned model to the update
// body, carrying the full desired state. The credential is re-sent whenever
// it is configured: the write is idempotent, and it keeps the stored secret
// converged on the configuration (the API cannot report drift for it).
func buildLlmProviderUpdateRequest(plan *llmProviderResourceModel) (*llmProviderUpdateRequest, error) {
	settings, err := plannedSettings(plan.Settings)
	if err != nil {
		return nil, err
	}
	name := plan.Name.ValueString()
	baseURL := plan.BaseURL.ValueString()
	body := &llmProviderUpdateRequest{
		Name:     &name,
		BaseURL:  &baseURL,
		Settings: settings,
	}
	if v, ok := knownString(plan.AuthType); ok {
		body.AuthType = &v
	}
	if v, ok := knownString(plan.APIKey); ok {
		body.APIKey = &v
	}
	if v, ok := knownBool(plan.Enabled); ok {
		body.Enabled = &v
	}
	if v, ok := knownBool(plan.EnforceHealthCheck); ok {
		body.EnforceHealthCheck = &v
	}
	return body, nil
}

// applyLlmProviderResponse maps the server's view onto a state model. prior
// is the plan (Create/Update) or previous state (Read) — the source of the
// write-only credential and the settings null settling.
func applyLlmProviderResponse(provider *llmProviderResponse, prior *llmProviderResourceModel) llmProviderResourceModel {
	// An empty settings object settles back to null when the configuration
	// said nothing — the server materializes `{}` for an omitted settings.
	settings := jsontypes.NewNormalizedNull()
	if len(provider.Settings) > 0 {
		raw := string(provider.Settings)
		priorUnset := prior.Settings.IsNull() || prior.Settings.IsUnknown()
		if raw != "{}" || !priorUnset {
			settings = jsontypes.NewNormalizedValue(raw)
		}
	}

	apiKey := prior.APIKey
	if apiKey.IsUnknown() {
		apiKey = types.StringNull()
	}

	return llmProviderResourceModel{
		ID:                 types.StringValue(provider.ID),
		OrgID:              types.StringValue(provider.OrgID),
		Name:               types.StringValue(provider.Name),
		ModelProvider:      types.StringValue(provider.ModelProvider),
		BaseURL:            types.StringValue(provider.BaseURL),
		AuthType:           types.StringValue(provider.AuthType),
		APIKey:             apiKey,
		Settings:           settings,
		Enabled:            types.BoolValue(provider.Enabled),
		EnforceHealthCheck: types.BoolValue(provider.EnforceHealthCheck),
		HealthStatus:       types.StringValue(provider.HealthStatus),
		HealthDetail:       optionalStringFromPtr(provider.HealthDetail, prior.HealthDetail),
		HealthCheckedAt:    optionalStringFromPtr(provider.HealthCheckedAt, prior.HealthCheckedAt),
		CreatedAt:          types.StringValue(provider.CreatedAt),
		UpdatedAt:          types.StringValue(provider.UpdatedAt),
	}
}
