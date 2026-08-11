// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework-jsontypes/jsontypes"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// dlpDetectionEngineCapabilitiesAttrTypes describes the computed
// `capabilities` object, mirroring the dlp-service
// DetectionEngineCapabilitiesResponse.
var dlpDetectionEngineCapabilitiesAttrTypes = map[string]attr.Type{
	"provider_class":                 types.StringType,
	"pii_detection":                  types.BoolType,
	"secret_detection":               types.BoolType,
	"prompt_attack_detection":        types.BoolType,
	"span_offsets":                   types.BoolType,
	"confidence_scores":              types.BoolType,
	"native_masking":                 types.BoolType,
	"native_blocking":                types.BoolType,
	"supports_tokenization_pipeline": types.BoolType,
}

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                = &dlpDetectionEngineResource{}
	_ resource.ResourceWithConfigure   = &dlpDetectionEngineResource{}
	_ resource.ResourceWithImportState = &dlpDetectionEngineResource{}
)

// NewDlpDetectionEngineResource returns a new barndoor_dlp_detection_engine resource.
func NewDlpDetectionEngineResource() resource.Resource {
	return &dlpDetectionEngineResource{}
}

// dlpDetectionEngineResource manages a Data Protection detection engine
// through the dlp-service tenant admin REST API
// (`/api/dlp/admin/v1/detection-engines`).
type dlpDetectionEngineResource struct {
	client *client.Client
}

// dlpDetectionEngineResourceModel maps the resource schema to Go types. The
// data source reuses it — both surfaces carry the same attributes.
type dlpDetectionEngineResourceModel struct {
	ID                      types.String         `tfsdk:"id"`
	OrgID                   types.String         `tfsdk:"org_id"`
	Name                    types.String         `tfsdk:"name"`
	ProviderType            types.String         `tfsdk:"provider_type"`
	ProviderConnectionName  types.String         `tfsdk:"provider_connection_name"`
	EnabledDetectionTypes   types.List           `tfsdk:"enabled_detection_types"`
	Config                  jsontypes.Normalized `tfsdk:"config"`
	SupportedDetectionTypes types.List           `tfsdk:"supported_detection_types"`
	SupportedActions        types.List           `tfsdk:"supported_actions"`
	ProviderClass           types.String         `tfsdk:"provider_class"`
	Capabilities            types.Object         `tfsdk:"capabilities"`
	RuntimeStages           types.List           `tfsdk:"runtime_stages"`
	CreatedBy               types.String         `tfsdk:"created_by"`
	UpdatedBy               types.String         `tfsdk:"updated_by"`
	CreatedAt               types.String         `tfsdk:"created_at"`
	UpdatedAt               types.String         `tfsdk:"updated_at"`
}

func (r *dlpDetectionEngineResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dlp_detection_engine"
}

