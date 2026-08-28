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
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// channelsPath is the collection endpoint; byID builds the per-channel paths.
var channelsPath = notificationAPIPrefix + "/channels"

func channelIDPath(id, suffix string) string {
	return fmt.Sprintf("%s/%s%s", channelsPath, id, suffix)
}

// buildChannelWriteRequest maps the plan to the upsert body, emitting only the
// destination fields that belong to the channel's type.
func buildChannelWriteRequest(plan *notificationChannelResourceModel, withID bool) *notificationChannelWriteRequest {
	body := &notificationChannelWriteRequest{
		Type:          plan.Type.ValueString(),
		Enabled:       plan.Enabled.ValueBool(),
		Subscriptions: []notificationSubscriptionBody{},
	}
	if withID {
		if id, ok := knownString(plan.ID); ok {
			body.ID = &id
		}
	}
	if v, ok := knownString(plan.EmailAddress); ok {
		body.EmailAddress = &v
	}
	if v, ok := knownString(plan.URL); ok {
		body.URL = &v
	}
	if v, ok := knownString(plan.Label); ok {
		body.Label = &v
	}
	if v, ok := knownString(plan.SlackChannelID); ok {
		body.SlackChannelID = &v
	}
	if v, ok := knownString(plan.TeamsWorkflowURL); ok {
		body.TeamsWorkflowURL = &v
	}
	for _, s := range sortedSubscriptions(plan.Subscriptions) {
		body.Subscriptions = append(body.Subscriptions, notificationSubscriptionBody{AlertType: s})
	}
	return body
}

// applyChannelResponse projects an API response onto state.
//
// Two attributes deliberately do NOT come from the response, because the API
// cannot return them:
//
//   - signing_secret: revealed exactly once. `revealed` carries a fresh value
//     when this response IS the reveal (create, or rotate); otherwise the prior
//     state value is preserved verbatim. Nulling it here would produce a
//     permanent diff on every plan, and refreshing it is impossible.
//   - teams_workflow_url: write-only, so state follows configuration.
func applyChannelResponse(
	ch *notificationChannelResponse,
	prior *notificationChannelResourceModel,
	revealed *string,
) *notificationChannelResourceModel {
	subs := make([]string, 0, len(ch.Subscriptions))
	for _, s := range ch.Subscriptions {
		subs = append(subs, s.AlertType)
	}
	subsValue := types.SetNull(types.StringType)
	// An absent config set and an empty server set are both "delivers nothing";
	// echoing an empty set where the config said nothing would be perpetual drift.
	if len(subs) > 0 || !(prior.Subscriptions.IsNull() || prior.Subscriptions.IsUnknown()) {
		set, _ := types.SetValueFrom(context.Background(), types.StringType, subs)
		subsValue = set
	}

	secret := nullIfUnknownString(prior.SigningSecret)
	if revealed != nil && *revealed != "" {
		secret = types.StringValue(*revealed)
	}

	return &notificationChannelResourceModel{
		ID:             types.StringValue(ch.ID),
		Type:           types.StringValue(ch.Type),
		Enabled:        types.BoolValue(ch.Enabled),
		EmailAddress:   optionalStringFromPtr(ch.EmailAddress, prior.EmailAddress),
		URL:            optionalStringFromPtr(ch.URL, prior.URL),
		Label:          optionalStringFromPtr(ch.Label, prior.Label),
		SlackChannelID: optionalStringFromPtr(ch.SlackChannelID, prior.SlackChannelID),

		// Write-only: state follows configuration.
		TeamsWorkflowURL: nullIfUnknownString(prior.TeamsWorkflowURL),

		Subscriptions:     subsValue,
		RotateWhenChanged: prior.RotateWhenChanged,
		SigningSecret:     secret,
		HasSigningSecret:  types.BoolValue(ch.HasSigningSecret),
		HasWorkflowURL:    types.BoolValue(ch.HasWorkflowURL),
		CreatedAt:         types.StringValue(ch.CreatedAt),
		UpdatedAt:         types.StringValue(ch.UpdatedAt),
	}
}

func addChannelAPIError(diags *diag.Diagnostics, action string, err error) {
	diags.AddError(fmt.Sprintf("Unable to %s", action), err.Error())
}

