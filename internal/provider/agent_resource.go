// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                = &agentResource{}
	_ resource.ResourceWithConfigure   = &agentResource{}
	_ resource.ResourceWithImportState = &agentResource{}
)

// NewAgentResource returns a new barndoor_agent resource.
func NewAgentResource() resource.Resource {
	return &agentResource{}
}

// agentResource manages an AI Agent registration (a registry `Application`)
// through the registry-service public REST API (`/api/registry/v1/agents`).
type agentResource struct {
	client *client.Client
}

// agentResourceModel maps the resource schema to Go types.
type agentResourceModel struct {
	ID                         types.String `tfsdk:"id"`
	ApplicationDirectoryID     types.String `tfsdk:"application_directory_id"`
	WriteConfirmationsRequired types.Bool   `tfsdk:"write_confirmations_required"`
	LlmGatewayEnabled          types.Bool   `tfsdk:"llm_gateway_enabled"`
	Name                       types.String `tfsdk:"name"`
	AgentType                  types.String `tfsdk:"agent_type"`
}

func (r *agentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_agent"
}

func (r *agentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an AI Agent registration in the organization: the binding of an agent " +
			"directory entry (the OAuth client definition) to the organization, plus its per-agent platform " +
			"toggles. Registering an agent also attaches the directory's machine-to-machine service account " +
			"to the organization. An organization can hold **one live registration per directory entry**. " +
			"`terraform destroy` unregisters the agent (a soft-delete): the service account is detached and " +
			"policies bound to the agent are archived by the platform.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Agent registration UUID assigned by the API; also the " +
					"`terraform import` key. (This is the id referenced by `barndoor_policy.application_ids`.)",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"application_directory_id": schema.StringAttribute{
				MarkdownDescription: "ID of the agent directory entry (the OAuth client definition) this " +
					"registration binds to the organization. Changing it forces a new registration.",
				Required: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"write_confirmations_required": schema.BoolAttribute{
				MarkdownDescription: "Whether tool calls that write require an end-user confirmation. " +
					"Defaults to `true` on the server.",
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"llm_gateway_enabled": schema.BoolAttribute{
				MarkdownDescription: "Whether the agent may use the LLM Gateway. Defaults to `false` on " +
					"the server.",
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Display name of the AI Agent, from its directory entry.",
				Computed:            true,
			},
			"agent_type": schema.StringAttribute{
				MarkdownDescription: "Agent classification derived from the directory entry: `internal` " +
					"(organization-defined client) or `external` (dynamically-registered client).",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *agentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *agentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan agentResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := agentRegisterRequest{
		ApplicationDirectoryID: plan.ApplicationDirectoryID.ValueString(),
	}
	if !plan.WriteConfirmationsRequired.IsNull() && !plan.WriteConfirmationsRequired.IsUnknown() {
		v := plan.WriteConfirmationsRequired.ValueBool()
		body.WriteConfirmationsRequired = &v
	}
	if !plan.LlmGatewayEnabled.IsNull() && !plan.LlmGatewayEnabled.IsUnknown() {
		v := plan.LlmGatewayEnabled.ValueBool()
		body.LlmGatewayEnabled = &v
	}

	// The register endpoint returns the bare registration row (no directory
	// join), so fetch the full object afterwards for the computed name and
	// agent_type.
	var created agentResponse
	if err := doJSON(ctx, r.client, http.MethodPost, registryAPIPrefix+"/agents", body, &created); err != nil {
		addAgentAPIError(&resp.Diagnostics, "register the AI Agent", err)
		return
	}
	if created.ID == "" {
		resp.Diagnostics.AddError("Malformed registry API response", "Register returned no agent id.")
		return
	}

	var agent agentResponse
	if err := doJSON(ctx, r.client, http.MethodGet, registryAPIPrefix+"/agents/"+created.ID, nil, &agent); err != nil {
		// The registration exists; track it (with unknowable computed fields
		// nulled) so a subsequent apply reconciles instead of orphaning it.
		resp.Diagnostics.AddError("Failed to read the AI Agent after create", err.Error())
		plan.ID = types.StringValue(created.ID)
		plan.WriteConfirmationsRequired = types.BoolValue(created.WriteConfirmationsRequired)
		plan.LlmGatewayEnabled = types.BoolValue(created.LlmGatewayEnabled)
		plan.Name = types.StringNull()
		plan.AgentType = types.StringNull()
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, applyAgentResponse(&agent))...)
}

func (r *agentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state agentResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var agent agentResponse
	err := doJSON(ctx, r.client, http.MethodGet, registryAPIPrefix+"/agents/"+state.ID.ValueString(), nil, &agent)
	if err != nil {
		if isNotFound(err) {
			// Unregistered out-of-band (the API 404s soft-deleted rows); drop
			// it so Terraform plans a recreate.
			resp.State.RemoveResource(ctx)
			return
		}
		addAgentAPIError(&resp.Diagnostics, "read the AI Agent", err)
		return
	}
	if agent.DeletedAt != nil {
		// Defense-in-depth: a soft-deleted row that still reads back is a
		// deleted agent in Terraform terms.
		resp.State.RemoveResource(ctx)
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, applyAgentResponse(&agent))...)
}

// Update reconciles the two per-agent toggles through their dedicated PATCH
// endpoints (the registration binding itself is immutable — RequiresReplace).
func (r *agentResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan, state agentResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id := state.ID.ValueString()

	if known(plan.WriteConfirmationsRequired) && !plan.WriteConfirmationsRequired.Equal(state.WriteConfirmationsRequired) {
		body := map[string]bool{"write_confirmations_required": plan.WriteConfirmationsRequired.ValueBool()}
		if err := doJSON(ctx, r.client, http.MethodPatch, registryAPIPrefix+"/agents/"+id+"/write-confirmations", body, nil); err != nil {
			addAgentAPIError(&resp.Diagnostics, "update the AI Agent's write-confirmation setting", err)
			return
		}
	}
	if known(plan.LlmGatewayEnabled) && !plan.LlmGatewayEnabled.Equal(state.LlmGatewayEnabled) {
		body := map[string]bool{"llm_gateway_enabled": plan.LlmGatewayEnabled.ValueBool()}
		if err := doJSON(ctx, r.client, http.MethodPatch, registryAPIPrefix+"/agents/"+id+"/llm-gateway", body, nil); err != nil {
			addAgentAPIError(&resp.Diagnostics, "update the AI Agent's LLM Gateway setting", err)
			return
		}
	}

	var agent agentResponse
	if err := doJSON(ctx, r.client, http.MethodGet, registryAPIPrefix+"/agents/"+id, nil, &agent); err != nil {
		addAgentAPIError(&resp.Diagnostics, "read the AI Agent after update", err)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, applyAgentResponse(&agent))...)
}