func (r *dlpDetectionEngineResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Data Protection (DLP) detection engine — the platform app calls " +
			"these **Protection Profiles**: a named binding of a detection provider (`provider_type`) to the " +
			"detection types it scans for, optionally wired to a named provider connection and provider-" +
			"specific configuration.\n\n" +
			"Detection engines are what `barndoor_dlp_enforcement_policy.detection_engine_ids` references. " +
			"One UI Protection Profile can span several engines that share a `name` across provider types — " +
			"the uniqueness key is (`name`, `provider_type`) per organization.\n\n" +
			"Supported `provider_type` values (validated by the API): `builtin_regex`, `builtin_pii_regex`, " +
			"`builtin_secrets_regex`, `custom_regex`, `presidio`, `gliner`, `rampart`, `google_dlp`, " +
			"`aws_comprehend_pii`, `azure_ai_language_pii`, `prompt_injection`, `aws_bedrock_guardrails`, " +
			"`azure_content_safety`, `code_execution`. `code_execution` and `rampart` are feature-gated per " +
			"organization (the API answers 403 when the organization is not entitled).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Detection engine UUID assigned by the API; also the `terraform import` " +
					"key, and what `barndoor_dlp_enforcement_policy.detection_engine_ids` references.",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"org_id": schema.StringAttribute{
				MarkdownDescription: "Organization the detection engine belongs to, resolved from the " +
					"provider credential's token claims.",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Display name of the detection engine (the Protection Profile name in " +
					"the app). Unique per organization **and** `provider_type` — the same name may exist for " +
					"several provider types.",
				Required: true,
				Validators: []validator.String{
					dlpNoSurroundingWhitespace,
				},
			},
			"provider_type": schema.StringAttribute{
				MarkdownDescription: "Detection provider backing the engine (see the resource description " +
					"for the supported values). Changing it updates the engine in place.",
				Required: true,
				Validators: []validator.String{
					dlpNoSurroundingWhitespace,
				},
			},
			"provider_connection_name": schema.StringAttribute{
				MarkdownDescription: "Name of the Data Protection provider connection the engine uses (for " +
					"provider types that call an external service, e.g. `presidio` or `google_dlp`). Omit " +
					"for built-in providers or to use the provider type's default connection.",
				Optional: true,
				Validators: []validator.String{
					dlpNoSurroundingWhitespace,
				},
			},
			"enabled_detection_types": schema.ListAttribute{
				MarkdownDescription: "Detection type wire names (`DETECTION_TYPE_…`, including " +
					"`barndoor_dlp_custom_detection_type.detection_type` values) this engine scans for. At " +
					"least one is required, and the API rejects names outside the built-in set and the " +
					"organization's custom detection types.",
				ElementType: types.StringType,
				Required:    true,
				Validators: []validator.List{
					listvalidator.SizeAtLeast(1),
					listvalidator.UniqueValues(),
					listvalidator.ValueStringsAre(dlpNoSurroundingWhitespace),
				},
			},
			"config": schema.StringAttribute{
				MarkdownDescription: "Provider-specific engine configuration as JSON (`jsonencode({...})`). " +
					"The platform stores it verbatim (JSONB) and defaults to `{}` when omitted; removing the " +
					"attribute resets the configuration to `{}`.",
				Optional:   true,
				CustomType: jsontypes.NormalizedType{},
			},
			"supported_detection_types": schema.ListAttribute{
				MarkdownDescription: "Every detection type wire name the engine's provider can produce, " +
					"derived by the platform from its capability tables.",
				ElementType: types.StringType,
				Computed:    true,
			},
			"supported_actions": schema.ListAttribute{
				MarkdownDescription: "Enforcement policy actions the engine supports (what " +
					"`barndoor_dlp_enforcement_policy.action` may be when the policy references this engine).",
				ElementType: types.StringType,
				Computed:    true,
			},
			"provider_class": schema.StringAttribute{
				MarkdownDescription: "Capability class of the provider: `native`, `span_detection`, or " +
					"`guardrail_intervention`.",
				Computed: true,
			},
			"capabilities": schema.SingleNestedAttribute{
				MarkdownDescription: "Capability flags of the engine's provider, derived by the platform " +
					"from `provider_type`.",
				Computed:   true,
				Attributes: dlpDetectionEngineCapabilitiesSchemaAttributes(),
			},
			"runtime_stages": schema.ListAttribute{
				MarkdownDescription: "Runtime stages the engine can evaluate.",
				ElementType:         types.StringType,
				Computed:            true,
			},
			"created_by": schema.StringAttribute{
				MarkdownDescription: "Subject that created the engine (empty when the platform could not " +
					"attribute it).",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"updated_by": schema.StringAttribute{
				MarkdownDescription: "Subject that last updated the engine.",
				Computed:            true,
			},
			"created_at": schema.StringAttribute{
				MarkdownDescription: "When the engine was created (RFC 3339).",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"updated_at": schema.StringAttribute{
				MarkdownDescription: "When the engine was last updated (RFC 3339).",
				Computed:            true,
			},
		},
	}
}

// dlpDetectionEngineCapabilitiesSchemaAttributes defines the computed
// capabilities object for both the resource and the data source.
func dlpDetectionEngineCapabilitiesSchemaAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"provider_class": schema.StringAttribute{
			MarkdownDescription: "Capability class (same value as the top-level `provider_class`).",
			Computed:            true,
		},
		"pii_detection": schema.BoolAttribute{
			MarkdownDescription: "Whether the provider detects PII.",
			Computed:            true,
		},
		"secret_detection": schema.BoolAttribute{
			MarkdownDescription: "Whether the provider detects secrets.",
			Computed:            true,
		},
		"prompt_attack_detection": schema.BoolAttribute{
			MarkdownDescription: "Whether the provider detects prompt attacks.",
			Computed:            true,
		},
		"span_offsets": schema.BoolAttribute{
			MarkdownDescription: "Whether findings carry span offsets (required for redact/mask).",
			Computed:            true,
		},
		"confidence_scores": schema.BoolAttribute{
			MarkdownDescription: "Whether findings carry confidence scores.",
			Computed:            true,
		},
		"native_masking": schema.BoolAttribute{
			MarkdownDescription: "Whether the provider masks content natively.",
			Computed:            true,
		},
		"native_blocking": schema.BoolAttribute{
			MarkdownDescription: "Whether the provider blocks content natively.",
			Computed:            true,
		},
		"supports_tokenization_pipeline": schema.BoolAttribute{
			MarkdownDescription: "Whether the engine can feed the tokenization pipeline.",
			Computed:            true,
		},
	}
}

