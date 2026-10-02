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
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                   = &llmConnectionResource{}
	_ resource.ResourceWithConfigure      = &llmConnectionResource{}
	_ resource.ResourceWithImportState    = &llmConnectionResource{}
	_ resource.ResourceWithValidateConfig = &llmConnectionResource{}
)

// NewLlmConnectionResource returns a new barndoor_llm_connection resource.
func NewLlmConnectionResource() resource.Resource {
	return &llmConnectionResource{}
}

// llmConnectionResource manages an LLM Gateway credential (a "connection")
// through the llm-gateway-service admin REST API
// (`/api/llm-gateway/admin/connections`). Since BCP-3647 a provider can no
// longer hold its own key: the secret lives on a connection, and providers
// reference it with `connection_id`.
type llmConnectionResource struct {
	client *client.Client
}

// llmConnectionResourceModel maps the resource schema to Go types.
type llmConnectionResourceModel struct {
	ID                types.String         `tfsdk:"id"`
	OrgID             types.String         `tfsdk:"org_id"`
	Name              types.String         `tfsdk:"name"`
	ModelProvider     types.String         `tfsdk:"model_provider"`
	AuthType          types.String         `tfsdk:"auth_type"`
	BaseURL           types.String         `tfsdk:"base_url"`
	APIKey            types.String         `tfsdk:"api_key"`
	Credentials       jsontypes.Normalized `tfsdk:"credentials"`
	Settings          jsontypes.Normalized `tfsdk:"settings"`
	EffectiveSettings jsontypes.Normalized `tfsdk:"effective_settings"`
	KeyLast4          types.String         `tfsdk:"key_last4"`
	StoresKeyMaterial types.Bool           `tfsdk:"stores_key_material"`
	ChangeNote        types.String         `tfsdk:"change_note"`
	CreatedAt         types.String         `tfsdk:"created_at"`
	UpdatedAt         types.String         `tfsdk:"updated_at"`
}

func (r *llmConnectionResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_llm_connection"
}

