// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
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

// Detection severity/confidence enum wire values (the proto str_name forms).
// The API defaults both to MEDIUM when unset. Note the API validates
// default_confidence but stores default_severity verbatim without validation —
// the OneOf validators below are the only guard against a typo'd severity, so
// keep them aligned with the platform enum.
var (
	dlpDetectionSeverities = []string{
		"DETECTION_SEVERITY_LOW",
		"DETECTION_SEVERITY_MEDIUM",
		"DETECTION_SEVERITY_HIGH",
	}
	dlpDetectionConfidences = []string{
		"DETECTION_CONFIDENCE_LOW",
		"DETECTION_CONFIDENCE_MEDIUM",
		"DETECTION_CONFIDENCE_HIGH",
	}
)

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                = &dlpCustomDetectionTypeResource{}
	_ resource.ResourceWithConfigure   = &dlpCustomDetectionTypeResource{}
	_ resource.ResourceWithImportState = &dlpCustomDetectionTypeResource{}
)

// NewDlpCustomDetectionTypeResource returns a new barndoor_dlp_custom_detection_type resource.
func NewDlpCustomDetectionTypeResource() resource.Resource {
	return &dlpCustomDetectionTypeResource{}
}

// dlpCustomDetectionTypeResource manages an organization-defined Data
// Protection detection type through the dlp-service tenant admin REST API
// (`/api/dlp/admin/v1/custom-detection-types`).
type dlpCustomDetectionTypeResource struct {
	client *client.Client
}

// dlpCustomDetectionTypeResourceModel maps the resource schema to Go types.
// Patterns is a plain slice: the attribute is a list (the API persists and
// returns patterns in configuration order) and requires at least one entry,
// so no null-vs-empty settling is needed.
type dlpCustomDetectionTypeResourceModel struct {
	ID                types.String                     `tfsdk:"id"`
	OrgID             types.String                     `tfsdk:"org_id"`
	DetectionType     types.String                     `tfsdk:"detection_type"`
	Name              types.String                     `tfsdk:"name"`
	Description       types.String                     `tfsdk:"description"`
	Patterns          []dlpCustomDetectionPatternModel `tfsdk:"patterns"`
	DetectionGroup    types.String                     `tfsdk:"detection_group"`
	DefaultSeverity   types.String                     `tfsdk:"default_severity"`
	DefaultConfidence types.String                     `tfsdk:"default_confidence"`
	CreatedAt         types.String                     `tfsdk:"created_at"`
	UpdatedAt         types.String                     `tfsdk:"updated_at"`
}

// dlpCustomDetectionPatternModel maps one entry of the patterns list.
type dlpCustomDetectionPatternModel struct {
	Pattern     types.String `tfsdk:"pattern"`
	PatternType types.String `tfsdk:"pattern_type"`
}

func (r *dlpCustomDetectionTypeResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dlp_custom_detection_type"
}

func (r *dlpCustomDetectionTypeResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an organization-defined Data Protection (DLP) custom detection type: a " +
			"named set of literal or regular-expression patterns that detection engines match alongside the " +
			"built-in detection types.\n\n" +
			"The platform assigns the type's wire name (`detection_type`, of the form " +
			"`DETECTION_TYPE_CUSTOM_…`) and places it in the `DETECTION_GROUP_CUSTOM` group; reference the " +
			"generated `detection_type` from `barndoor_dlp_allow_list_entry.detection_types` or activity " +
			"filters.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Detection type UUID assigned by the API; also the `terraform import` key.",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"org_id": schema.StringAttribute{
				MarkdownDescription: "Organization the detection type belongs to, resolved from the provider " +
					"credential's token claims.",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"detection_type": schema.StringAttribute{
				MarkdownDescription: "Generated wire name of the detection type " +
					"(`DETECTION_TYPE_CUSTOM_<hex>`); this is the name findings carry and other resources " +
					"(e.g. allow-list `detection_types`) reference.",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Human-readable display name of the detection type.",
				Required:            true,
				Validators: []validator.String{
					dlpNoSurroundingWhitespace,
				},
			},
			"description": schema.StringAttribute{
				MarkdownDescription: "What the detection type matches (free-form).",
				Optional:            true,
				Validators: []validator.String{
					dlpNoSurroundingWhitespace,
				},
			},
			"patterns": schema.ListNestedAttribute{
				MarkdownDescription: "Patterns that constitute the detection type, evaluated in order. At " +
					"least one is required. Regular expressions use Rust `regex` syntax and are validated " +
					"by the API (a non-compiling pattern is rejected with a 422).",
				Required:     true,
				NestedObject: dlpCustomDetectionPatternNestedObject(),
				Validators: []validator.List{
					listvalidator.SizeAtLeast(1),
				},
			},
			"detection_group": schema.StringAttribute{
				MarkdownDescription: "Detection group the type belongs to — always `DETECTION_GROUP_CUSTOM` " +
					"for organization-defined types.",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"default_severity": schema.StringAttribute{
				MarkdownDescription: "Severity findings of this type carry: `DETECTION_SEVERITY_LOW`, " +
					"`DETECTION_SEVERITY_MEDIUM`, or `DETECTION_SEVERITY_HIGH`. Defaults to " +
					"`DETECTION_SEVERITY_MEDIUM` when unset.",
				Optional: true,
				Computed: true,
				Validators: []validator.String{
					stringvalidator.OneOf(dlpDetectionSeverities...),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"default_confidence": schema.StringAttribute{
				MarkdownDescription: "Confidence findings of this type carry: `DETECTION_CONFIDENCE_LOW`, " +
					"`DETECTION_CONFIDENCE_MEDIUM`, or `DETECTION_CONFIDENCE_HIGH`. Defaults to " +
					"`DETECTION_CONFIDENCE_MEDIUM` when unset.",
				Optional: true,
				Computed: true,
				Validators: []validator.String{
					stringvalidator.OneOf(dlpDetectionConfidences...),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"created_at": schema.StringAttribute{
				MarkdownDescription: "When the detection type was created (RFC 3339).",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"updated_at": schema.StringAttribute{
				MarkdownDescription: "When the detection type was last updated (RFC 3339).",
				Computed:            true,
			},
		},
	}
}

// dlpCustomDetectionPatternNestedObject defines the schema for one patterns entry.
func dlpCustomDetectionPatternNestedObject() schema.NestedAttributeObject {
	return schema.NestedAttributeObject{
		Attributes: map[string]schema.Attribute{
			"pattern": schema.StringAttribute{
				MarkdownDescription: "The literal value or regular expression to match, per `pattern_type`.",
				Required:            true,
				Validators: []validator.String{
					dlpNoSurroundingWhitespace,
				},
			},
			"pattern_type": schema.StringAttribute{
				MarkdownDescription: "How `pattern` matches: `PATTERN_TYPE_LITERAL` or `PATTERN_TYPE_REGEX`.",
				Required:            true,
				Validators: []validator.String{
					stringvalidator.OneOf(dlpPatternTypes...),
				},
			},
		},
	}
}

func (r *dlpCustomDetectionTypeResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *dlpCustomDetectionTypeResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan dlpCustomDetectionTypeResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := buildDlpCustomDetectionTypeCreateRequest(&plan)

	var detectionType dlpCustomDetectionTypeResponse
	if err := doJSON(ctx, r.client, http.MethodPost, dlpAPIPrefix+"/custom-detection-types", body, &detectionType); err != nil {
		addDlpCustomDetectionTypeAPIError(&resp.Diagnostics, "create the custom detection type", err)
		return
	}
	if detectionType.ID == "" {
		resp.Diagnostics.AddError("Malformed Data Protection API response", "Create returned no detection type id.")
		return
	}

	state := applyDlpCustomDetectionTypeResponse(&detectionType, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *dlpCustomDetectionTypeResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state dlpCustomDetectionTypeResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var detectionType dlpCustomDetectionTypeResponse
	err := doJSON(ctx, r.client, http.MethodGet,
		dlpAPIPrefix+"/custom-detection-types/"+state.ID.ValueString(), nil, &detectionType)
	if err != nil {
		if isNotFound(err) {
			// Deleted out-of-band; drop the resource so Terraform plans a
			// recreate.
			resp.State.RemoveResource(ctx)
			return
		}
		addDlpCustomDetectionTypeAPIError(&resp.Diagnostics, "read the custom detection type", err)
		return
	}

	newState := applyDlpCustomDetectionTypeResponse(&detectionType, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *dlpCustomDetectionTypeResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan dlpCustomDetectionTypeResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := buildDlpCustomDetectionTypeUpdateRequest(&plan)

	var detectionType dlpCustomDetectionTypeResponse
	if err := doJSON(ctx, r.client, http.MethodPut,
		dlpAPIPrefix+"/custom-detection-types/"+plan.ID.ValueString(), body, &detectionType); err != nil {
		addDlpCustomDetectionTypeAPIError(&resp.Diagnostics, "update the custom detection type", err)
		return
	}

	newState := applyDlpCustomDetectionTypeResponse(&detectionType, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Delete removes the detection type. A 404 means it is already gone — success
// for a destroy.
func (r *dlpCustomDetectionTypeResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state dlpCustomDetectionTypeResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := doJSON(ctx, r.client, http.MethodDelete,
		dlpAPIPrefix+"/custom-detection-types/"+state.ID.ValueString(), nil, nil)
	if err != nil && !isNotFound(err) {
		addDlpCustomDetectionTypeAPIError(&resp.Diagnostics, "delete the custom detection type", err)
	}
}

func (r *dlpCustomDetectionTypeResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *dlpCustomDetectionTypeResource) requireClient(diags *diag.Diagnostics) bool {
	if r.client == nil {
		diags.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return false
	}
	return true
}

// addDlpCustomDetectionTypeAPIError turns a DLP admin API error into an
// actionable diagnostic. action is the failed verb phrase, e.g. "create the
// custom detection type".
func addDlpCustomDetectionTypeAPIError(diags *diag.Diagnostics, action string, err error) {
	apiErr, ok := asAPIError(err)
	if !ok {
		diags.AddError("Failed to "+action, err.Error())
		return
	}

	switch apiErr.status {
	case http.StatusUnprocessableEntity:
		// The API's validation message (non-compiling regex, unknown
		// pattern_type or confidence, …) is the most specific thing we can
		// show; surface it verbatim.
		diags.AddError("Custom detection type rejected by the Data Protection API", apiErr.displayBody())
	case http.StatusUnauthorized, http.StatusForbidden:
		diags.AddError(
			"Permission denied by the Data Protection API",
			fmt.Sprintf("Failed to %s: the configured credential was not accepted. Confirm the "+
				"service-account credential's token carries the organization claim for the organization "+
				"that owns this detection type.\n\nServer message: %s", action, apiErr.displayBody()),
		)
	default:
		diags.AddError("Failed to "+action, apiErr.Error())
	}
}

// --- request/response DTOs ---------------------------------------------------

// dlpCustomDetectionPatternPayload mirrors the request and response shape of
// one patterns entry.
type dlpCustomDetectionPatternPayload struct {
	Pattern     string `json:"pattern"`
	PatternType string `json:"pattern_type"`
}

// dlpCustomDetectionTypeCreateRequest mirrors the dlp-service
// CreateCustomDetectionTypeRequest body. description, default_severity, and
// default_confidence are sent only when configured so the API's defaulting
// applies otherwise.
type dlpCustomDetectionTypeCreateRequest struct {
	Name              string                             `json:"name"`
	Description       *string                            `json:"description,omitempty"`
	Patterns          []dlpCustomDetectionPatternPayload `json:"patterns"`
	DefaultSeverity   *string                            `json:"default_severity,omitempty"`
	DefaultConfidence *string                            `json:"default_confidence,omitempty"`
}

// dlpCustomDetectionTypeUpdateRequest mirrors the dlp-service
// UpdateCustomDetectionTypeRequest body. The API applies exactly the keys
// present, so Update sends every managed field — Terraform's plan is the full
// desired state, and an omitted key would leave a removed value behind.
// description is special: the API treats an absent key as "no change" and an
// empty string as "clear", so it is always sent as a string, "" when unset.
type dlpCustomDetectionTypeUpdateRequest struct {
	Name              *string                             `json:"name,omitempty"`
	Description       *string                             `json:"description,omitempty"`
	Patterns          *[]dlpCustomDetectionPatternPayload `json:"patterns,omitempty"`
	DefaultSeverity   *string                             `json:"default_severity,omitempty"`
	DefaultConfidence *string                             `json:"default_confidence,omitempty"`
}

// dlpCustomDetectionTypeResponse mirrors the dlp-service
// CustomDetectionTypeResponse. Note the group field is serialized as `group`,
// not `detection_group`; description comes back as "" when unset.
type dlpCustomDetectionTypeResponse struct {
	ID                string                             `json:"id"`
	OrgID             string                             `json:"org_id"`
	DetectionType     string                             `json:"detection_type"`
	Name              string                             `json:"name"`
	Description       string                             `json:"description"`
	Patterns          []dlpCustomDetectionPatternPayload `json:"patterns"`
	Group             string                             `json:"group"`
	DefaultSeverity   string                             `json:"default_severity"`
	DefaultConfidence string                             `json:"default_confidence"`
	CreatedAt         string                             `json:"created_at"`
	UpdatedAt         string                             `json:"updated_at"`
}

// dlpCustomDetectionPatternPayloads converts the planned patterns list to the
// API shape (non-nil, so it serializes as `[]` rather than JSON null).
func dlpCustomDetectionPatternPayloads(patterns []dlpCustomDetectionPatternModel) []dlpCustomDetectionPatternPayload {
	out := make([]dlpCustomDetectionPatternPayload, 0, len(patterns))
	for _, p := range patterns {
		out = append(out, dlpCustomDetectionPatternPayload{
			Pattern:     p.Pattern.ValueString(),
			PatternType: p.PatternType.ValueString(),
		})
	}
	return out
}

// buildDlpCustomDetectionTypeCreateRequest converts the planned model to the
// create body.
func buildDlpCustomDetectionTypeCreateRequest(plan *dlpCustomDetectionTypeResourceModel) *dlpCustomDetectionTypeCreateRequest {
	body := &dlpCustomDetectionTypeCreateRequest{
		Name:     plan.Name.ValueString(),
		Patterns: dlpCustomDetectionPatternPayloads(plan.Patterns),
	}
	if v, ok := knownString(plan.Description); ok {
		body.Description = &v
	}
	if v, ok := knownString(plan.DefaultSeverity); ok {
		body.DefaultSeverity = &v
	}
	if v, ok := knownString(plan.DefaultConfidence); ok {
		body.DefaultConfidence = &v
	}
	return body
}

// buildDlpCustomDetectionTypeUpdateRequest converts the planned model to the
// update body, carrying the full desired state (see the request type's doc
// comment for the convergence contract).
func buildDlpCustomDetectionTypeUpdateRequest(plan *dlpCustomDetectionTypeResourceModel) *dlpCustomDetectionTypeUpdateRequest {
	name := plan.Name.ValueString()
	// "" clears the description server-side (an absent key would keep it).
	description := plan.Description.ValueString()
	patterns := dlpCustomDetectionPatternPayloads(plan.Patterns)

	body := &dlpCustomDetectionTypeUpdateRequest{
		Name:        &name,
		Description: &description,
		Patterns:    &patterns,
	}
	// Computed-when-unset attributes carry their last-known values through the
	// plan (UseStateForUnknown), so these are known on every ordinary update;
	// they are only omitted when a value is still unresolved mid-plan.
	if v, ok := knownString(plan.DefaultSeverity); ok {
		body.DefaultSeverity = &v
	}
	if v, ok := knownString(plan.DefaultConfidence); ok {
		body.DefaultConfidence = &v
	}
	return body
}

// applyDlpCustomDetectionTypeResponse maps the server's view onto a state
// model. prior is the plan (Create/Update) or previous state (Read), used to
// settle the cleared description back to null when the configuration said
// nothing.
func applyDlpCustomDetectionTypeResponse(detectionType *dlpCustomDetectionTypeResponse, prior *dlpCustomDetectionTypeResourceModel) dlpCustomDetectionTypeResourceModel {
	patterns := make([]dlpCustomDetectionPatternModel, 0, len(detectionType.Patterns))
	for _, p := range detectionType.Patterns {
		patterns = append(patterns, dlpCustomDetectionPatternModel{
			Pattern:     types.StringValue(p.Pattern),
			PatternType: types.StringValue(p.PatternType),
		})
	}

	return dlpCustomDetectionTypeResourceModel{
		ID:                types.StringValue(detectionType.ID),
		OrgID:             types.StringValue(detectionType.OrgID),
		DetectionType:     types.StringValue(detectionType.DetectionType),
		Name:              types.StringValue(detectionType.Name),
		Description:       optionalStringFromPtr(&detectionType.Description, prior.Description),
		Patterns:          patterns,
		DetectionGroup:    types.StringValue(detectionType.Group),
		DefaultSeverity:   types.StringValue(detectionType.DefaultSeverity),
		DefaultConfidence: types.StringValue(detectionType.DefaultConfidence),
		CreatedAt:         types.StringValue(detectionType.CreatedAt),
		UpdatedAt:         types.StringValue(detectionType.UpdatedAt),
	}
}
