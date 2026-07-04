// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework-jsontypes/jsontypes"
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

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                   = &dlpFieldControlPolicyResource{}
	_ resource.ResourceWithConfigure      = &dlpFieldControlPolicyResource{}
	_ resource.ResourceWithValidateConfig = &dlpFieldControlPolicyResource{}
	_ resource.ResourceWithImportState    = &dlpFieldControlPolicyResource{}
)

// NewDlpFieldControlPolicyResource returns a new barndoor_dlp_field_control_policy resource.
func NewDlpFieldControlPolicyResource() resource.Resource {
	return &dlpFieldControlPolicyResource{}
}

// dlpFieldControlPolicyResource manages a Data Protection field control
// policy through the dlp-service tenant admin REST API
// (`/api/dlp/admin/v1/field-control-policies`).
//
// The API keys one policy per (organization, MCP server) and its POST is an
// upsert against that key, always answering 201. Silently adopting — and
// overwriting — a policy someone else created for the same server would be
// surprising for a declarative tool, so Create treats an existing policy for
// the planned `mcp_server_id` as a conflict and directs the practitioner to
// `terraform import` instead.
type dlpFieldControlPolicyResource struct {
	client *client.Client
}

// dlpFieldControlPolicyResourceModel maps the resource schema to Go types.
type dlpFieldControlPolicyResourceModel struct {
	ID          types.String         `tfsdk:"id"`
	OrgID       types.String         `tfsdk:"org_id"`
	McpServerID types.String         `tfsdk:"mcp_server_id"`
	Name        types.String         `tfsdk:"name"`
	Enabled     types.Bool           `tfsdk:"enabled"`
	Rules       jsontypes.Normalized `tfsdk:"rules"`
	Version     types.Int64          `tfsdk:"version"`
	CreatedBy   types.String         `tfsdk:"created_by"`
	UpdatedBy   types.String         `tfsdk:"updated_by"`
	CreatedAt   types.String         `tfsdk:"created_at"`
	UpdatedAt   types.String         `tfsdk:"updated_at"`
}

func (r *dlpFieldControlPolicyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dlp_field_control_policy"
}