func (r *llmConnectionResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an LLM Gateway **connection**: a named, shareable credential for a model " +
			"vendor that `barndoor_llm_provider` resources reference with `connection_id`. Several providers " +
			"can share one connection, so rotating its key rotates it for all of them.\n\n" +
			"The platform stores the secret (`api_key` or `credentials`) in its secret store and never " +
			"returns it in any form. Terraform keeps the configured value in state and cannot detect " +
			"out-of-band rotation. Every apply that updates the connection re-sends it, which keeps the " +
			"stored secret converged on the configuration. Only the last four characters are reported, as " +
			"`key_last4`.\n\n" +
			"Changing `base_url` moves every provider that follows the connection's endpoint, which is any " +
			"provider that doesn't set its own. Destroying a connection fails while a provider still uses it.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Connection UUID assigned by the API; also the `terraform import` key.",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"org_id": schema.StringAttribute{
				MarkdownDescription: "Organization the connection belongs to, resolved from the provider " +
					"credential's token claims.",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Human-readable display name of the connection.",
				Required:            true,
			},
			"model_provider": schema.StringAttribute{
				MarkdownDescription: "Upstream model-provider family the credential is for: `" +
					joinBackticked(llmModelProviders) + "`. Changing it forces a new connection (the API " +
					"has no update for it).",
				Required: true,
				Validators: []validator.String{
					stringvalidator.OneOf(llmModelProviders...),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"auth_type": schema.StringAttribute{
				MarkdownDescription: "How the gateway authenticates upstream. Defaults per `model_provider` " +
					"when unset: `anthropic` → `x_api_key`, `azure_openai` → `azure_api_key`, " +
					"`azure_foundry` → `azure_foundry_api_key`, `bedrock` → `aws_role`, `vertex` → " +
					"`google_adc`, all others → `bearer_api_key`. Other values include " +
					"`aws_static_credentials`, `bedrock_api_key`, `google_service_account`, " +
					"`google_service_account_impersonation` and `azure_entra_client_secret`. Updated in place.",
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"base_url": schema.StringAttribute{
				MarkdownDescription: "Upstream API base URL, for example `https://api.openai.com` or an " +
					"Azure resource endpoint. For the OpenAI-compatible families it must **not** end in `/v1`: " +
					"the gateway appends the version itself.",
				Required: true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"api_key": schema.StringAttribute{
				MarkdownDescription: "Upstream API key, for the API-key auth types. **Write-only**: the " +
					"platform never returns it, so Terraform tracks the configured value. Changing it rotates " +
					"the key in place for every provider using this connection. Conflicts with `credentials`.",
				Optional:  true,
				Sensitive: true,
				Validators: []validator.String{
					stringvalidator.ConflictsWith(path.MatchRoot("credentials")),
				},
			},
			"credentials": schema.StringAttribute{
				MarkdownDescription: "Structured credentials as a JSON object (`jsonencode({ … })`), for auth " +
					"types whose secret has several parts: `access_key_id` / `secret_access_key` for " +
					"`aws_static_credentials`, `tenant_id` / `client_id` / `client_secret` for " +
					"`azure_entra_client_secret`, or a service-account key (`client_email`, `private_key`, …) " +
					"for `google_service_account`. Write-only, like `api_key`, which it conflicts with.",
				CustomType: jsontypes.NormalizedType{},
				Optional:   true,
				Sensitive:  true,
			},
			"settings": schema.StringAttribute{
				MarkdownDescription: "Resource settings as a JSON object: what the secret authorizes, for " +
					"example `region` and `iam_role_arn` for Bedrock or `project_id` and `location` for " +
					"Vertex. Providers bound to the connection inherit these, with their own settings layered " +
					"on top. The platform adds derived keys on write, such as a generated `external_id` for " +
					"`aws_role`. Those appear in `effective_settings` and don't produce a diff here.",
				CustomType: jsontypes.NormalizedType{},
				Optional:   true,
				Computed:   true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"effective_settings": schema.StringAttribute{
				MarkdownDescription: "The settings as the platform stored them, including derived keys. For " +
					"an `aws_role` connection, `jsondecode(...).external_id` is the external ID to put in the " +
					"IAM role's trust policy.",
				CustomType: jsontypes.NormalizedType{},
				Computed:   true,
			},
			"key_last4": schema.StringAttribute{
				MarkdownDescription: "Last four characters of the stored key, for recognizing it; null when " +
					"the auth type stores no key or the tail is not yet known.",
				Computed: true,
			},
			"stores_key_material": schema.BoolAttribute{
				MarkdownDescription: "Whether this auth type stores a secret at all. `aws_role`, `google_adc` " +
					"and the impersonation types use the gateway's own identity instead.",
				Computed: true,
			},
			"created_at": schema.StringAttribute{
				MarkdownDescription: "When the connection was created (RFC 3339).",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"updated_at": schema.StringAttribute{
				MarkdownDescription: "When the connection was last updated (RFC 3339).",
				Computed:            true,
			},
			"change_note": llmChangeNoteAttribute("connection"),
		},
	}
}

// ValidateConfig rejects a base_url ending in the `/v1` the gateway appends
// itself, which the platform answers with a 400.
func (r *llmConnectionResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg llmConnectionResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	baseURL, ok := knownString(cfg.BaseURL)
	if !ok {
		return
	}
	modelProvider, ok := knownString(cfg.ModelProvider)
	if !ok || cfg.AuthType.IsUnknown() {
		return
	}
	if corrected, bad := llmRedundantVersionSuffix(baseURL, modelProvider, cfg.AuthType.ValueString()); bad {
		addLlmBaseURLVersionError(&resp.Diagnostics, baseURL, corrected)
	}
}

func (r *llmConnectionResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *llmConnectionResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan llmConnectionResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, err := buildLlmConnectionRequest(&plan, true)
	if err != nil {
		resp.Diagnostics.AddError("Invalid JSON attribute", err.Error())
		return
	}

	var conn llmConnectionResponse
	if err := doJSON(ctx, r.client, http.MethodPost, llmGatewayAPIPrefix+"/connections", body, &conn); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM connection", "create the LLM connection", err)
		return
	}
	if conn.ID == "" {
		resp.Diagnostics.AddError("Malformed LLM Gateway API response", "Create returned no connection id.")
		return
	}

	state := applyLlmConnectionResponse(&conn, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *llmConnectionResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state llmConnectionResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var conn llmConnectionResponse
	err := doJSON(ctx, r.client, http.MethodGet,
		llmGatewayAPIPrefix+"/connections/"+state.ID.ValueString(), nil, &conn)
	if err != nil {
		if isNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM connection", "read the LLM connection", err)
		return
	}

	newState := applyLlmConnectionResponse(&conn, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *llmConnectionResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan llmConnectionResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, err := buildLlmConnectionRequest(&plan, false)
	if err != nil {
		resp.Diagnostics.AddError("Invalid JSON attribute", err.Error())
		return
	}

	var conn llmConnectionResponse
	if err := doJSON(ctx, r.client, http.MethodPut,
		llmGatewayAPIPrefix+"/connections/"+plan.ID.ValueString(), body, &conn); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM connection", "update the LLM connection", err)
		return
	}

	newState := applyLlmConnectionResponse(&conn, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Delete removes the connection and its stored secret. The platform refuses
// (409) while providers still reference it. Terraform orders provider
// destroys first when they reference this resource's id, so the 409 surfaces
// only for providers managed elsewhere. A 404 means it is already gone.
func (r *llmConnectionResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state llmConnectionResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := doJSON(ctx, r.client, http.MethodDelete,
		llmGatewayAPIPrefix+"/connections/"+state.ID.ValueString(), nil, nil)
	if err != nil && !isNotFound(err) {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM connection", "delete the LLM connection", err)
	}
}

func (r *llmConnectionResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *llmConnectionResource) requireClient(diags *diag.Diagnostics) bool {
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

// llmConnectionRequest mirrors the llm-gateway CreateConnectionRequest and
// UpdateConnectionRequest bodies (model_provider is create-only). Omitted
// keys leave the corresponding column unchanged on update.
type llmConnectionRequest struct {
	Name          string          `json:"name"`
	ModelProvider string          `json:"model_provider,omitempty"`
	AuthType      *string         `json:"auth_type,omitempty"`
	BaseURL       string          `json:"base_url"`
	APIKey        *string         `json:"api_key,omitempty"`
	Credentials   json.RawMessage `json:"credentials,omitempty"`
	Settings      json.RawMessage `json:"settings,omitempty"`
	ChangeNote    *string         `json:"change_note,omitempty"`
}

// llmConnectionResponse mirrors the llm-gateway Connection response. The
// secret is never part of it; `secret_path` and the attribution fields are
// not tracked.
type llmConnectionResponse struct {
	ID                string          `json:"id"`
	OrgID             string          `json:"org_id"`
	Name              string          `json:"name"`
	ModelProvider     string          `json:"model_provider"`
	AuthType          string          `json:"auth_type"`
	BaseURL           string          `json:"base_url"`
	Settings          json.RawMessage `json:"settings"`
	KeyLast4          *string         `json:"key_last4"`
	StoresKeyMaterial bool            `json:"stores_key_material"`
	CreatedAt         string          `json:"created_at"`
	UpdatedAt         string          `json:"updated_at"`
}

// buildLlmConnectionRequest converts the planned model to the create or
// update body. The secret is re-sent whenever it is configured: the write is
// idempotent, and the API cannot report drift for it.
func buildLlmConnectionRequest(plan *llmConnectionResourceModel, create bool) (*llmConnectionRequest, error) {
	settings, err := plannedSettings(plan.Settings)
	if err != nil {
		return nil, err
	}
	credentials, err := plannedSettings(plan.Credentials)
	if err != nil {
		return nil, fmt.Errorf("credentials: %w", err)
	}
	body := &llmConnectionRequest{
		Name:        plan.Name.ValueString(),
		AuthType:    stringPtrIfKnown(plan.AuthType),
		BaseURL:     plan.BaseURL.ValueString(),
		APIKey:      stringPtrIfKnown(plan.APIKey),
		Credentials: credentials,
		Settings:    settings,
		ChangeNote:  stringPtrIfKnown(plan.ChangeNote),
	}
	if create {
		body.ModelProvider = plan.ModelProvider.ValueString()
	}
	return body, nil
}

// applyLlmConnectionResponse maps the server's view onto a state model. prior
// is the plan (Create/Update) or previous state (Read): the source of the
// write-only secret and of the configured settings.
func applyLlmConnectionResponse(conn *llmConnectionResponse, prior *llmConnectionResourceModel) llmConnectionResourceModel {
	apiKey := prior.APIKey
	if apiKey.IsUnknown() {
		apiKey = types.StringNull()
	}
	credentials := prior.Credentials
	if credentials.IsUnknown() {
		credentials = jsontypes.NewNormalizedNull()
	}
	effective := jsontypes.NewNormalizedValue("{}")
	if len(conn.Settings) > 0 && string(conn.Settings) != "null" {
		effective = jsontypes.NewNormalizedValue(string(conn.Settings))
	}

	return llmConnectionResourceModel{
		ID:                types.StringValue(conn.ID),
		OrgID:             types.StringValue(conn.OrgID),
		Name:              types.StringValue(conn.Name),
		ModelProvider:     types.StringValue(conn.ModelProvider),
		AuthType:          types.StringValue(conn.AuthType),
		BaseURL:           types.StringValue(conn.BaseURL),
		APIKey:            apiKey,
		Credentials:       credentials,
		Settings:          settleLlmSettings(conn.Settings, prior.Settings),
		EffectiveSettings: effective,
		KeyLast4:          optionalStringFromPtr(conn.KeyLast4, types.StringNull()),
		StoresKeyMaterial: types.BoolValue(conn.StoresKeyMaterial),
		ChangeNote:        prior.ChangeNote, // write-only; see llmChangeNoteAttribute
		CreatedAt:         types.StringValue(conn.CreatedAt),
		UpdatedAt:         types.StringValue(conn.UpdatedAt),
	}
}