// Delete unregisters the agent (a soft-delete): the platform detaches the
// directory's service account from the organization and archives dependent
// policies. The API is idempotent (a re-delete returns 204) and a 404 means
// it is already gone — success for a destroy either way.
func (r *agentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state agentResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := doJSON(ctx, r.client, http.MethodDelete, registryAPIPrefix+"/agents/"+state.ID.ValueString(), nil, nil)
	if err != nil && !isNotFound(err) {
		addAgentAPIError(&resp.Diagnostics, "unregister (destroy) the AI Agent", err)
	}
}

func (r *agentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *agentResource) requireClient(diags *diag.Diagnostics) bool {
	if r.client == nil {
		diags.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return false
	}
	return true
}

// known reports whether a bool value is set (neither null nor unknown).
func known(v types.Bool) bool {
	return !v.IsNull() && !v.IsUnknown()
}

// addAgentAPIError turns a registry API error into an actionable diagnostic.
// action is the failed verb phrase, e.g. "register the AI Agent".
func addAgentAPIError(diags *diag.Diagnostics, action string, err error) {
	apiErr, ok := asAPIError(err)
	if !ok {
		diags.AddError("Failed to "+action, err.Error())
		return
	}

	switch apiErr.status {
	case http.StatusUnprocessableEntity:
		detail := apiErr.displayBody()
		msg := detail
		// The API reports a duplicate live registration for the same directory
		// entry as a generic constraint violation; name the actual rule.
		if strings.Contains(detail, "constraint violation") {
			msg = detail + "\n\nAn organization can hold only one live registration per agent directory " +
				"entry — this directory is probably already registered. Import the existing registration " +
				"(`terraform import`) or unregister it first."
		}
		diags.AddError("AI Agent registration rejected by the registry API", msg)
	case http.StatusForbidden:
		diags.AddError(
			"Permission denied by the registry API",
			fmt.Sprintf("Failed to %s: %s\n\nThis is either a protected platform agent (which cannot be "+
				"unregistered) or the configured credential lacks the organization admin role for this "+
				"organization.", action, apiErr.displayBody()),
		)
	default:
		diags.AddError("Failed to "+action, apiErr.Error())
	}
}

// --- request/response DTOs ---------------------------------------------------

// agentRegisterRequest mirrors the registry ApplicationPayload body. The
// toggles are sent only when configured, so the server's defaults apply
// otherwise.
type agentRegisterRequest struct {
	ApplicationDirectoryID     string `json:"application_directory_id"`
	WriteConfirmationsRequired *bool  `json:"write_confirmations_required,omitempty"`
	LlmGatewayEnabled          *bool  `json:"llm_gateway_enabled,omitempty"`
}

// agentDirectoryDTO is the subset of the joined directory entry this resource
// surfaces.
type agentDirectoryDTO struct {
	Name string `json:"name"`
}

// agentResponse mirrors the registry ApplicationResponse (and the bare
// Application row the register endpoint returns — the directory join and
// agent_type are simply absent there).
type agentResponse struct {
	ID                         string             `json:"id"`
	ApplicationDirectoryID     string             `json:"application_directory_id"`
	WriteConfirmationsRequired bool               `json:"write_confirmations_required"`
	LlmGatewayEnabled          bool               `json:"llm_gateway_enabled"`
	AgentType                  string             `json:"agent_type"`
	DeletedAt                  *string            `json:"deleted_at"`
	ApplicationDirectory       *agentDirectoryDTO `json:"application_directory"`
}

// applyAgentResponse maps the server's view onto a state model. Every
// attribute is authoritative server-side (there are no write-only fields on
// this resource).
func applyAgentResponse(agent *agentResponse) *agentResourceModel {
	m := &agentResourceModel{
		ID:                         types.StringValue(agent.ID),
		ApplicationDirectoryID:     types.StringValue(agent.ApplicationDirectoryID),
		WriteConfirmationsRequired: types.BoolValue(agent.WriteConfirmationsRequired),
		LlmGatewayEnabled:          types.BoolValue(agent.LlmGatewayEnabled),
		Name:                       types.StringNull(),
		AgentType:                  types.StringNull(),
	}
	if agent.ApplicationDirectory != nil {
		m.Name = types.StringValue(agent.ApplicationDirectory.Name)
	}
	if agent.AgentType != "" {
		m.AgentType = types.StringValue(agent.AgentType)
	}
	return m
}