func (r *dlpDetectionEngineResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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
	r.client = c
}

func (r *dlpDetectionEngineResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan dlpDetectionEngineResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, err := buildDlpDetectionEngineCreateRequest(ctx, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid detection engine configuration", err.Error())
		return
	}

	var engine dlpDetectionEngineResponse
	if err := doJSON(ctx, r.client, http.MethodPost, dlpAPIPrefix+"/detection-engines", body, &engine); err != nil {
		addDlpDetectionEngineAPIError(&resp.Diagnostics, "create the detection engine", err)
		return
	}
	if engine.ID == "" {
		resp.Diagnostics.AddError("Malformed Data Protection API response", "Create returned no detection engine id.")
		return
	}

	state, err := applyDlpDetectionEngineResponse(ctx, &engine, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to map the Data Protection API response", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *dlpDetectionEngineResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state dlpDetectionEngineResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var engine dlpDetectionEngineResponse
	err := doJSON(ctx, r.client, http.MethodGet,
		dlpAPIPrefix+"/detection-engines/"+state.ID.ValueString(), nil, &engine)
	if err != nil {
		if isNotFound(err) {
			// Deleted out-of-band; drop the resource so Terraform plans a
			// recreate.
			resp.State.RemoveResource(ctx)
			return
		}
		addDlpDetectionEngineAPIError(&resp.Diagnostics, "read the detection engine", err)
		return
	}

	newState, err := applyDlpDetectionEngineResponse(ctx, &engine, &state)
	if err != nil {
		resp.Diagnostics.AddError("Failed to map the Data Protection API response", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *dlpDetectionEngineResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan dlpDetectionEngineResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, err := buildDlpDetectionEngineUpdateRequest(ctx, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid detection engine configuration", err.Error())
		return
	}

	var engine dlpDetectionEngineResponse
	if err := doJSON(ctx, r.client, http.MethodPut,
		dlpAPIPrefix+"/detection-engines/"+plan.ID.ValueString(), body, &engine); err != nil {
		addDlpDetectionEngineAPIError(&resp.Diagnostics, "update the detection engine", err)
		return
	}

	newState, err := applyDlpDetectionEngineResponse(ctx, &engine, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to map the Data Protection API response", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Delete removes the detection engine. A 404 means it is already gone —
// success for a destroy. A 409 means an enforcement policy still depends on
// the engine as its only one; the API refuses rather than leaving a policy
// with zero engines.
func (r *dlpDetectionEngineResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state dlpDetectionEngineResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := doJSON(ctx, r.client, http.MethodDelete,
		dlpAPIPrefix+"/detection-engines/"+state.ID.ValueString(), nil, nil)
	if err != nil && !isNotFound(err) {
		addDlpDetectionEngineAPIError(&resp.Diagnostics, "delete the detection engine", err)
	}
}

func (r *dlpDetectionEngineResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *dlpDetectionEngineResource) requireClient(diags *diag.Diagnostics) bool {
	if r.client == nil {
		diags.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return false
	}
	return true
}

// addDlpDetectionEngineAPIError turns a DLP admin API error into an
// actionable diagnostic. action is the failed verb phrase, e.g. "create the
// detection engine".
func addDlpDetectionEngineAPIError(diags *diag.Diagnostics, action string, err error) {
	apiErr, ok := asAPIError(err)
	if !ok {
		diags.AddError("Failed to "+action, err.Error())
		return
	}

	switch apiErr.status {
	case http.StatusBadRequest:
		// The API's validation message (unsupported provider_type, unknown
		// detection type, empty name, …) is the most specific thing we can
		// show; surface it verbatim.
		diags.AddError("Detection engine rejected by the Data Protection API", apiErr.displayBody())
	case http.StatusConflict:
		if action == "delete the detection engine" {
			// The engine is the only one on at least one enforcement policy;
			// the API refuses rather than leaving a policy with zero engines.
			diags.AddError(
				"Detection engine is still referenced by an enforcement policy",
				fmt.Sprintf("Failed to %s: %s\n\nRemove the engine from every "+
					"`barndoor_dlp_enforcement_policy.detection_engine_ids` that lists it as the only "+
					"engine (or delete those policies) before destroying it.", action, apiErr.displayBody()),
			)
			return
		}
		diags.AddError(
			"Detection engine conflicts with an existing one",
			fmt.Sprintf("Failed to %s: %s\n\nDetection engines are unique per organization by (`name`, "+
				"`provider_type`); choose a different `name`, or `terraform import` the existing engine "+
				"instead of recreating it.", action, apiErr.displayBody()),
		)
	case http.StatusUnauthorized, http.StatusForbidden:
		diags.AddError(
			"Permission denied by the Data Protection API",
			fmt.Sprintf("Failed to %s: the configured credential was not accepted, or the organization is "+
				"not entitled to the requested provider type (`code_execution` and `rampart` are "+
				"feature-gated). Confirm the service-account credential's token carries the organization "+
				"claim for the organization that owns this engine.\n\nServer message: %s",
				action, apiErr.displayBody()),
		)
	default:
		diags.AddError("Failed to "+action, apiErr.Error())
	}
}

// --- request/response DTOs ---------------------------------------------------------

// dlpDetectionEngineCreateRequest mirrors the dlp-service
// CreateDetectionEngineRequest body. provider_connection_name and config are
// sent only when configured so the API's defaulting (no connection, `{}`
// config) applies otherwise.
type dlpDetectionEngineCreateRequest struct {
	Name                   string          `json:"name"`
	ProviderType           string          `json:"provider_type"`
	ProviderConnectionName *string         `json:"provider_connection_name,omitempty"`
	EnabledDetectionTypes  []string        `json:"enabled_detection_types"`
	Config                 json.RawMessage `json:"config,omitempty"`
}

// dlpDetectionEngineUpdateRequest mirrors the dlp-service
// UpdateDetectionEngineRequest body. The API applies exactly the keys
// present, so Update sends every managed field — Terraform's plan is the full
// desired state, and an omitted key would leave a removed value behind.
// provider_connection_name is the API's double-Option: an absent key keeps
// the stored value and JSON null clears it, so the key is always sent (null
// when unset). config is COALESCE-kept when absent, so it is always sent too
// (`{}` when unset — the server-side default).
type dlpDetectionEngineUpdateRequest struct {
	Name                   *string         `json:"name,omitempty"`
	ProviderType           *string         `json:"provider_type,omitempty"`
	ProviderConnectionName *string         `json:"provider_connection_name"`
	EnabledDetectionTypes  *[]string       `json:"enabled_detection_types,omitempty"`
	Config                 json.RawMessage `json:"config,omitempty"`
}

// dlpDetectionEngineCapabilitiesResponse mirrors the dlp-service
// DetectionEngineCapabilitiesResponse.
type dlpDetectionEngineCapabilitiesResponse struct {
	ProviderClass                string `json:"provider_class"`
	PiiDetection                 bool   `json:"pii_detection"`
	SecretDetection              bool   `json:"secret_detection"`
	PromptAttackDetection        bool   `json:"prompt_attack_detection"`
	SpanOffsets                  bool   `json:"span_offsets"`
	ConfidenceScores             bool   `json:"confidence_scores"`
	NativeMasking                bool   `json:"native_masking"`
	NativeBlocking               bool   `json:"native_blocking"`
	SupportsTokenizationPipeline bool   `json:"supports_tokenization_pipeline"`
}

// dlpDetectionEngineResponse mirrors the dlp-service DetectionEngineResponse.
// created_by/updated_by come back as JSON null when unattributed, which
// decodes to "".
type dlpDetectionEngineResponse struct {
	ID                      string                                 `json:"id"`
	OrgID                   string                                 `json:"org_id"`
	Name                    string                                 `json:"name"`
	ProviderType            string                                 `json:"provider_type"`
	ProviderConnectionName  *string                                `json:"provider_connection_name"`
	EnabledDetectionTypes   []string                               `json:"enabled_detection_types"`
	SupportedDetectionTypes []string                               `json:"supported_detection_types"`
	SupportedActions        []string                               `json:"supported_actions"`
	ProviderClass           string                                 `json:"provider_class"`
	Capabilities            dlpDetectionEngineCapabilitiesResponse `json:"capabilities"`
	RuntimeStages           []string                               `json:"runtime_stages"`
	Config                  json.RawMessage                        `json:"config"`
	CreatedBy               string                                 `json:"created_by"`
	UpdatedBy               string                                 `json:"updated_by"`
	CreatedAt               string                                 `json:"created_at"`
	UpdatedAt               string                                 `json:"updated_at"`
}

// dlpPlannedEngineConfig renders the planned config for a request body: the
// configured JSON text, or `{}` when unset (the server-side default, sent
// explicitly on update so removing the attribute converges — the API's
// COALESCE keeps the stored value for an absent key).
func dlpPlannedEngineConfig(config jsontypes.Normalized) (json.RawMessage, error) {
	v, ok := knownNormalized(config)
	if !ok {
		return json.RawMessage("{}"), nil
	}
	if !json.Valid([]byte(v)) {
		return nil, fmt.Errorf("config: not valid JSON")
	}
	return json.RawMessage(v), nil
}

// buildDlpDetectionEngineCreateRequest converts the planned model to the
// create body.
func buildDlpDetectionEngineCreateRequest(ctx context.Context, plan *dlpDetectionEngineResourceModel) (*dlpDetectionEngineCreateRequest, error) {
	detectionTypes, err := stringsFromList(ctx, plan.EnabledDetectionTypes)
	if err != nil {
		return nil, fmt.Errorf("enabled_detection_types: %w", err)
	}

	body := &dlpDetectionEngineCreateRequest{
		Name:                  plan.Name.ValueString(),
		ProviderType:          plan.ProviderType.ValueString(),
		EnabledDetectionTypes: nonNilStrings(detectionTypes),
	}
	if v, ok := knownString(plan.ProviderConnectionName); ok {
		body.ProviderConnectionName = &v
	}
	if v, ok := knownNormalized(plan.Config); ok {
		if !json.Valid([]byte(v)) {
			return nil, fmt.Errorf("config: not valid JSON")
		}
		body.Config = json.RawMessage(v)
	}
	return body, nil
}

// buildDlpDetectionEngineUpdateRequest converts the planned model to the
// update body, carrying the full desired state (see the request type's doc
// comment for the convergence contract).
func buildDlpDetectionEngineUpdateRequest(ctx context.Context, plan *dlpDetectionEngineResourceModel) (*dlpDetectionEngineUpdateRequest, error) {
	detectionTypes, err := stringsFromList(ctx, plan.EnabledDetectionTypes)
	if err != nil {
		return nil, fmt.Errorf("enabled_detection_types: %w", err)
	}
	config, err := dlpPlannedEngineConfig(plan.Config)
	if err != nil {
		return nil, err
	}

	name := plan.Name.ValueString()
	providerType := plan.ProviderType.ValueString()
	nonNilTypes := nonNilStrings(detectionTypes)

	body := &dlpDetectionEngineUpdateRequest{
		Name:                  &name,
		ProviderType:          &providerType,
		EnabledDetectionTypes: &nonNilTypes,
		Config:                config,
	}
	// nil serializes as JSON null, which clears the connection name (the
	// API's double-Option: absent = keep, null = clear).
	if v, ok := knownString(plan.ProviderConnectionName); ok {
		body.ProviderConnectionName = &v
	}
	return body, nil
}

// applyDlpDetectionEngineResponse maps the server's view onto a state model.
// prior is the plan (Create/Update) or previous state (Read), used to settle
// the cleared provider_connection_name and the `{}` config back to null when
// the configuration said nothing.
func applyDlpDetectionEngineResponse(ctx context.Context, engine *dlpDetectionEngineResponse, prior *dlpDetectionEngineResourceModel) (dlpDetectionEngineResourceModel, error) {
	capabilities, diags := types.ObjectValue(dlpDetectionEngineCapabilitiesAttrTypes, map[string]attr.Value{
		"provider_class":                 types.StringValue(engine.Capabilities.ProviderClass),
		"pii_detection":                  types.BoolValue(engine.Capabilities.PiiDetection),
		"secret_detection":               types.BoolValue(engine.Capabilities.SecretDetection),
		"prompt_attack_detection":        types.BoolValue(engine.Capabilities.PromptAttackDetection),
		"span_offsets":                   types.BoolValue(engine.Capabilities.SpanOffsets),
		"confidence_scores":              types.BoolValue(engine.Capabilities.ConfidenceScores),
		"native_masking":                 types.BoolValue(engine.Capabilities.NativeMasking),
		"native_blocking":                types.BoolValue(engine.Capabilities.NativeBlocking),
		"supports_tokenization_pipeline": types.BoolValue(engine.Capabilities.SupportsTokenizationPipeline),
	})
	if diags.HasError() {
		return dlpDetectionEngineResourceModel{}, fmt.Errorf("capabilities: %v", diags)
	}

	m := dlpDetectionEngineResourceModel{
		ID:                     types.StringValue(engine.ID),
		OrgID:                  types.StringValue(engine.OrgID),
		Name:                   types.StringValue(engine.Name),
		ProviderType:           types.StringValue(engine.ProviderType),
		ProviderConnectionName: optionalStringFromPtr(engine.ProviderConnectionName, prior.ProviderConnectionName),
		Config:                 normalizedFromDlpEngineConfig(engine.Config, prior.Config),
		ProviderClass:          types.StringValue(engine.ProviderClass),
		Capabilities:           capabilities,
		CreatedBy:              types.StringValue(engine.CreatedBy),
		UpdatedBy:              types.StringValue(engine.UpdatedBy),
		CreatedAt:              types.StringValue(engine.CreatedAt),
		UpdatedAt:              types.StringValue(engine.UpdatedAt),
	}

	var err error
	if m.EnabledDetectionTypes, err = listFromStrings(ctx, engine.EnabledDetectionTypes, prior.EnabledDetectionTypes); err != nil {
		return dlpDetectionEngineResourceModel{}, fmt.Errorf("enabled_detection_types: %w", err)
	}
	if m.SupportedDetectionTypes, err = computedListFromStrings(ctx, engine.SupportedDetectionTypes); err != nil {
		return dlpDetectionEngineResourceModel{}, fmt.Errorf("supported_detection_types: %w", err)
	}
	if m.SupportedActions, err = computedListFromStrings(ctx, engine.SupportedActions); err != nil {
		return dlpDetectionEngineResourceModel{}, fmt.Errorf("supported_actions: %w", err)
	}
	if m.RuntimeStages, err = computedListFromStrings(ctx, engine.RuntimeStages); err != nil {
		return dlpDetectionEngineResourceModel{}, fmt.Errorf("runtime_stages: %w", err)
	}
	return m, nil
}

// computedListFromStrings maps a wire string list to a computed-only state
// list: always a concrete list, `[]` when the server sent none (a computed
// attribute has no config to settle against, unlike listFromStrings).
func computedListFromStrings(ctx context.Context, vals []string) (types.List, error) {
	if vals == nil {
		vals = []string{}
	}
	list, diags := types.ListValueFrom(ctx, types.StringType, vals)
	if diags.HasError() {
		return types.ListNull(types.StringType), fmt.Errorf("%v", diags)
	}
	return list, nil
}

// normalizedFromDlpEngineConfig maps the wire config JSON to state, settling
// the empty object to null when the prior value was null/unknown — the server
// defaults an omitted config to `{}` and cannot distinguish "cleared" from
// "never set", so echoing `{}` where the config said nothing would be
// perpetual drift. Formatting differences against the config (JSONB
// normalizes key order and whitespace) are absorbed by jsontypes.Normalized
// semantic equality.
func normalizedFromDlpEngineConfig(raw json.RawMessage, prior jsontypes.Normalized) jsontypes.Normalized {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if prior.IsNull() || prior.IsUnknown() {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err == nil && len(obj) == 0 {
			return jsontypes.NewNormalizedNull()
		}
	}
	return jsontypes.NewNormalizedValue(string(raw))
}