func (r *dlpFieldControlPolicyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the Data Protection (DLP) field control policy of one MCP server: " +
			"per-tool rules that pass, redact, or block specific fields of tool **output** payloads.\n\n" +
			"The platform keeps at most one field control policy per MCP server and organization. Creating " +
			"this resource fails when a policy already exists for the server — `terraform import` it " +
			"instead of recreating it, so out-of-band rules are not silently overwritten.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Policy UUID assigned by the API; also the `terraform import` key.",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"org_id": schema.StringAttribute{
				MarkdownDescription: "Organization the policy belongs to, resolved from the provider " +
					"credential's token claims.",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"mcp_server_id": schema.StringAttribute{
				MarkdownDescription: "MCP server the policy applies to. One policy per server and " +
					"organization; changing it forces a new policy.",
				Required: true,
				Validators: []validator.String{
					dlpNoSurroundingWhitespace,
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Policy name.",
				Required:            true,
				Validators: []validator.String{
					dlpNoSurroundingWhitespace,
				},
			},
			"enabled": schema.BoolAttribute{
				MarkdownDescription: "Whether the policy is enforced. Defaults to `true`.",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
			},
			"rules": schema.StringAttribute{
				MarkdownDescription: "Field control rules as a JSON array (`jsonencode([...])`). Each rule " +
					"is an object with `tool` (tool name), `direction` (only `\"output\"` is supported), " +
					"optional `fields` (`[{path, action}]` with `action` of `pass`/`redact`/`block`), " +
					"optional `when` predicates (`[{path, eq, action, scope}]`), optional `principals` " +
					"(`[{type: GROUP|ROLE, id}]`), and an optional `default_action` (`pass`/`block`). The " +
					"API validates the shape and rejects malformed rules with a 409. Omit for an empty " +
					"rule set.",
				Optional:   true,
				CustomType: jsontypes.NormalizedType{},
			},
			"version": schema.Int64Attribute{
				MarkdownDescription: "Server-managed revision counter: starts at 1 and increments on every " +
					"content change.",
				Computed: true,
			},
			"created_by": schema.StringAttribute{
				MarkdownDescription: "Subject that created the policy (empty when the platform could not " +
					"attribute it).",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"updated_by": schema.StringAttribute{
				MarkdownDescription: "Subject that last updated the policy.",
				Computed:            true,
			},
			"created_at": schema.StringAttribute{
				MarkdownDescription: "When the policy was created (RFC 3339).",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"updated_at": schema.StringAttribute{
				MarkdownDescription: "When the policy was last updated (RFC 3339).",
				Computed:            true,
			},
		},
	}
}

func (r *dlpFieldControlPolicyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// ValidateConfig rejects a `rules` value that is valid JSON but not an array —
// the API requires a JSON array of rule objects, so catch a bare object,
// string, or number at plan time. Rule contents are validated server-side.
func (r *dlpFieldControlPolicyResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg dlpFieldControlPolicyResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	validateJSONArray(cfg.Rules, path.Root("rules"), &resp.Diagnostics)
}

// validateJSONArray adds a diagnostic when v holds known JSON that is not an
// array.
func validateJSONArray(v jsontypes.Normalized, p path.Path, diags *diag.Diagnostics) {
	if v.IsNull() || v.IsUnknown() {
		return
	}
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(v.ValueString()), &arr); err != nil || arr == nil {
		detail := "the value is JSON null"
		if err != nil {
			detail = err.Error()
		}
		diags.AddAttributeError(p, "Value must be a JSON array",
			fmt.Sprintf("Expected a JSON array of rule objects (e.g. `jsonencode([{...}])`): %s.", detail))
	}
}

// Create upserts the policy after confirming no policy already exists for the
// planned MCP server (see the resource doc comment for why adoption is a
// conflict, not a merge).
func (r *dlpFieldControlPolicyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan dlpFieldControlPolicyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Pre-flight: the API's POST is an upsert keyed on (org, mcp_server_id)
	// and answers 201 for update-of-existing too, so an existing policy must
	// be detected here — the status code will not tell.
	var existing dlpFieldControlPoliciesPage
	if err := doJSON(ctx, r.client, http.MethodGet, dlpAPIPrefix+"/field-control-policies", nil, &existing); err != nil {
		addDlpFieldControlPolicyAPIError(&resp.Diagnostics, "list field control policies before create", err)
		return
	}
	mcpServerID := plan.McpServerID.ValueString()
	for i := range existing.Items {
		if existing.Items[i].McpServerID == mcpServerID {
			resp.Diagnostics.AddError(
				"Field control policy already exists for this MCP server",
				fmt.Sprintf("MCP server %q already has field control policy %q (id %s), and the platform "+
					"keeps at most one per server. Import it instead of recreating it, so its current rules "+
					"are not silently overwritten:\n\n  terraform import <address> %s",
					mcpServerID, existing.Items[i].Name, existing.Items[i].ID, existing.Items[i].ID),
			)
			return
		}
	}

	body, err := buildDlpFieldControlPolicyUpsertRequest(&plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid field control policy configuration", err.Error())
		return
	}

	var policy dlpFieldControlPolicyResponse
	if err := doJSON(ctx, r.client, http.MethodPost, dlpAPIPrefix+"/field-control-policies", body, &policy); err != nil {
		addDlpFieldControlPolicyAPIError(&resp.Diagnostics, "create the field control policy", err)
		return
	}
	if policy.ID == "" {
		resp.Diagnostics.AddError("Malformed Data Protection API response", "Create returned no policy id.")
		return
	}

	state := applyDlpFieldControlPolicyResponse(&policy, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *dlpFieldControlPolicyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state dlpFieldControlPolicyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var policy dlpFieldControlPolicyResponse
	err := doJSON(ctx, r.client, http.MethodGet,
		dlpAPIPrefix+"/field-control-policies/"+state.ID.ValueString(), nil, &policy)
	if err != nil {
		if isNotFound(err) {
			// Deleted out-of-band; drop the resource so Terraform plans a
			// recreate.
			resp.State.RemoveResource(ctx)
			return
		}
		addDlpFieldControlPolicyAPIError(&resp.Diagnostics, "read the field control policy", err)
		return
	}

	newState := applyDlpFieldControlPolicyResponse(&policy, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *dlpFieldControlPolicyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan dlpFieldControlPolicyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, err := buildDlpFieldControlPolicyUpdateRequest(&plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid field control policy configuration", err.Error())
		return
	}

	var policy dlpFieldControlPolicyResponse
	if err := doJSON(ctx, r.client, http.MethodPut,
		dlpAPIPrefix+"/field-control-policies/"+plan.ID.ValueString(), body, &policy); err != nil {
		addDlpFieldControlPolicyAPIError(&resp.Diagnostics, "update the field control policy", err)
		return
	}

	newState := applyDlpFieldControlPolicyResponse(&policy, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Delete removes the policy. A 404 means it is already gone — success for a
// destroy.
func (r *dlpFieldControlPolicyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state dlpFieldControlPolicyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := doJSON(ctx, r.client, http.MethodDelete,
		dlpAPIPrefix+"/field-control-policies/"+state.ID.ValueString(), nil, nil)
	if err != nil && !isNotFound(err) {
		addDlpFieldControlPolicyAPIError(&resp.Diagnostics, "delete the field control policy", err)
	}
}

func (r *dlpFieldControlPolicyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *dlpFieldControlPolicyResource) requireClient(diags *diag.Diagnostics) bool {
	if r.client == nil {
		diags.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return false
	}
	return true
}

// addDlpFieldControlPolicyAPIError turns a DLP admin API error into an
// actionable diagnostic. action is the failed verb phrase, e.g. "create the
// field control policy".
func addDlpFieldControlPolicyAPIError(diags *diag.Diagnostics, action string, err error) {
	apiErr, ok := asAPIError(err)
	if !ok {
		diags.AddError("Failed to "+action, err.Error())
		return
	}

	switch apiErr.status {
	case http.StatusConflict:
		// The API answers 409 for malformed rule contents (its store-level
		// shape validation), not for duplicates; the message names the exact
		// rule and field.
		diags.AddError(
			"Field control rules rejected by the Data Protection API",
			fmt.Sprintf("Failed to %s: %s\n\nEach `rules` entry must be an object with a non-empty `tool`, "+
				"`direction = \"output\"`, `fields`/`when` entries with dot-separated `path`s, and "+
				"`pass`/`redact`/`block` actions.", action, apiErr.displayBody()),
		)
	case http.StatusUnprocessableEntity:
		// The API's validation message (empty name or mcp_server_id, invalid
		// UUID, …) is the most specific thing we can show; surface it verbatim.
		diags.AddError("Field control policy rejected by the Data Protection API", apiErr.displayBody())
	case http.StatusUnauthorized, http.StatusForbidden:
		diags.AddError(
			"Permission denied by the Data Protection API",
			fmt.Sprintf("Failed to %s: the configured credential was not accepted. Confirm the "+
				"service-account credential's token carries the organization claim for the organization "+
				"that owns this policy.\n\nServer message: %s", action, apiErr.displayBody()),
		)
	default:
		diags.AddError("Failed to "+action, apiErr.Error())
	}
}

// --- request/response DTOs ---------------------------------------------------

// dlpFieldControlPolicyUpsertRequest mirrors the dlp-service
// UpsertFieldControlPolicyRequest body. enabled and rules are always sent so
// the stored row matches the plan exactly (the API would default them to
// true / [] anyway).
type dlpFieldControlPolicyUpsertRequest struct {
	McpServerID string          `json:"mcp_server_id"`
	Name        string          `json:"name"`
	Enabled     bool            `json:"enabled"`
	Rules       json.RawMessage `json:"rules"`
}

// dlpFieldControlPolicyUpdateRequest mirrors the dlp-service
// UpdateFieldControlPolicyRequest body (deny_unknown_fields: only these keys
// may appear). The API applies exactly the keys present, so Update sends
// every managed field — Terraform's plan is the full desired state, and an
// omitted rules key would leave removed rules behind (rules are sent as `[]`
// when unset).
type dlpFieldControlPolicyUpdateRequest struct {
	Name    string          `json:"name"`
	Enabled bool            `json:"enabled"`
	Rules   json.RawMessage `json:"rules"`
}

// dlpFieldControlPolicyResponse mirrors the dlp-service
// FieldControlPolicyResponse.
type dlpFieldControlPolicyResponse struct {
	ID          string          `json:"id"`
	OrgID       string          `json:"org_id"`
	McpServerID string          `json:"mcp_server_id"`
	Name        string          `json:"name"`
	Enabled     bool            `json:"enabled"`
	Version     int64           `json:"version"`
	Rules       json.RawMessage `json:"rules"`
	CreatedBy   string          `json:"created_by"`
	UpdatedBy   string          `json:"updated_by"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
}

// dlpFieldControlPoliciesPage mirrors the dlp-service
// FieldControlPoliciesResponse envelope (`{"items": [...]}`; the listing is
// not paginated).
type dlpFieldControlPoliciesPage struct {
	Items []dlpFieldControlPolicyResponse `json:"items"`
}

// dlpPlannedRules renders the planned rules for a request body: the
// configured JSON text, or `[]` when unset (never JSON null — the API's
// update handler would treat it as valid "no rules key" JSON but the upsert
// default only applies to an absent key, and the convergence contract wants
// an explicit clear).
func dlpPlannedRules(rules jsontypes.Normalized) (json.RawMessage, error) {
	v, ok := knownNormalized(rules)
	if !ok {
		return json.RawMessage("[]"), nil
	}
	if !json.Valid([]byte(v)) {
		return nil, fmt.Errorf("rules: not valid JSON")
	}
	return json.RawMessage(v), nil
}

// buildDlpFieldControlPolicyUpsertRequest converts the planned model to the
// create (upsert) body.
func buildDlpFieldControlPolicyUpsertRequest(plan *dlpFieldControlPolicyResourceModel) (*dlpFieldControlPolicyUpsertRequest, error) {
	rules, err := dlpPlannedRules(plan.Rules)
	if err != nil {
		return nil, err
	}
	return &dlpFieldControlPolicyUpsertRequest{
		McpServerID: plan.McpServerID.ValueString(),
		Name:        plan.Name.ValueString(),
		Enabled:     plan.Enabled.ValueBool(),
		Rules:       rules,
	}, nil
}

// buildDlpFieldControlPolicyUpdateRequest converts the planned model to the
// update body, carrying the full desired state (see the request type's doc
// comment for the convergence contract).
func buildDlpFieldControlPolicyUpdateRequest(plan *dlpFieldControlPolicyResourceModel) (*dlpFieldControlPolicyUpdateRequest, error) {
	rules, err := dlpPlannedRules(plan.Rules)
	if err != nil {
		return nil, err
	}
	return &dlpFieldControlPolicyUpdateRequest{
		Name:    plan.Name.ValueString(),
		Enabled: plan.Enabled.ValueBool(),
		Rules:   rules,
	}, nil
}

// applyDlpFieldControlPolicyResponse maps the server's view onto a state
// model. prior is the plan (Create/Update) or previous state (Read), used to
// settle an empty rules array back to null when the configuration said
// nothing.
func applyDlpFieldControlPolicyResponse(policy *dlpFieldControlPolicyResponse, prior *dlpFieldControlPolicyResourceModel) dlpFieldControlPolicyResourceModel {
	return dlpFieldControlPolicyResourceModel{
		ID:          types.StringValue(policy.ID),
		OrgID:       types.StringValue(policy.OrgID),
		McpServerID: types.StringValue(policy.McpServerID),
		Name:        types.StringValue(policy.Name),
		Enabled:     types.BoolValue(policy.Enabled),
		Rules:       normalizedFromRules(policy.Rules, prior.Rules),
		Version:     types.Int64Value(policy.Version),
		CreatedBy:   types.StringValue(policy.CreatedBy),
		UpdatedBy:   types.StringValue(policy.UpdatedBy),
		CreatedAt:   types.StringValue(policy.CreatedAt),
		UpdatedAt:   types.StringValue(policy.UpdatedAt),
	}
}

// normalizedFromRules maps the wire rules JSON to state, settling an empty
// array to null when the prior value was null/unknown — the server cannot
// distinguish "cleared" from "never set", and echoing `[]` where the config
// said nothing would be perpetual drift. Formatting differences against the
// config (the platform stores rules as JSONB, which normalizes key order and
// whitespace) are absorbed by jsontypes.Normalized semantic equality.
func normalizedFromRules(raw json.RawMessage, prior jsontypes.Normalized) jsontypes.Normalized {
	if len(raw) == 0 {
		raw = json.RawMessage("[]")
	}
	if prior.IsNull() || prior.IsUnknown() {
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err == nil && len(arr) == 0 {
			return jsontypes.NewNormalizedNull()
		}
	}
	return jsontypes.NewNormalizedValue(string(raw))
}
