// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// Enforcement policy enum wire values, as the dlp-service admin REST API
// serializes them (the proto str_name forms for actions and runtime stages;
// plain uppercase words for target kinds, directions, and principal types).
var (
	dlpPolicyActions = []string{
		"POLICY_ACTION_TOKENIZE",
		"POLICY_ACTION_REDACT",
		"POLICY_ACTION_PASSTHROUGH",
		"POLICY_ACTION_ALERT_ONLY",
		"POLICY_ACTION_BLOCK",
		"POLICY_ACTION_MASK",
		"POLICY_ACTION_OMIT",
	}
	dlpRuntimeStages = []string{
		"RUNTIME_STAGE_UNSPECIFIED",
		"RUNTIME_STAGE_PROMPT",
		"RUNTIME_STAGE_RESPONSE",
		"RUNTIME_STAGE_TOOL_INPUT",
		"RUNTIME_STAGE_TOOL_OUTPUT",
	}
	dlpTargetKinds    = []string{"MCP_SERVER", "MODEL_PROVIDER"}
	dlpDirections     = []string{"REQUEST", "RESPONSE", "BOTH"}
	dlpPrincipalTypes = []string{"GROUP", "ROLE"}
)

// dlpNoSurroundingWhitespace mirrors the API's trimming of identifier-ish
// values (names, scope ids): a padded value would read back trimmed and
// surface as a "provider produced an inconsistent result" error, so reject it
// (and the empty string) at plan time instead.
var dlpNoSurroundingWhitespace = stringvalidator.RegexMatches(
	regexp.MustCompile(`^\S(.*\S)?$`),
	"must not be empty or have leading/trailing whitespace",
)

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                = &dlpEnforcementPolicyResource{}
	_ resource.ResourceWithConfigure   = &dlpEnforcementPolicyResource{}
	_ resource.ResourceWithImportState = &dlpEnforcementPolicyResource{}
)

// NewDlpEnforcementPolicyResource returns a new barndoor_dlp_enforcement_policy resource.
func NewDlpEnforcementPolicyResource() resource.Resource {
	return &dlpEnforcementPolicyResource{}
}

// dlpEnforcementPolicyResource manages a Data Protection enforcement policy
// through the dlp-service tenant admin REST API
// (`/api/dlp/admin/v1/enforcement-policies`).
type dlpEnforcementPolicyResource struct {
	client *client.Client
}

// dlpEnforcementPolicyResourceModel maps the resource schema to Go types.
// McpTargets and Principals are plain slices so a null set (attribute
// omitted) round-trips as nil and an explicit empty set as a non-nil empty
// slice.
type dlpEnforcementPolicyResourceModel struct {
	ID                 types.String        `tfsdk:"id"`
	OrgID              types.String        `tfsdk:"org_id"`
	Name               types.String        `tfsdk:"name"`
	TargetKind         types.String        `tfsdk:"target_kind"`
	ProviderIDs        types.List          `tfsdk:"provider_ids"`
	ModelAlias         types.String        `tfsdk:"model_alias"`
	RuntimeStage       types.String        `tfsdk:"runtime_stage"`
	Action             types.String        `tfsdk:"action"`
	Priority           types.Int64         `tfsdk:"priority"`
	DryRun             types.Bool          `tfsdk:"dry_run"`
	McpTargets         []dlpMcpTargetModel `tfsdk:"mcp_targets"`
	Principals         []dlpPrincipalModel `tfsdk:"principals"`
	DetectionEngineIDs types.List          `tfsdk:"detection_engine_ids"`
	CreatedBy          types.String        `tfsdk:"created_by"`
	UpdatedBy          types.String        `tfsdk:"updated_by"`
	CreatedAt          types.String        `tfsdk:"created_at"`
	UpdatedAt          types.String        `tfsdk:"updated_at"`
}

// dlpMcpTargetModel maps one entry of the mcp_targets set.
type dlpMcpTargetModel struct {
	McpServerID types.String `tfsdk:"mcp_server_id"`
	Direction   types.String `tfsdk:"direction"`
}

