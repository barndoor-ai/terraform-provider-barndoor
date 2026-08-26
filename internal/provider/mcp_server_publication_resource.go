// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// Publish precondition retry tuning (see publishWithRetry): the window Create
// keeps retrying the precondition 422s, and the pause between attempts.
// Variables so tests can shrink them; production code never mutates them.
var (
	publishRetryWindow   = 2 * time.Minute
	publishRetryInterval = 5 * time.Second
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
// resource, the publication is ordered after the policies by referencing their
// ids in `policy_ids` — the reference is the dependency edge.
type mcpServerPublicationResource struct {
	client *client.Client
}

// mcpServerPublicationResourceModel maps the resource schema to Go types. The
// resource's identifier is mcp_server_id — a server has at most one
// publication.
type mcpServerPublicationResourceModel struct {
	McpServerID types.String `tfsdk:"mcp_server_id"`
	PolicyIDs   types.List   `tfsdk:"policy_ids"`
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
			"Publishing has two preconditions, and the platform refuses with a 422 until both hold. " +
			"First, the server must be **operationally available**: supplying credentials at create " +
			"activates it, whereas a server created without them stays `pending` and cannot be published " +
			"until someone connects it (servers from `embedded`/`local` directory entries need no " +
			"credentials). Second, it must have at least one **ACTIVE policy** — and since policies " +
			"reference the server's id, order this resource after them by referencing their ids in " +
			"`policy_ids` (see the example). A configuration that expresses no ordering still " +
			"converges when the policy lands during the same apply: creation retries the two " +
			"precondition rejections for up to two minutes before failing.\n\n" +
			"**Removing this resource from configuration does NOT unpublish the server** — unpublishing " +
			"does not exist. `terraform destroy` simply stops tracking the publication; the server stays " +
			"published and is untouched (its connections and credentials are never affected by this " +
			"resource). Publishing an already-published server is an idempotent no-op, so re-creating the " +
			"declaration later is always safe.",
		Attributes: map[string]schema.Attribute{
			"mcp_server_id": schema.StringAttribute{
				MarkdownDescription: "ID of the MCP server to publish; also the `terraform import` key. " +
					"A server has at most one publication. Importing a server that is not published yet " +
					"fails (there is no publication to import) — apply this resource to publish it instead.",
				Required: true,
				PlanModifiers: []planmodifier.String{
					// Replacing the publication never touches the old server:
					// Delete is a state-only operation (there is no unpublish).
					stringplanmodifier.RequiresReplace(),
				},
			},
			"policy_ids": schema.ListAttribute{
				MarkdownDescription: "IDs of the ACTIVE policies this publication is ordered after. " +
					"Referencing them (`policy_ids = [barndoor_policy.x.id]`) is what makes Terraform create " +
					"the policies first — no `depends_on` needed (`depends_on` remains the fallback for a " +
					"policy that is not managed alongside). Ordering-only: the list is not validated against " +
					"the server's policies, changing it later is an in-place state update with no API call " +
					"(never a replacement), and it reads back null after `terraform import`.",
				ElementType: types.StringType,
				Optional:    true,
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
	server, err := publishWithRetry(ctx, r.client, serverID)
	if err != nil {
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
	err := doJSON(ctx, r.client, http.MethodGet, serverPath(state.McpServerID.ValueString(), ""), nil, &server)
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

// Update handles a policy_ids edit — the only in-place change the schema
// allows (mcp_server_id forces replacement). policy_ids is ordering-only and
// means nothing to the API once the server is published, so this is a pure
// state write with no API call; published_at is carried forward from state by
// UseStateForUnknown, so the plan already is the complete new state.
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

// publishWithRetry calls the publish endpoint, retrying ONLY the precondition
// 422s (isPublishPrecondition) for publishRetryWindow: a configuration with no
// ordering between policy and publication may create them concurrently, and
// the policy lands seconds after the first attempt. Every other error — 403,
// 404, 503, a validation 422 — is returned at once; a precondition that never
// comes true is returned after the window.
func publishWithRetry(ctx context.Context, c *client.Client, serverID string) (*mcpServerResponse, error) {
	path := serverPath(serverID, "publish")
	deadline := time.Now().Add(publishRetryWindow)
	timer := time.NewTimer(publishRetryInterval)
	defer timer.Stop()
	for {
		var server mcpServerResponse
		err := doJSON(ctx, c, http.MethodPost, path, nil, &server)
		if err == nil {
			return &server, nil
		}
		if !isPublishPrecondition(err) || !time.Now().Before(deadline) {
			return nil, err
		}
		tflog.Debug(ctx, "Publish precondition not met yet; retrying", map[string]any{
			"server_id": serverID,
			"error":     err.Error(),
		})
		timer.Reset(publishRetryInterval)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// publishPreconditionPrefix opens both precondition rejections the publish
// route raises ("Cannot publish: server is not operationally available" /
// "… has no ACTIVE policy"; registry-service routes/servers.py).
const publishPreconditionPrefix = "Cannot publish:"

// isPublishPrecondition reports whether err is a publish precondition
// rejection — the only errors worth waiting out. A 422 whose body is not a
// string message is FastAPI request validation (a `detail` array, e.g. a
// malformed mcp_server_id); a 422 with an unrecognised message is a final
// verdict. Neither is retried.
func isPublishPrecondition(err error) bool {
	apiErr, ok := asAPIError(err)
	return ok && apiErr.status == http.StatusUnprocessableEntity &&
		strings.HasPrefix(apiErr.displayBody(), publishPreconditionPrefix)
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
		// 422 covers two unrelated things: the publish preconditions (a string
		// `detail`) and FastAPI request validation, e.g. an mcp_server_id that
		// is not a UUID (a `detail` ARRAY, which displayBody cannot reduce to a
		// message). Only claim the precondition story for the former —
		// otherwise a malformed id is reported as a missing ACTIVE policy.
		if !apiErr.hasMessage() {
			diags.AddError(
				"MCP server publish request rejected",
				fmt.Sprintf("The registry rejected the publish request for server %s as invalid. Check "+
					"that mcp_server_id is a well-formed server id.\n\nServer response: %s",
					serverID, apiErr.displayBody()),
			)
			return
		}
		// The API distinguishes "not operationally available" from "no ACTIVE
		// policy"; surface its message and the fix for each.
		diags.AddError(
			"MCP server cannot be published yet",
			fmt.Sprintf("The registry refused to publish server %s: %s\n\nPublishing requires the server "+
				"to be operationally available AND to have at least one ACTIVE policy. If the policy is "+
				"managed in this configuration, order the publication after it with "+
				"`policy_ids = [barndoor_policy.<name>.id]`.", serverID, apiErr.displayBody()),
		)
	case http.StatusForbidden:
		// The admin authorization check on the server runs BEFORE the
		// ACTIVE-policy verification, and both require the same admin role —
		// so in practice this is "the credential is not an org admin", with an
		// opaque "Forbidden" body. Don't attribute it to a policy check the
		// caller almost certainly never reached.
		diags.AddError(
			"Permission denied by the registry API",
			fmt.Sprintf("Failed to publish server %s: the configured credential is not authorized to "+
				"publish it. Confirm the service-account credential carries the organization admin "+
				"role — publishing needs it both to modify the server and to verify its ACTIVE "+
				"policies.\n\nServer message: %s", serverID, apiErr.displayBody()),
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
