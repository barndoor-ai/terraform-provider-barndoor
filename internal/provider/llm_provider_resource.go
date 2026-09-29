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
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// llmModelProviders are the upstream model-provider families the gateway can
// speak to (the `ModelProvider` enum's wire values).
var llmModelProviders = []string{
	"openai", "anthropic", "azure_openai", "azure_foundry", "google_ai", "bedrock",
	"vertex", "groq", "together", "mistral", "cohere", "xai", "fireworks",
	"perplexity", "openrouter", "deepseek", "custom",
}

// llmBillingModes / llmBillingReasons are the BillingMode / BillingReason
// enums' wire values (V69, BCP-3876). The two are independent: a reason is
// required only when the mode is not_metered.
var (
	llmBillingModes   = []string{"per_token", "not_metered"}
	llmBillingReasons = []string{"subscription", "local", "external", "other"}
)

// llmBillingNoteMaxLen matches the billing_note varchar(200) column. The API
// counts characters, not bytes, hence the UTF-8 length validator.
const llmBillingNoteMaxLen = 200

// llmProviderManagedKey is the private-state key recording which of the
// clearable billing attributes the configuration set on the last apply. See
// llmClearIfPreviouslyConfigured.
const llmProviderManagedKey = "configured_billing_attributes"

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                = &llmProviderResource{}
	_ resource.ResourceWithConfigure   = &llmProviderResource{}
	_ resource.ResourceWithImportState = &llmProviderResource{}
	_ resource.ResourceWithModifyPlan  = &llmProviderResource{}
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
	BillingMode        types.String         `tfsdk:"billing_mode"`
	BillingReason      types.String         `tfsdk:"billing_reason"`
	BillingNote        types.String         `tfsdk:"billing_note"`
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
			"supported by this resource — create those in the Barndoor app instead.\n\n" +
			"The billing attributes (`billing_mode`, `billing_reason`, `billing_note`) are left alone " +
			"unless configured, so a configuration that never mentions them does not disturb billing set " +
			"in the app. Removing `billing_reason` or `billing_note` from a configuration that set it " +
			"clears it on the platform. Removing `billing_mode` keeps the stored mode. An attribute counts " +
			"as set by the configuration once an apply has written it: adopting a value identical to the " +
			"stored one (for example right after an import) needs no apply, so it is not yet owned.",
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
					"gateway speaks: `openai`, `anthropic`, `azure_openai`, `azure_foundry`, `google_ai`, " +
					"`bedrock`, `vertex`, `groq`, `together`, `mistral`, `cohere`, `xai`, `fireworks`, " +
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
					"(`anthropic` → `x_api_key`, `azure_openai` → `azure_api_key`, `azure_foundry` → " +
					"`azure_foundry_api_key`, `bedrock` → `aws_role`, `vertex` → `google_adc`, all others → " +
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
			"billing_mode": schema.StringAttribute{
				MarkdownDescription: "Whether Barndoor calculates and reports a per-token cost for this " +
					"provider's traffic: `per_token` (the default) or `not_metered`. A `not_metered` " +
					"provider still counts and reports token usage, but records its token cost as $0, and " +
					"requires `billing_reason`.\n\n" +
					"Changing it is **not retroactive**: cost is resolved when each request is served, so " +
					"usage already recorded keeps the cost it was recorded with. Setting `per_token` on a " +
					"flat-rate provider is also not a way to see what it would have cost at API rates. It " +
					"records real cost, which appears in cost reports as actual spend and consumes spend " +
					"budgets. For the same reason, a spend (cost) budget on a `not_metered` provider never " +
					"fires; use a token budget instead. Left unchanged when removed from configuration.",
				Optional: true,
				Computed: true,
				Validators: []validator.String{
					stringvalidator.OneOf(llmBillingModes...),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"billing_reason": schema.StringAttribute{
				MarkdownDescription: "How the vendor actually bills this provider: `subscription` (a " +
					"flat-rate plan, e.g. a Claude account over OAuth passthrough), `local` (self-hosted " +
					"inference), `external` (metered, but billed through another system), or `other` (pair " +
					"it with `billing_note`). **Required** when `billing_mode` is `not_metered`, and " +
					"optional with `per_token`, where it describes a subscription that bills overages per " +
					"token. The two attributes are independent. A descriptive label only: nothing in the " +
					"billing path reads it.",
				Optional: true,
				Computed: true,
				Validators: []validator.String{
					stringvalidator.OneOf(llmBillingReasons...),
				},
				PlanModifiers: []planmodifier.String{
					llmClearIfPreviouslyConfigured{},
				},
			},
			"billing_note": schema.StringAttribute{
				MarkdownDescription: "Free-text context for the billing arrangement, at most 200 " +
					"characters. Human-readable only; never parsed or aggregated.",
				Optional: true,
				Computed: true,
				Validators: []validator.String{
					noSurroundingWhitespace,
					stringvalidator.UTF8LengthAtMost(llmBillingNoteMaxLen),
				},
				PlanModifiers: []planmodifier.String{
					llmClearIfPreviouslyConfigured{},
				},
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
	resp.Diagnostics.Append(recordConfiguredBilling(ctx, req.Config, resp.Private)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// The create endpoint has no `enabled` field (new providers are always
	// enabled); converge a `enabled = false` plan with an immediate update.
	// The body carries only `enabled`: llmProviderUpdateRequest always sends
	// the clearable billing keys, and a null there would clear what create
	// just set.
	if enabled, ok := knownBool(plan.Enabled); ok && !enabled {
		if err := doJSON(ctx, r.client, http.MethodPut, llmGatewayAPIPrefix+"/providers/"+provider.ID,
			map[string]bool{"enabled": false}, &provider); err != nil {
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
	resp.Diagnostics.Append(recordConfiguredBilling(ctx, req.Config, resp.Private)...)
}

// ModifyPlan rejects a not_metered provider with no billing_reason at plan
// time (the API answers 400, and the V69 CHECK is the backstop). It runs on
// the plan rather than the configuration because the reason may be
// unconfigured yet still set on the platform, which is valid.
func (r *llmProviderResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // destroy
	}

	var mode, reason types.String
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("billing_mode"), &mode)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("billing_reason"), &reason)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if m, ok := knownString(mode); ok && m == "not_metered" && reason.IsNull() {
		resp.Diagnostics.AddAttributeError(path.Root("billing_reason"),
			"billing_reason is required when billing_mode is \"not_metered\"",
			"A provider cannot stop recording cost without saying why. Set billing_reason to one of "+
				"\"subscription\", \"local\", \"external\" or \"other\".")
	}
}

// llmClearIfPreviouslyConfigured is the plan modifier for the clearable
// billing attributes. The platform treats an omitted field as "keep" and an
// explicit null as "clear", and Terraform cannot express that difference in
// configuration. So the provider records in private state whether the
// configuration set each attribute on the last apply:
//
//   - configured now: plan the configured value;
//   - removed after being configured: plan null, which Update sends as an
//     explicit null to clear it;
//   - never configured: keep the stored value, so billing set in the app (or
//     present at import) is left alone.
//
// Ownership is recorded by Create/Update, so it is only taken by an apply. A
// configuration adopting exactly the stored value produces no change and so no
// apply. Terraform discards plan-time private state on a no-op, and forcing an
// update would make every matching import plan non-empty. Until something else
// triggers an apply, removing that value keeps it rather than clearing it.
type llmClearIfPreviouslyConfigured struct{}

func (llmClearIfPreviouslyConfigured) Description(_ context.Context) string {
	return "Clears the value when it is removed from a configuration that set it; otherwise keeps the stored value."
}

func (m llmClearIfPreviouslyConfigured) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (llmClearIfPreviouslyConfigured) PlanModifyString(ctx context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if !req.ConfigValue.IsNull() {
		return
	}
	if req.StateValue.IsNull() {
		// Create, or nothing stored: the platform leaves it null too.
		resp.PlanValue = types.StringNull()
		return
	}

	configured, diags := configuredBilling(ctx, req.Private)
	resp.Diagnostics.Append(diags...)
	if configured[req.Path.String()] {
		resp.PlanValue = types.StringNull()
		return
	}
	resp.PlanValue = req.StateValue
}

// privateStateReader is the read half of the framework's private state,
// which both plan-modifier requests and resource responses expose.
type privateStateReader interface {
	GetKey(ctx context.Context, key string) ([]byte, diag.Diagnostics)
}

type privateStateWriter interface {
	SetKey(ctx context.Context, key string, value []byte) diag.Diagnostics
}

// configuredBilling reads which clearable billing attributes the last apply's
// configuration set. Absent (import, or state written by an older provider
// version) means none were.
func configuredBilling(ctx context.Context, private privateStateReader) (map[string]bool, diag.Diagnostics) {
	configured := map[string]bool{}
	raw, diags := private.GetKey(ctx, llmProviderManagedKey)
	if diags.HasError() || len(raw) == 0 {
		return configured, diags
	}
	if err := json.Unmarshal(raw, &configured); err != nil {
		diags.AddError("Malformed provider private state", err.Error())
	}
	return configured, diags
}

// recordConfiguredBilling stores which clearable billing attributes this
// apply's configuration set, for llmClearIfPreviouslyConfigured.
func recordConfiguredBilling(ctx context.Context, config tfsdk.Config, private privateStateWriter) diag.Diagnostics {
	configured := map[string]bool{}
	var diags diag.Diagnostics
	for _, name := range []string{"billing_reason", "billing_note"} {
		var v types.String
		diags.Append(config.GetAttribute(ctx, path.Root(name), &v)...)
		configured[name] = !v.IsNull()
	}
	if diags.HasError() {
		return diags
	}
	raw, err := json.Marshal(configured)
	if err != nil {
		diags.AddError("Failed to encode provider private state", err.Error())
		return diags
	}
	diags.Append(private.SetKey(ctx, llmProviderManagedKey, raw)...)
	return diags
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
	BillingMode        *string         `json:"billing_mode,omitempty"`
	BillingReason      *string         `json:"billing_reason,omitempty"`
	BillingNote        *string         `json:"billing_note,omitempty"`
}

// llmProviderUpdateRequest mirrors the llm-gateway UpdateProviderRequest body.
// Omitted keys leave the corresponding column unchanged, so Update sends every
// managed field — Terraform's plan is the full desired state. The two
// clearable billing keys are deliberately **not** omitempty: the API reads an
// explicit null as "clear", and a planned null only arises from nothing
// stored or from llmClearIfPreviouslyConfigured asking to clear.
type llmProviderUpdateRequest struct {
	Name               *string         `json:"name,omitempty"`
	BaseURL            *string         `json:"base_url,omitempty"`
	AuthType           *string         `json:"auth_type,omitempty"`
	APIKey             *string         `json:"api_key,omitempty"`
	Settings           json.RawMessage `json:"settings,omitempty"`
	Enabled            *bool           `json:"enabled,omitempty"`
	EnforceHealthCheck *bool           `json:"enforce_health_check,omitempty"`
	BillingMode        *string         `json:"billing_mode,omitempty"`
	BillingReason      *string         `json:"billing_reason"`
	BillingNote        *string         `json:"billing_note"`
}

// llmProviderResponse mirrors the llm-gateway Provider response. The stored
// credential is never part of it (in any form — not even masked): the API
// writes it to the platform secret store and returns only the opaque
// `secret_path`, which this resource does not track. `health_detail`,
// `health_checked_at`, `billing_reason`, `billing_note` and `catalog_slug`
// are omitted from the JSON when null.
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
	BillingMode        string          `json:"billing_mode"`
	BillingReason      *string         `json:"billing_reason"`
	BillingNote        *string         `json:"billing_note"`
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
	body.BillingMode = stringPtrIfKnown(plan.BillingMode)
	body.BillingReason = stringPtrIfKnown(plan.BillingReason)
	body.BillingNote = stringPtrIfKnown(plan.BillingNote)
	return body, nil
}

// stringPtrIfKnown converts a known, non-null types.String to a wire pointer;
// null/unknown convert to nil.
func stringPtrIfKnown(v types.String) *string {
	if s, ok := knownString(v); ok {
		return &s
	}
	return nil
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
	body.BillingMode = stringPtrIfKnown(plan.BillingMode)
	body.BillingReason = stringPtrIfKnown(plan.BillingReason)
	body.BillingNote = stringPtrIfKnown(plan.BillingNote)
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
		BillingMode:        types.StringValue(provider.BillingMode),
		BillingReason:      optionalStringFromPtr(provider.BillingReason, prior.BillingReason),
		BillingNote:        optionalStringFromPtr(provider.BillingNote, prior.BillingNote),
		HealthStatus:       types.StringValue(provider.HealthStatus),
		HealthDetail:       optionalStringFromPtr(provider.HealthDetail, prior.HealthDetail),
		HealthCheckedAt:    optionalStringFromPtr(provider.HealthCheckedAt, prior.HealthCheckedAt),
		CreatedAt:          types.StringValue(provider.CreatedAt),
		UpdatedAt:          types.StringValue(provider.UpdatedAt),
	}
}
