// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// llmRouteGroupMaxMembers mirrors the platform's MAX_MEMBERS_PER_REQUEST: the
// update API replaces the membership in one request, so a larger set would
// always be rejected.
const llmRouteGroupMaxMembers = 1000

// llmTrimmedFreeText accepts the empty string or text without surrounding
// whitespace: the platform trims free-text fields such as descriptions, so a
// padded value would read back changed.
var llmTrimmedFreeText = stringvalidator.RegexMatches(
	regexp.MustCompile(`^(?s:\S(.*\S)?)?$`),
	"must not have leading/trailing whitespace",
)

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                = &llmModelRouteGroupResource{}
	_ resource.ResourceWithConfigure   = &llmModelRouteGroupResource{}
	_ resource.ResourceWithImportState = &llmModelRouteGroupResource{}
)

// NewLlmModelRouteGroupResource returns a new barndoor_llm_model_route_group
// resource.
func NewLlmModelRouteGroupResource() resource.Resource {
	return &llmModelRouteGroupResource{}
}

// llmModelRouteGroupResource manages an LLM Gateway model route group (a
// named set of route aliases that a model-access policy can target as one)
// through the llm-gateway-service admin REST API
// (`/api/llm-gateway/admin/model-route-groups`).
type llmModelRouteGroupResource struct {
	client *client.Client
}

// llmModelRouteGroupResourceModel maps the resource schema to Go types.
type llmModelRouteGroupResourceModel struct {
	ID           types.String `tfsdk:"id"`
	OrgID        types.String `tfsdk:"org_id"`
	Name         types.String `tfsdk:"name"`
	Description  types.String `tfsdk:"description"`
	ModelAliases types.Set    `tfsdk:"model_aliases"`
	ChangeNote   types.String `tfsdk:"change_note"`
}

func (r *llmModelRouteGroupResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_llm_model_route_group"
}

func (r *llmModelRouteGroupResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an LLM Gateway model route group: a named set of route aliases " +
			"(the `model_alias` of `barndoor_llm_model_mapping` routes) that a " +
			"`barndoor_llm_model_access` policy can target as one, with a `kind = \"route_group\"` " +
			"target.\n\n" +
			"The platform also changes membership without an edit to the group. Renaming a route's " +
			"alias moves its memberships to the new alias, and deleting a route's last mapping removes " +
			"the alias from every group. Terraform reads the membership back from the platform, so " +
			"either change shows as a diff on `model_aliases` on the next plan, and applying restores " +
			"the configured set. Update the configuration to accept the change instead.\n\n" +
			"Members are not checked against existing routes: an alias with no mapping is stored and " +
			"simply matches nothing until a route with that alias exists.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Route group UUID assigned by the API; also the `terraform import` key.",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"org_id": schema.StringAttribute{
				MarkdownDescription: "Organization the route group belongs to, resolved from the provider " +
					"credential's token claims.",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Group name, unique within the organization ignoring case; the " +
					"platform answers 409 on a duplicate.",
				Required: true,
				Validators: []validator.String{
					noSurroundingWhitespace,
				},
			},
			"description": schema.StringAttribute{
				MarkdownDescription: "What the group is for. Defaults to an empty string, which is also " +
					"what removing it from configuration sets.",
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(""),
				Validators: []validator.String{
					llmTrimmedFreeText,
				},
			},
			"model_aliases": schema.SetAttribute{
				MarkdownDescription: "Route aliases that belong to the group, at most " +
					fmt.Sprint(llmRouteGroupMaxMembers) + ". Every apply replaces the platform's " +
					"membership with this set; omit it or set `[]` for an empty group. An empty group " +
					"expands to no targets in the policies that reference it.",
				ElementType: types.StringType,
				Optional:    true,
				Computed:    true,
				Default:     setdefault.StaticValue(types.SetValueMust(types.StringType, []attr.Value{})),
				Validators: []validator.Set{
					setvalidator.SizeAtMost(llmRouteGroupMaxMembers),
					setvalidator.ValueStringsAre(noSurroundingWhitespace),
				},
			},
			"change_note": llmChangeNoteAttribute("route group"),
		},
	}
}

func (r *llmModelRouteGroupResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *llmModelRouteGroupResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan llmModelRouteGroupResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	aliases, diags := llmRouteGroupAliases(ctx, plan.ModelAliases)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := &llmRouteGroupCreateRequest{
		Name:         plan.Name.ValueString(),
		Description:  plan.Description.ValueString(),
		ModelAliases: aliases,
	}
	if v, ok := knownString(plan.ChangeNote); ok {
		body.ChangeNote = &v
	}

	var group llmRouteGroupResponse
	if err := doJSON(ctx, r.client, http.MethodPost, llmGatewayAPIPrefix+"/model-route-groups", body, &group); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM model route group", "create the LLM model route group", err)
		return
	}
	if group.ID == "" {
		resp.Diagnostics.AddError("Malformed LLM Gateway API response", "Create returned no route group id.")
		return
	}

	state, diags := applyLlmRouteGroupResponse(ctx, &group, &plan)
	resp.Diagnostics.Append(diags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Read refreshes the group from the org-wide listing: the admin API has no
// get-by-id endpoint for route groups. model_aliases is always taken from
// the server, so a platform-side membership sweep (a route rename or a
// route's last mapping deleted) surfaces as drift.
func (r *llmModelRouteGroupResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state llmModelRouteGroupResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var groups []llmRouteGroupResponse
	if err := doJSON(ctx, r.client, http.MethodGet, llmGatewayAPIPrefix+"/model-route-groups", nil, &groups); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM model route group", "list LLM model route groups", err)
		return
	}

	for i := range groups {
		if groups[i].ID == state.ID.ValueString() {
			newState, diags := applyLlmRouteGroupResponse(ctx, &groups[i], &state)
			resp.Diagnostics.Append(diags...)
			resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
			return
		}
	}

	// Deleted out-of-band; drop the resource so Terraform plans a recreate.
	resp.State.RemoveResource(ctx)
}