// dlpPrincipalModel maps one entry of the principals set.
type dlpPrincipalModel struct {
	PrincipalType types.String `tfsdk:"principal_type"`
	PrincipalID   types.String `tfsdk:"principal_id"`
}

func (r *dlpEnforcementPolicyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dlp_enforcement_policy"
}

func (r *dlpEnforcementPolicyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Data Protection (DLP) enforcement policy: what traffic it targets " +
			"(MCP servers or model providers), which detection engines evaluate it, the action taken on a " +
			"finding, and which principals it applies to.\n\n" +
			"A policy targets exactly one lane, and the API rejects mixed shapes: `MCP_SERVER` policies use " +
			"`mcp_targets` (with `RUNTIME_STAGE_TOOL_INPUT`/`RUNTIME_STAGE_TOOL_OUTPUT` stages) and may not " +
			"set `provider_ids`/`model_alias`; `MODEL_PROVIDER` policies are the reverse (with " +
			"`RUNTIME_STAGE_PROMPT`/`RUNTIME_STAGE_RESPONSE` stages). When `target_kind` is unset the API " +
			"infers it from the scope attributes.",
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
			"name": schema.StringAttribute{
				MarkdownDescription: "Policy name. Must be unique among the organization's enforcement " +
					"policies.",
				Required: true,
				Validators: []validator.String{
					dlpNoSurroundingWhitespace,
				},
			},
			"target_kind": schema.StringAttribute{
				MarkdownDescription: "Which lane the policy targets: `MCP_SERVER` or `MODEL_PROVIDER`. " +
					"Inferred by the API when unset (`MODEL_PROVIDER` when `provider_ids`, `model_alias`, " +
					"or a prompt/response `runtime_stage` is present; `MCP_SERVER` otherwise).",
				Optional: true,
				Computed: true,
				Validators: []validator.String{
					stringvalidator.OneOf(dlpTargetKinds...),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"provider_ids": schema.ListAttribute{
				MarkdownDescription: "Model provider IDs the policy is scoped to (`MODEL_PROVIDER` policies " +
					"only). Omit to match every provider.",
				ElementType: types.StringType,
				Optional:    true,
				Validators: []validator.List{
					listvalidator.UniqueValues(),
					listvalidator.ValueStringsAre(dlpNoSurroundingWhitespace),
				},
			},
			"model_alias": schema.StringAttribute{
				MarkdownDescription: "Model alias the policy is scoped to (`MODEL_PROVIDER` policies only). " +
					"Omit to match every model.",
				Optional: true,
				Validators: []validator.String{
					dlpNoSurroundingWhitespace,
				},
			},
			"runtime_stage": schema.StringAttribute{
				MarkdownDescription: "Runtime stage the policy applies to: `RUNTIME_STAGE_PROMPT` / " +
					"`RUNTIME_STAGE_RESPONSE` (`MODEL_PROVIDER`), `RUNTIME_STAGE_TOOL_INPUT` / " +
					"`RUNTIME_STAGE_TOOL_OUTPUT` (`MCP_SERVER`), or `RUNTIME_STAGE_UNSPECIFIED` (every " +
					"stage of the target lane — the server default).",
				Optional: true,
				Computed: true,
				Validators: []validator.String{
					stringvalidator.OneOf(dlpRuntimeStages...),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"action": schema.StringAttribute{
				MarkdownDescription: "Action taken when a detection engine reports a finding: " +
					"`POLICY_ACTION_TOKENIZE`, `POLICY_ACTION_REDACT`, `POLICY_ACTION_PASSTHROUGH`, " +
					"`POLICY_ACTION_ALERT_ONLY`, `POLICY_ACTION_BLOCK`, `POLICY_ACTION_MASK`, or " +
					"`POLICY_ACTION_OMIT`. Every referenced detection engine must support the action " +
					"(the API rejects unsupported combinations with a 422).",
				Required: true,
				Validators: []validator.String{
					stringvalidator.OneOf(dlpPolicyActions...),
				},
			},
			"priority": schema.Int64Attribute{
				MarkdownDescription: "Evaluation priority — lower wins, and the value is unique per " +
					"organization and target kind. Assigned by the API (next free value in the target " +
					"kind's lane) when unset.",
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"dry_run": schema.BoolAttribute{
				MarkdownDescription: "Whether the policy only observes (records findings without acting). " +
					"Defaults to `false`.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
			},
			"mcp_targets": schema.SetNestedAttribute{
				MarkdownDescription: "MCP servers the policy targets (`MCP_SERVER` policies only). Use " +
					"`mcp_server_id = \"*\"` to target every server — the wildcard may not be mixed with " +
					"specific server IDs. Omit to match every server as well.",
				Optional:     true,
				NestedObject: dlpMcpTargetNestedObject(),
			},
			"principals": schema.SetNestedAttribute{
				MarkdownDescription: "Principals (groups or roles) the policy is limited to. Omit to apply " +
					"to everyone.",
				Optional:     true,
				NestedObject: dlpPrincipalNestedObject(),
			},
			"detection_engine_ids": schema.ListAttribute{
				MarkdownDescription: "IDs (UUIDs) of the detection engines that evaluate traffic for this " +
					"policy, in evaluation order. At least one is required, and each engine must belong to " +
					"the organization and support the policy's `action`.",
				ElementType: types.StringType,
				Required:    true,
				Validators: []validator.List{
					listvalidator.SizeAtLeast(1),
					listvalidator.UniqueValues(),
					listvalidator.ValueStringsAre(dlpNoSurroundingWhitespace),
				},
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

// dlpMcpTargetNestedObject defines the schema for one mcp_targets entry.
func dlpMcpTargetNestedObject() schema.NestedAttributeObject {
	return schema.NestedAttributeObject{
		Attributes: map[string]schema.Attribute{
			"mcp_server_id": schema.StringAttribute{
				MarkdownDescription: "MCP server the target names, or `*` for every server.",
				Required:            true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"direction": schema.StringAttribute{
				MarkdownDescription: "Traffic direction the target covers: `REQUEST`, `RESPONSE`, or " +
					"`BOTH`. Must be compatible with the policy's `runtime_stage` (`REQUEST` for tool " +
					"input, `RESPONSE` for tool output).",
				Required: true,
				Validators: []validator.String{
					stringvalidator.OneOf(dlpDirections...),
				},
			},
		},
	}
}

// dlpPrincipalNestedObject defines the schema for one principals entry.
func dlpPrincipalNestedObject() schema.NestedAttributeObject {
	return schema.NestedAttributeObject{
		Attributes: map[string]schema.Attribute{
			"principal_type": schema.StringAttribute{
				MarkdownDescription: "Kind of principal: `GROUP` or `ROLE`.",
				Required:            true,
				Validators: []validator.String{
					stringvalidator.OneOf(dlpPrincipalTypes...),
				},
			},
			"principal_id": schema.StringAttribute{
				MarkdownDescription: "Group or role the policy is limited to.",
				Required:            true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
		},
	}
}

func (r *dlpEnforcementPolicyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *dlpEnforcementPolicyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan dlpEnforcementPolicyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, err := buildDlpEnforcementPolicyCreateRequest(ctx, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid enforcement policy configuration", err.Error())
		return
	}

	var policy dlpEnforcementPolicyResponse
	if err := doJSON(ctx, r.client, http.MethodPost, dlpAPIPrefix+"/enforcement-policies", body, &policy); err != nil {
		addDlpEnforcementPolicyAPIError(&resp.Diagnostics, "create the enforcement policy", err)
		return
	}
	if policy.ID == "" {
		resp.Diagnostics.AddError("Malformed Data Protection API response", "Create returned no policy id.")
		return
	}

	state, err := applyDlpEnforcementPolicyResponse(ctx, &policy, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to map the Data Protection API response", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *dlpEnforcementPolicyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state dlpEnforcementPolicyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var policy dlpEnforcementPolicyResponse
	err := doJSON(ctx, r.client, http.MethodGet,
		dlpAPIPrefix+"/enforcement-policies/"+state.ID.ValueString(), nil, &policy)
	if err != nil {
		if isNotFound(err) {
			// Deleted out-of-band; drop the resource so Terraform plans a
			// recreate.
			resp.State.RemoveResource(ctx)
			return
		}
		addDlpEnforcementPolicyAPIError(&resp.Diagnostics, "read the enforcement policy", err)
		return
	}

	newState, err := applyDlpEnforcementPolicyResponse(ctx, &policy, &state)
	if err != nil {
		resp.Diagnostics.AddError("Failed to map the Data Protection API response", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *dlpEnforcementPolicyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan dlpEnforcementPolicyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, err := buildDlpEnforcementPolicyUpdateRequest(ctx, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid enforcement policy configuration", err.Error())
		return
	}

	var policy dlpEnforcementPolicyResponse
	if err := doJSON(ctx, r.client, http.MethodPut,
		dlpAPIPrefix+"/enforcement-policies/"+plan.ID.ValueString(), body, &policy); err != nil {
		addDlpEnforcementPolicyAPIError(&resp.Diagnostics, "update the enforcement policy", err)
		return
	}

	newState, err := applyDlpEnforcementPolicyResponse(ctx, &policy, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to map the Data Protection API response", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Delete removes the policy. A 404 means it is already gone — success for a
// destroy.
func (r *dlpEnforcementPolicyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state dlpEnforcementPolicyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := doJSON(ctx, r.client, http.MethodDelete,
		dlpAPIPrefix+"/enforcement-policies/"+state.ID.ValueString(), nil, nil)
	if err != nil && !isNotFound(err) {
		addDlpEnforcementPolicyAPIError(&resp.Diagnostics, "delete the enforcement policy", err)
	}
}

func (r *dlpEnforcementPolicyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *dlpEnforcementPolicyResource) requireClient(diags *diag.Diagnostics) bool {
	if r.client == nil {
		diags.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return false
	}
	return true
}

// addDlpEnforcementPolicyAPIError turns a DLP admin API error into an
// actionable diagnostic. action is the failed verb phrase, e.g. "create the
// enforcement policy".
func addDlpEnforcementPolicyAPIError(diags *diag.Diagnostics, action string, err error) {
	apiErr, ok := asAPIError(err)
	if !ok {
		diags.AddError("Failed to "+action, err.Error())
		return
	}

	switch apiErr.status {
	case http.StatusNotFound:
		// Either the policy itself or a referenced detection engine; the API's
		// message names which.
		diags.AddError(
			"Object not found by the Data Protection API",
			fmt.Sprintf("Failed to %s: %s\n\nConfirm every `detection_engine_ids` entry names a detection "+
				"engine that exists in the organization.", action, apiErr.displayBody()),
		)
	case http.StatusConflict:
		diags.AddError(
			"Enforcement policy conflicts with an existing one",
			fmt.Sprintf("Failed to %s: %s\n\nPolicy names are unique per organization and `priority` is "+
				"unique per organization and target kind; choose a different `name`, pick a free "+
				"`priority`, or omit `priority` to let the API assign the next free value.",
				action, apiErr.displayBody()),
		)
	case http.StatusUnprocessableEntity:
		// The API's validation message (mixed target shape, unsupported
		// engine/action combination, wildcard mixing, direction/stage
		// mismatch, …) is the most specific thing we can show; surface it
		// verbatim.
		diags.AddError("Enforcement policy rejected by the Data Protection API", apiErr.displayBody())
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

// dlpMcpTargetPayload / dlpPrincipalPayload mirror the request shapes of one
// mcp_targets / principals entry (the response rows additionally carry a
// server-internal row `id`, which this resource does not track — the rows are
// replaced wholesale on update, so their ids are not stable identity).
type dlpMcpTargetPayload struct {
	McpServerID string `json:"mcp_server_id"`
	Direction   string `json:"direction"`
}

type dlpPrincipalPayload struct {
	PrincipalType string `json:"principal_type"`
	PrincipalID   string `json:"principal_id"`
}

// dlpEnforcementPolicyCreateRequest mirrors the dlp-service
// CreateEnforcementPolicyRequest body. The collections are always non-nil:
// the API tolerates absent collections but rejects JSON null for them.
type dlpEnforcementPolicyCreateRequest struct {
	Name               string                `json:"name"`
	TargetKind         *string               `json:"target_kind,omitempty"`
	ProviderIDs        []string              `json:"provider_ids"`
	ModelAlias         *string               `json:"model_alias,omitempty"`
	RuntimeStage       *string               `json:"runtime_stage,omitempty"`
	Action             string                `json:"action"`
	Priority           *int64                `json:"priority,omitempty"`
	DryRun             bool                  `json:"dry_run"`
	McpTargets         []dlpMcpTargetPayload `json:"mcp_targets"`
	Principals         []dlpPrincipalPayload `json:"principals"`
	DetectionEngineIDs []string              `json:"detection_engine_ids"`
}

// dlpEnforcementPolicyUpdateRequest mirrors the dlp-service
// UpdateEnforcementPolicyRequest body (deny_unknown_fields: only these keys
// may appear). The API applies exactly the keys present, so Update sends
// every managed field — Terraform's plan is the full desired state, and an
// omitted key would leave a removed value behind. Collections are
// pointer-to-slice so an emptied collection serializes as `[]` (clear), never
// as an absent key (keep). model_alias is special: the API treats JSON null
// as "no change" and an empty string as "clear", so it is always sent as a
// string, "" when unset.
type dlpEnforcementPolicyUpdateRequest struct {
	Name               *string                `json:"name,omitempty"`
	TargetKind         *string                `json:"target_kind,omitempty"`
	ProviderIDs        *[]string              `json:"provider_ids,omitempty"`
	ModelAlias         *string                `json:"model_alias,omitempty"`
	RuntimeStage       *string                `json:"runtime_stage,omitempty"`
	Action             *string                `json:"action,omitempty"`
	Priority           *int64                 `json:"priority,omitempty"`
	DryRun             *bool                  `json:"dry_run,omitempty"`
	McpTargets         *[]dlpMcpTargetPayload `json:"mcp_targets,omitempty"`
	Principals         *[]dlpPrincipalPayload `json:"principals,omitempty"`
	DetectionEngineIDs *[]string              `json:"detection_engine_ids,omitempty"`
}

// dlpEnforcementPolicyResponse mirrors the dlp-service
// EnforcementPolicyResponse.
type dlpEnforcementPolicyResponse struct {
	ID                 string                `json:"id"`
	OrgID              string                `json:"org_id"`
	Name               string                `json:"name"`
	TargetKind         string                `json:"target_kind"`
	ProviderIDs        []string              `json:"provider_ids"`
	ModelAlias         *string               `json:"model_alias"`
	RuntimeStage       string                `json:"runtime_stage"`
	Action             string                `json:"action"`
	Priority           int64                 `json:"priority"`
	DryRun             bool                  `json:"dry_run"`
	McpTargets         []dlpMcpTargetPayload `json:"mcp_targets"`
	Principals         []dlpPrincipalPayload `json:"principals"`
	DetectionEngineIDs []string              `json:"detection_engine_ids"`
	CreatedBy          string                `json:"created_by"`
	UpdatedBy          string                `json:"updated_by"`
	CreatedAt          string                `json:"created_at"`
	UpdatedAt          string                `json:"updated_at"`
}

// dlpMcpTargetPayloads converts the planned mcp_targets set to the API shape
// (non-nil, so it serializes as `[]` when empty).
func dlpMcpTargetPayloads(targets []dlpMcpTargetModel) []dlpMcpTargetPayload {
	out := make([]dlpMcpTargetPayload, 0, len(targets))
	for _, t := range targets {
		out = append(out, dlpMcpTargetPayload{
			McpServerID: t.McpServerID.ValueString(),
			Direction:   t.Direction.ValueString(),
		})
	}
	return out
}

// dlpPrincipalPayloads converts the planned principals set to the API shape
// (non-nil, so it serializes as `[]` when empty).
func dlpPrincipalPayloads(principals []dlpPrincipalModel) []dlpPrincipalPayload {
	out := make([]dlpPrincipalPayload, 0, len(principals))
	for _, p := range principals {
		out = append(out, dlpPrincipalPayload{
			PrincipalType: p.PrincipalType.ValueString(),
			PrincipalID:   p.PrincipalID.ValueString(),
		})
	}
	return out
}

// nonNilStrings normalizes a nil string slice to an empty one so it
// serializes as `[]`, never JSON null (which the API rejects).
func nonNilStrings(vals []string) []string {
	if vals == nil {
		return []string{}
	}
	return vals
}

// buildDlpEnforcementPolicyCreateRequest converts the planned model to the
// create body. target_kind, runtime_stage, and priority are sent only when
// configured so the API's inference/defaulting applies otherwise.
func buildDlpEnforcementPolicyCreateRequest(ctx context.Context, plan *dlpEnforcementPolicyResourceModel) (*dlpEnforcementPolicyCreateRequest, error) {
	providerIDs, err := stringsFromList(ctx, plan.ProviderIDs)
	if err != nil {
		return nil, fmt.Errorf("provider_ids: %w", err)
	}
	engineIDs, err := stringsFromList(ctx, plan.DetectionEngineIDs)
	if err != nil {
		return nil, fmt.Errorf("detection_engine_ids: %w", err)
	}

	body := &dlpEnforcementPolicyCreateRequest{
		Name:               plan.Name.ValueString(),
		ProviderIDs:        nonNilStrings(providerIDs),
		Action:             plan.Action.ValueString(),
		DryRun:             plan.DryRun.ValueBool(),
		McpTargets:         dlpMcpTargetPayloads(plan.McpTargets),
		Principals:         dlpPrincipalPayloads(plan.Principals),
		DetectionEngineIDs: nonNilStrings(engineIDs),
	}
	if v, ok := knownString(plan.TargetKind); ok {
		body.TargetKind = &v
	}
	if v, ok := knownString(plan.ModelAlias); ok {
		body.ModelAlias = &v
	}
	if v, ok := knownString(plan.RuntimeStage); ok {
		body.RuntimeStage = &v
	}
	if v, ok := knownInt64(plan.Priority); ok {
		body.Priority = &v
	}
	return body, nil
}

// buildDlpEnforcementPolicyUpdateRequest converts the planned model to the
// update body, carrying the full desired state (see the request type's doc
// comment for the convergence contract).
func buildDlpEnforcementPolicyUpdateRequest(ctx context.Context, plan *dlpEnforcementPolicyResourceModel) (*dlpEnforcementPolicyUpdateRequest, error) {
	providerIDs, err := stringsFromList(ctx, plan.ProviderIDs)
	if err != nil {
		return nil, fmt.Errorf("provider_ids: %w", err)
	}
	engineIDs, err := stringsFromList(ctx, plan.DetectionEngineIDs)
	if err != nil {
		return nil, fmt.Errorf("detection_engine_ids: %w", err)
	}

	name := plan.Name.ValueString()
	action := plan.Action.ValueString()
	dryRun := plan.DryRun.ValueBool()
	// "" clears the alias server-side (JSON null would mean "no change").
	modelAlias := plan.ModelAlias.ValueString()
	nonNilProviderIDs := nonNilStrings(providerIDs)
	mcpTargets := dlpMcpTargetPayloads(plan.McpTargets)
	principals := dlpPrincipalPayloads(plan.Principals)
	nonNilEngineIDs := nonNilStrings(engineIDs)

	body := &dlpEnforcementPolicyUpdateRequest{
		Name:               &name,
		ProviderIDs:        &nonNilProviderIDs,
		ModelAlias:         &modelAlias,
		Action:             &action,
		DryRun:             &dryRun,
		McpTargets:         &mcpTargets,
		Principals:         &principals,
		DetectionEngineIDs: &nonNilEngineIDs,
	}
	// Computed-when-unset attributes carry their last-known values through the
	// plan (UseStateForUnknown), so these are known on every ordinary update;
	// they are only omitted when a value is still unresolved mid-plan.
	if v, ok := knownString(plan.TargetKind); ok {
		body.TargetKind = &v
	}
	if v, ok := knownString(plan.RuntimeStage); ok {
		body.RuntimeStage = &v
	}
	if v, ok := knownInt64(plan.Priority); ok {
		body.Priority = &v
	}
	return body, nil
}

// applyDlpEnforcementPolicyResponse maps the server's view onto a state
// model. prior is the plan (Create/Update) or previous state (Read), used to
// settle empty collections and the cleared model_alias back to null when the
// configuration said nothing.
func applyDlpEnforcementPolicyResponse(ctx context.Context, policy *dlpEnforcementPolicyResponse, prior *dlpEnforcementPolicyResourceModel) (dlpEnforcementPolicyResourceModel, error) {
	m := dlpEnforcementPolicyResourceModel{
		ID:           types.StringValue(policy.ID),
		OrgID:        types.StringValue(policy.OrgID),
		Name:         types.StringValue(policy.Name),
		TargetKind:   types.StringValue(policy.TargetKind),
		ModelAlias:   optionalStringFromPtr(policy.ModelAlias, prior.ModelAlias),
		RuntimeStage: types.StringValue(policy.RuntimeStage),
		Action:       types.StringValue(policy.Action),
		Priority:     types.Int64Value(policy.Priority),
		DryRun:       types.BoolValue(policy.DryRun),
		McpTargets:   applyDlpMcpTargets(policy.McpTargets, prior.McpTargets),
		Principals:   applyDlpPrincipals(policy.Principals, prior.Principals),
		CreatedBy:    types.StringValue(policy.CreatedBy),
		UpdatedBy:    types.StringValue(policy.UpdatedBy),
		CreatedAt:    types.StringValue(policy.CreatedAt),
		UpdatedAt:    types.StringValue(policy.UpdatedAt),
	}

	var err error
	if m.ProviderIDs, err = listFromStrings(ctx, policy.ProviderIDs, prior.ProviderIDs); err != nil {
		return dlpEnforcementPolicyResourceModel{}, fmt.Errorf("provider_ids: %w", err)
	}
	if m.DetectionEngineIDs, err = listFromStrings(ctx, policy.DetectionEngineIDs, prior.DetectionEngineIDs); err != nil {
		return dlpEnforcementPolicyResourceModel{}, fmt.Errorf("detection_engine_ids: %w", err)
	}
	return m, nil
}

// applyDlpMcpTargets maps the wire mcp_targets to state, settling an empty
// collection to null (nil) when the prior value was null — the server cannot
// distinguish "cleared" from "never set", and echoing a set where the config
// said nothing would be perpetual drift.
func applyDlpMcpTargets(targets []dlpMcpTargetPayload, prior []dlpMcpTargetModel) []dlpMcpTargetModel {
	if len(targets) == 0 {
		if prior == nil {
			return nil
		}
		return []dlpMcpTargetModel{}
	}
	out := make([]dlpMcpTargetModel, 0, len(targets))
	for _, t := range targets {
		out = append(out, dlpMcpTargetModel{
			McpServerID: types.StringValue(t.McpServerID),
			Direction:   types.StringValue(t.Direction),
		})
	}
	return out
}

// applyDlpPrincipals is applyDlpMcpTargets for the principals set.
func applyDlpPrincipals(principals []dlpPrincipalPayload, prior []dlpPrincipalModel) []dlpPrincipalModel {
	if len(principals) == 0 {
		if prior == nil {
			return nil
		}
		return []dlpPrincipalModel{}
	}
	out := make([]dlpPrincipalModel, 0, len(principals))
	for _, p := range principals {
		out = append(out, dlpPrincipalModel{
			PrincipalType: types.StringValue(p.PrincipalType),
			PrincipalID:   types.StringValue(p.PrincipalID),
		})
	}
	return out
}
