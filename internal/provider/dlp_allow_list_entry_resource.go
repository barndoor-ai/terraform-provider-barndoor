// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// dlpAllowListPageSize is the page size requested when walking the allow
// list, so a lookup makes as few round trips as possible.
const dlpAllowListPageSize = 100

// dlpAllowListMaxPages bounds how many pages a lookup will walk before giving
// up; it only guards against a misbehaving pagination envelope looping
// forever.
const dlpAllowListMaxPages = 100

// Allow-list pattern type wire values (the proto str_name forms).
var dlpPatternTypes = []string{"PATTERN_TYPE_LITERAL", "PATTERN_TYPE_REGEX"}

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                = &dlpAllowListEntryResource{}
	_ resource.ResourceWithConfigure   = &dlpAllowListEntryResource{}
	_ resource.ResourceWithImportState = &dlpAllowListEntryResource{}
)

// NewDlpAllowListEntryResource returns a new barndoor_dlp_allow_list_entry resource.
func NewDlpAllowListEntryResource() resource.Resource {
	return &dlpAllowListEntryResource{}
}

// dlpAllowListEntryResource manages one Data Protection allow-list entry
// through the dlp-service tenant admin REST API
// (`/api/dlp/admin/v1/allow-list`). The API offers create, paginated list,
// and delete only — there is no update and no get-by-id — so every attribute
// forces replacement and Read walks the list to find the entry.
type dlpAllowListEntryResource struct {
	client *client.Client
}

// dlpAllowListEntryResourceModel maps the resource schema to Go types.
type dlpAllowListEntryResourceModel struct {
	ID             types.String `tfsdk:"id"`
	OrgID          types.String `tfsdk:"org_id"`
	Pattern        types.String `tfsdk:"pattern"`
	PatternType    types.String `tfsdk:"pattern_type"`
	DetectionTypes types.List   `tfsdk:"detection_types"`
	Reason         types.String `tfsdk:"reason"`
	CreatedBy      types.String `tfsdk:"created_by"`
	CreatedAt      types.String `tfsdk:"created_at"`
}

func (r *dlpAllowListEntryResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dlp_allow_list_entry"
}