func (r *llmModelRouteGroupResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan llmModelRouteGroupResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	aliases, diags := llmRouteGroupAliases(ctx, plan.ModelAliases)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Every managed field is sent: name and description are COALESCEd and
	// model_aliases replaces the membership wholesale, so the body is the full
	// desired state. change_note is sent whenever it is configured, because
	// the API clears the stored note on an update that omits it.
	name := plan.Name.ValueString()
	description := plan.Description.ValueString()
	body := &llmRouteGroupUpdateRequest{
		Name:         &name,
		Description:  &description,
		ModelAliases: &aliases,
	}
	if v, ok := knownString(plan.ChangeNote); ok {
		body.ChangeNote = &v
	}

	var group llmRouteGroupResponse
	if err := doJSON(ctx, r.client, http.MethodPut,
		llmGatewayAPIPrefix+"/model-route-groups/"+plan.ID.ValueString(), body, &group); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM model route group", "update the LLM model route group", err)
		return
	}

	newState, diags := applyLlmRouteGroupResponse(ctx, &group, &plan)
	resp.Diagnostics.Append(diags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Delete removes the group. A 404 means it is already gone — success for a
// destroy. The platform never refuses the delete: a model-access policy that
// targets the group keeps a dangling target that matches nothing.
func (r *llmModelRouteGroupResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state llmModelRouteGroupResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := doJSON(ctx, r.client, http.MethodDelete,
		llmGatewayAPIPrefix+"/model-route-groups/"+state.ID.ValueString(), nil, nil)
	if err != nil && !isNotFound(err) {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM model route group", "delete the LLM model route group", err)
	}
}

func (r *llmModelRouteGroupResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *llmModelRouteGroupResource) requireClient(diags *diag.Diagnostics) bool {
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

// llmRouteGroupCreateRequest mirrors the llm-gateway CreateRouteGroupRequest
// body.
type llmRouteGroupCreateRequest struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	ModelAliases []string `json:"model_aliases"`
	ChangeNote   *string  `json:"change_note,omitempty"`
}

// llmRouteGroupUpdateRequest mirrors the llm-gateway UpdateRouteGroupRequest
// body. An omitted name/description keeps the stored value, a present
// model_aliases replaces the membership (`[]` empties it), and an omitted
// change_note clears the stored note.
type llmRouteGroupUpdateRequest struct {
	Name         *string   `json:"name,omitempty"`
	Description  *string   `json:"description,omitempty"`
	ModelAliases *[]string `json:"model_aliases,omitempty"`
	ChangeNote   *string   `json:"change_note,omitempty"`
}

// llmRouteGroupResponse mirrors the llm-gateway ModelRouteGroup response. The
// flattened change attribution (created_by_*, last_modified_*,
// last_change_note) is not mapped: none of it is managed configuration.
type llmRouteGroupResponse struct {
	ID           string   `json:"id"`
	OrgID        string   `json:"org_id"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	ModelAliases []string `json:"model_aliases"`
}

// llmRouteGroupAliases converts the planned membership to the wire list; a
// null or unknown set sends an empty list.
func llmRouteGroupAliases(ctx context.Context, set types.Set) ([]string, diag.Diagnostics) {
	aliases := []string{}
	if set.IsNull() || set.IsUnknown() {
		return aliases, nil
	}
	diags := set.ElementsAs(ctx, &aliases, false)
	return aliases, diags
}

// applyLlmRouteGroupResponse maps the server's view onto a state model. prior
// is the plan (Create/Update) or previous state (Read); only change_note is
// carried over from it, because the API never returns the note as written.
func applyLlmRouteGroupResponse(ctx context.Context, group *llmRouteGroupResponse, prior *llmModelRouteGroupResourceModel) (llmModelRouteGroupResourceModel, diag.Diagnostics) {
	aliases := group.ModelAliases
	if aliases == nil {
		aliases = []string{}
	}
	set, diags := types.SetValueFrom(ctx, types.StringType, aliases)

	return llmModelRouteGroupResourceModel{
		ID:           types.StringValue(group.ID),
		OrgID:        types.StringValue(group.OrgID),
		Name:         types.StringValue(group.Name),
		Description:  types.StringValue(group.Description),
		ModelAliases: set,
		ChangeNote:   prior.ChangeNote,
	}, diags
}