// findChannelByID lists the org's channels and returns the one with id.
// The API has no GET-by-id, so read is a filtered list.
func (r *notificationChannelResource) findChannelByID(
	ctx context.Context, id string, diags *diag.Diagnostics,
) (*notificationChannelResponse, bool) {
	var list notificationChannelListResponse
	if err := doJSON(ctx, r.client, http.MethodGet, channelsPath, nil, &list); err != nil {
		addChannelAPIError(diags, "read notification channels", err)
		return nil, false
	}
	for i := range list.Data {
		if list.Data[i].ID == id {
			return &list.Data[i], true
		}
	}
	return nil, false
}

func (r *notificationChannelResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}
	var plan notificationChannelResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var ch notificationChannelResponse
	if err := doJSON(ctx, r.client, http.MethodPut, channelsPath,
		buildChannelWriteRequest(&plan, false), &ch); err != nil {
		addChannelAPIError(&resp.Diagnostics, "create the notification channel", err)
		return
	}
	if ch.ID == "" {
		resp.Diagnostics.AddError("Malformed notification API response", "Create returned no channel id.")
		return
	}

	// The write path is an idempotent upsert keyed on natural identity, so a
	// "create" that matched an existing channel returns that channel with no
	// signing_secret. Surface it rather than silently adopting a channel
	// someone configured out-of-band, since adopting would also mean the user
	// never receives a secret they may be relying on.
	if ch.Type == channelTypeWebhook && ch.SigningSecret == nil {
		resp.Diagnostics.AddError(
			"Notification channel already exists",
			fmt.Sprintf("A %s channel with this destination already exists (id %s), so the platform "+
				"returned the existing channel instead of creating one, and issued no signing secret. "+
				"Import it instead:\n\n  terraform import <address> %s", ch.Type, ch.ID, ch.ID),
		)
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, applyChannelResponse(&ch, &plan, ch.SigningSecret))...)
}

func (r *notificationChannelResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}
	var state notificationChannelResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	ch, found := r.findChannelByID(ctx, state.ID.ValueString(), &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		// Deleted out-of-band.
		resp.State.RemoveResource(ctx)
		return
	}

	// revealed is nil: a read is never the one-time reveal, so the prior
	// signing_secret is preserved.
	resp.Diagnostics.Append(resp.State.Set(ctx, applyChannelResponse(ch, &state, nil))...)
}

func (r *notificationChannelResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}
	var plan, state notificationChannelResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Carry the server-assigned id from state; it is not in the plan when the
	// practitioner never set it.
	plan.ID = state.ID

	var ch notificationChannelResponse
	if err := doJSON(ctx, r.client, http.MethodPut, channelsPath,
		buildChannelWriteRequest(&plan, true), &ch); err != nil {
		addChannelAPIError(&resp.Diagnostics, "update the notification channel", err)
		return
	}

	// Rotate in place when the keeper map changed. Done AFTER the upsert so a
	// failed upsert does not burn a secret, and so the rotated value is the one
	// written to state.
	revealed := ch.SigningSecret
	if rotationTriggered(&plan, &state) {
		var rotated notificationWebhookSecretResponse
		if err := doJSON(ctx, r.client, http.MethodPost,
			channelIDPath(ch.ID, "/regenerate-secret"), nil, &rotated); err != nil {
			addChannelAPIError(&resp.Diagnostics, "rotate the webhook signing secret", err)
			// The upsert already landed; persist it so the next apply retries
			// only the rotation rather than re-applying everything.
			resp.Diagnostics.Append(resp.State.Set(ctx, applyChannelResponse(&ch, &state, revealed))...)
			return
		}
		revealed = &rotated.SigningSecret
	}

	// prior = state, so signing_secret falls back to the stored value when this
	// update was not a reveal.
	next := applyChannelResponse(&ch, &state, revealed)
	next.RotateWhenChanged = plan.RotateWhenChanged
	next.TeamsWorkflowURL = nullIfUnknownString(plan.TeamsWorkflowURL)
	resp.Diagnostics.Append(resp.State.Set(ctx, next)...)
}

func (r *notificationChannelResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}
	var state notificationChannelResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := doJSON(ctx, r.client, http.MethodDelete, channelIDPath(state.ID.ValueString(), ""), nil, nil)
	if err != nil && !isNotFound(err) {
		addChannelAPIError(&resp.Diagnostics, "delete the notification channel", err)
	}
}

// ImportState adopts an existing channel by id.
//
// signing_secret cannot be recovered on import — the platform reveals it once
// and this resource was not the recipient — so it stays null until the next
// rotation. has_signing_secret still reports whether one exists.
func (r *notificationChannelResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