func (r *dlpAllowListEntryResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages one Data Protection (DLP) allow-list entry: a literal value or regular " +
			"expression that detection findings are suppressed for, optionally limited to specific detection " +
			"types.\n\nThe platform API has no update endpoint for allow-list entries, so **changing any " +
			"attribute replaces the entry** (delete + create).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Entry UUID assigned by the API; also the `terraform import` key.",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"org_id": schema.StringAttribute{
				MarkdownDescription: "Organization the entry belongs to, resolved from the provider " +
					"credential's token claims.",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"pattern": schema.StringAttribute{
				MarkdownDescription: "The value to allow: a literal string or a regular expression, per " +
					"`pattern_type`. Changing it forces a new entry.",
				Required: true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"pattern_type": schema.StringAttribute{
				MarkdownDescription: "How `pattern` matches: `PATTERN_TYPE_LITERAL` or " +
					"`PATTERN_TYPE_REGEX`. Changing it forces a new entry.",
				Required: true,
				Validators: []validator.String{
					stringvalidator.OneOf(dlpPatternTypes...),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"detection_types": schema.ListAttribute{
				MarkdownDescription: "Detection types the entry suppresses (built-in `DETECTION_TYPE_*` " +
					"names or an organization's custom detection type names). Omit to suppress matches " +
					"for every detection type. Changing it forces a new entry.",
				ElementType: types.StringType,
				Optional:    true,
				Validators: []validator.List{
					listvalidator.UniqueValues(),
					listvalidator.ValueStringsAre(stringvalidator.LengthAtLeast(1)),
				},
				PlanModifiers: []planmodifier.List{
					listplanmodifier.RequiresReplace(),
				},
			},
			"reason": schema.StringAttribute{
				MarkdownDescription: "Why the value is allowed (free-form, for audit context). Changing it " +
					"forces a new entry.",
				Optional: true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"created_by": schema.StringAttribute{
				MarkdownDescription: "Subject that created the entry (empty when the platform could not " +
					"attribute it).",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"created_at": schema.StringAttribute{
				MarkdownDescription: "When the entry was created (RFC 3339).",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *dlpAllowListEntryResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *dlpAllowListEntryResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan dlpAllowListEntryResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, err := buildDlpAllowListEntryCreateRequest(ctx, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid allow-list entry configuration", err.Error())
		return
	}

	var entry dlpAllowListEntryResponse
	if err := doJSON(ctx, r.client, http.MethodPost, dlpAPIPrefix+"/allow-list", body, &entry); err != nil {
		addDlpAllowListEntryAPIError(&resp.Diagnostics, "create the allow-list entry", err)
		return
	}
	if entry.ID == "" {
		resp.Diagnostics.AddError("Malformed Data Protection API response", "Create returned no entry id.")
		return
	}

	state, err := applyDlpAllowListEntryResponse(ctx, &entry, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to map the Data Protection API response", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Read finds the entry by walking the paginated allow list — the API has no
// get-by-id endpoint. A missing entry means it was deleted out-of-band, so
// the resource is dropped for a recreate.
func (r *dlpAllowListEntryResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state dlpAllowListEntryResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	entry, err := r.findAllowListEntry(ctx, state.ID.ValueString())
	if err != nil {
		addDlpAllowListEntryAPIError(&resp.Diagnostics, "read the allow list", err)
		return
	}
	if entry == nil {
		resp.State.RemoveResource(ctx)
		return
	}

	newState, err := applyDlpAllowListEntryResponse(ctx, entry, &state)
	if err != nil {
		resp.Diagnostics.AddError("Failed to map the Data Protection API response", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Update never runs: the API has no update endpoint, so every configurable
// attribute forces replacement. The framework still requires the method to
// exist.
func (r *dlpAllowListEntryResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError(
		"Unexpected update of barndoor_dlp_allow_list_entry",
		"Every configurable attribute of barndoor_dlp_allow_list_entry forces replacement, so an in-place "+
			"update should be impossible. This is a bug in the provider.",
	)
}

// Delete removes the entry. A 404 means it is already gone — success for a
// destroy.
func (r *dlpAllowListEntryResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state dlpAllowListEntryResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := doJSON(ctx, r.client, http.MethodDelete,
		dlpAPIPrefix+"/allow-list/"+state.ID.ValueString(), nil, nil)
	if err != nil && !isNotFound(err) {
		addDlpAllowListEntryAPIError(&resp.Diagnostics, "delete the allow-list entry", err)
	}
}

func (r *dlpAllowListEntryResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *dlpAllowListEntryResource) requireClient(diags *diag.Diagnostics) bool {
	if r.client == nil {
		diags.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return false
	}
	return true
}

// findAllowListEntry pages through GET /allow-list until it finds id,
// returning nil (no error) when the entry does not exist. The page token is
// treated as opaque; the walk is bounded by dlpAllowListMaxPages.
func (r *dlpAllowListEntryResource) findAllowListEntry(ctx context.Context, id string) (*dlpAllowListEntryResponse, error) {
	pageToken := ""
	for page := 0; page < dlpAllowListMaxPages; page++ {
		query := url.Values{"page_size": {strconv.Itoa(dlpAllowListPageSize)}}
		if pageToken != "" {
			query.Set("page_token", pageToken)
		}

		var out dlpAllowListPage
		if err := doJSON(ctx, r.client, http.MethodGet, dlpAPIPrefix+"/allow-list?"+query.Encode(), nil, &out); err != nil {
			return nil, err
		}
		for i := range out.Items {
			if out.Items[i].ID == id {
				return &out.Items[i], nil
			}
		}

		if out.NextPageToken == "" || len(out.Items) == 0 {
			return nil, nil
		}
		pageToken = out.NextPageToken
	}
	return nil, fmt.Errorf("GET %s/allow-list: pagination did not terminate after %d pages",
		dlpAPIPrefix, dlpAllowListMaxPages)
}

// addDlpAllowListEntryAPIError turns a DLP admin API error into an actionable
// diagnostic. action is the failed verb phrase, e.g. "create the allow-list
// entry".
func addDlpAllowListEntryAPIError(diags *diag.Diagnostics, action string, err error) {
	apiErr, ok := asAPIError(err)
	if !ok {
		diags.AddError("Failed to "+action, err.Error())
		return
	}

	switch apiErr.status {
	case http.StatusUnprocessableEntity:
		// The API's validation message (unknown detection_type, unknown
		// pattern_type, …) is the most specific thing we can show; surface it
		// verbatim.
		diags.AddError("Allow-list entry rejected by the Data Protection API", apiErr.displayBody())
	case http.StatusUnauthorized, http.StatusForbidden:
		diags.AddError(
			"Permission denied by the Data Protection API",
			fmt.Sprintf("Failed to %s: the configured credential was not accepted. Confirm the "+
				"service-account credential's token carries the organization claim for the organization "+
				"that owns this entry.\n\nServer message: %s", action, apiErr.displayBody()),
		)
	default:
		diags.AddError("Failed to "+action, apiErr.Error())
	}
}

// --- request/response DTOs ---------------------------------------------------

// dlpAllowListEntryCreateRequest mirrors the dlp-service
// CreateAllowListEntryRequest body. detection_types and reason are sent only
// when configured (the API treats an absent list as "all detection types").
type dlpAllowListEntryCreateRequest struct {
	Pattern        string   `json:"pattern"`
	PatternType    string   `json:"pattern_type"`
	DetectionTypes []string `json:"detection_types,omitempty"`
	Reason         *string  `json:"reason,omitempty"`
}

// dlpAllowListEntryResponse mirrors the dlp-service AllowListEntryResponse.
// reason and created_by come back as "" when unset.
type dlpAllowListEntryResponse struct {
	ID             string   `json:"id"`
	OrgID          string   `json:"org_id"`
	Pattern        string   `json:"pattern"`
	PatternType    string   `json:"pattern_type"`
	DetectionTypes []string `json:"detection_types"`
	Reason         string   `json:"reason"`
	CreatedBy      string   `json:"created_by"`
	CreatedAt      string   `json:"created_at"`
}

// dlpAllowListPage mirrors the dlp-service PaginatedResponse envelope
// (`{"items": [...], "next_page_token": "...", "total_count": N}`; an empty
// next_page_token means the last page).
type dlpAllowListPage struct {
	Items         []dlpAllowListEntryResponse `json:"items"`
	NextPageToken string                      `json:"next_page_token"`
	TotalCount    int64                       `json:"total_count"`
}

// buildDlpAllowListEntryCreateRequest converts the planned model to the API
// body.
func buildDlpAllowListEntryCreateRequest(ctx context.Context, plan *dlpAllowListEntryResourceModel) (*dlpAllowListEntryCreateRequest, error) {
	detectionTypes, err := stringsFromList(ctx, plan.DetectionTypes)
	if err != nil {
		return nil, fmt.Errorf("detection_types: %w", err)
	}

	body := &dlpAllowListEntryCreateRequest{
		Pattern:        plan.Pattern.ValueString(),
		PatternType:    plan.PatternType.ValueString(),
		DetectionTypes: detectionTypes,
	}
	if v, ok := knownString(plan.Reason); ok {
		body.Reason = &v
	}
	return body, nil
}

// applyDlpAllowListEntryResponse maps the server's view onto a state model.
// prior is the plan (Create) or previous state (Read), used to settle the
// empty detection_types collection and the empty reason back to null when
// the configuration said nothing.
func applyDlpAllowListEntryResponse(ctx context.Context, entry *dlpAllowListEntryResponse, prior *dlpAllowListEntryResourceModel) (dlpAllowListEntryResourceModel, error) {
	m := dlpAllowListEntryResourceModel{
		ID:          types.StringValue(entry.ID),
		OrgID:       types.StringValue(entry.OrgID),
		Pattern:     types.StringValue(entry.Pattern),
		PatternType: types.StringValue(entry.PatternType),
		Reason:      optionalStringFromPtr(&entry.Reason, prior.Reason),
		CreatedBy:   types.StringValue(entry.CreatedBy),
		CreatedAt:   types.StringValue(entry.CreatedAt),
	}

	var err error
	if m.DetectionTypes, err = listFromStrings(ctx, entry.DetectionTypes, prior.DetectionTypes); err != nil {
		return dlpAllowListEntryResourceModel{}, fmt.Errorf("detection_types: %w", err)
	}
	return m, nil
}
