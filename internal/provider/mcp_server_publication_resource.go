// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                = &mcpServerPublicationResource{}
	_ resource.ResourceWithConfigure   = &mcpServerPublicationResource{}
	_ resource.ResourceWithImportState = &mcpServerPublicationResource{}
)

// NewMcpServerPublicationResource returns a new barndoor_mcp_server_publication resource.
func NewMcpServerPublicationResource() resource.Resource {
	return &mcpServerPublicationResource{}
}

// mcpServerPublicationResource publishes an MCP server — the one-way,
// admin-initiated step that makes a server discoverable to end users
// (`POST /api/registry/v1/servers/{id}/publish`). It is a separate resource
// rather than a flag on barndoor_mcp_server because publishing requires the
// server to already have an ACTIVE policy, and policies are separate resources
// that reference the server's id — a flag on the server itself would have to
// fire before any policy could exist and would fail every time. As its own
// resource, the publication can be ordered after the policies with
// `depends_on`.
type mcpServerPublicationResource struct {
	client *client.Client
}

// mcpServerPublicationResourceModel maps the resource schema to Go types. The
// resource's identifier is mcp_server_id — a server has at most one
// publication.
type mcpServerPublicationResourceModel struct {
	McpServerID types.String `tfsdk:"mcp_server_id"`
	PublishedAt types.String `tfsdk:"published_at"`
}

func (r *mcpServerPublicationResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_mcp_server_publication"
}

func (r *mcpServerPublicationResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Publishes an MCP server, making it discoverable to end users. Publishing is " +
			"**one-way and deliberate**: there is no unpublish, and later operational-state changes never " +
			"hide a published server (taking a server out of service means deactivating its policies).\n\n" +
			"The platform refuses to publish a server without an ACTIVE policy, so order this resource " +
			"after the server's policies with `depends_on` (see the example).\n\n" +
			"**Removing this resource from configuration does NOT unpublish the server** — unpublishing " +
			"does not exist. `terraform destroy` simply stops tracking the publication; the server stays " +
			"published and is untouched (its connections and credentials are never affected by this " +
			"resource). Publishing an already-published server is an idempotent no-op, so re-creating the " +
			"declaration later is always safe.",
		Attributes: map[string]schema.Attribute{
			"mcp_server_id": schema.StringAttribute{
				MarkdownDescription: "ID of the MCP server to publish; also the `terraform import` key. " +
					"A server has at most one publication.",
				Required: true,
				PlanModifiers: []planmodifier.String{
					// Replacing the publication never touches the old server:
					// Delete is a state-only operation (there is no unpublish).
					stringplanmodifier.RequiresReplace(),
				},
			},
			"published_at": schema.StringAttribute{
				MarkdownDescription: "RFC 3339 timestamp of when the server was published. For a server " +
					"that was already published (e.g. from the admin UI), creating this resource adopts " +
					"the existing publication and reflects its original timestamp.",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *mcpServerPublicationResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// Create publishes the server. The API is idempotent: publishing an
// already-published server is a no-op success that returns the original
// published_at, so adopting an out-of-band publication works without special
// handling.
func (r *mcpServerPublicationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan mcpServerPublicationResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	serverID := plan.McpServerID.ValueString()
	var server mcpServerResponse
	if err := doJSON(ctx, r.client, http.MethodPost, registryAPIPrefix+"/servers/"+serverID+"/publish", nil, &server); err != nil {
		addPublishAPIError(&resp.Diagnostics, serverID, err)
		return
	}
	if server.PublishedAt == nil {
		resp.Diagnostics.AddError(
			"Malformed registry API response",
			"The publish call succeeded but the response carries no published_at timestamp.",
		)
		return
	}

	plan.PublishedAt = types.StringValue(*server.PublishedAt)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *mcpServerPublicationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state mcpServerPublicationResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var server mcpServerResponse
	err := doJSON(ctx, r.client, http.MethodGet, registryAPIPrefix+"/servers/"+state.McpServerID.ValueString(), nil, &server)
	if err != nil {
		if isNotFound(err) {
			// The server was deleted out-of-band; its publication is gone with it.
			resp.State.RemoveResource(ctx)
			return
		}
		addMcpServerAPIError(&resp.Diagnostics, "read the MCP server's publish state", err)
		return
	}

	if server.PublishedAt == nil {
		// Publishing is one-way, so a published server can only read back
		// unpublished if it was replaced out-of-band (deleted and re-created
		// under the same id lineage, restored from backup, …). Drop the
		// publication so the next apply republishes.
		resp.State.RemoveResource(ctx)
		return
	}

	state.PublishedAt = types.StringValue(*server.PublishedAt)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is unreachable: mcp_server_id forces replacement and published_at is
// computed. It exists to satisfy the resource.Resource interface.
func (r *mcpServerPublicationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan mcpServerPublicationResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete removes the publication from state WITHOUT unpublishing — there is no
// unpublish, by design. The server itself is untouched (never deleted, never
// recreated, connections intact); it simply stays published untracked.
func (r *mcpServerPublicationResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
	// Intentionally no API call. Returning without error removes the resource
	// from state.
}

// ImportState imports by server id. Importing errors for a server that exists
// but is unpublished (Read drops the resource), which is correct: an
// unpublished server has no publication to import — apply the resource to
// publish it instead.
func (r *mcpServerPublicationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("mcp_server_id"), req, resp)
}

func (r *mcpServerPublicationResource) requireClient(diags *diag.Diagnostics) bool {
	if r.client == nil {
		diags.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return false
	}
	return true
}

// addPublishAPIError turns a publish-endpoint error into an actionable
// diagnostic. The interesting cases are the publish preconditions: the server
// must be operationally available and must have at least one ACTIVE policy.
func addPublishAPIError(diags *diag.Diagnostics, serverID string, err error) {
	apiErr, ok := asAPIError(err)
	if !ok {
		diags.AddError("Failed to publish the MCP server", err.Error())
		return
	}

	switch apiErr.status {
	case http.StatusNotFound:
		diags.AddError(
			"MCP server not found",
			fmt.Sprintf("No MCP server with id %q exists in the organization (the API 404s deleted "+
				"servers), so there is nothing to publish.", serverID),
		)
	case http.StatusUnprocessableEntity:
		// The API distinguishes "not operationally available" from "no ACTIVE
		// policy"; surface its message and the fix for each.
		diags.AddError(
			"MCP server cannot be published yet",
			fmt.Sprintf("The registry refused to publish server %s: %s\n\nPublishing requires the server "+
				"to be operationally available AND to have at least one ACTIVE policy. If the policy is "+
				"managed in this configuration, order the publication after it with "+
				"`depends_on = [barndoor_policy.<name>]`.", serverID, apiErr.displayBody()),
		)
	case http.StatusForbidden:
		diags.AddError(
			"Permission denied by the registry API",
			fmt.Sprintf("Failed to publish server %s: the configured credential is not authorized to "+
				"verify the server's ACTIVE policies. Confirm the service-account credential carries the "+
				"organization admin role.\n\nServer message: %s", serverID, apiErr.displayBody()),
		)
	case http.StatusServiceUnavailable:
		diags.AddError(
			"Publish precondition check unavailable",
			fmt.Sprintf("The registry could not verify server %s's ACTIVE policies because "+
				"policy-service is unavailable. The server was not published; retry the apply.", serverID),
		)
	default:
		diags.AddError("Failed to publish the MCP server", apiErr.Error())
	}
}
