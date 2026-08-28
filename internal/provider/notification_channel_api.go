// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

// Wire types for the notification-service public channel API (BCP-3758),
// `/api/notification/public/v1/channels`.

// notificationChannelWriteRequest is the upsert body. Destination fields are
// pointers so only the ones belonging to the channel's type are emitted —
// sending a field that does not belong to the type is a 422, not an ignored
// field. `Subscriptions` is always emitted (never omitempty): the endpoint
// REPLACES the set, so an absent key and an empty list mean the same thing,
// and being explicit keeps that destructive default visible on the wire.
type notificationChannelWriteRequest struct {
	ID               *string                        `json:"id,omitempty"`
	Type             string                         `json:"type"`
	Enabled          bool                           `json:"enabled"`
	EmailAddress     *string                        `json:"email_address,omitempty"`
	URL              *string                        `json:"url,omitempty"`
	Label            *string                        `json:"label,omitempty"`
	SlackChannelID   *string                        `json:"slack_channel_id,omitempty"`
	TeamsWorkflowURL *string                        `json:"teams_workflow_url,omitempty"`
	Subscriptions    []notificationSubscriptionBody `json:"subscriptions"`
}

type notificationSubscriptionBody struct {
	AlertType string `json:"alert_type"`
}

// notificationChannelResponse is a channel as the API returns it. Secrets are
// never present except `SigningSecret` on the one-time reveal.
type notificationChannelResponse struct {
	ID               string                         `json:"id"`
	Type             string                         `json:"type"`
	Enabled          bool                           `json:"enabled"`
	UserID           *string                        `json:"user_id"`
	EmailAddress     *string                        `json:"email_address"`
	URL              *string                        `json:"url"`
	Label            *string                        `json:"label"`
	SlackChannelID   *string                        `json:"slack_channel_id"`
	Subscriptions    []notificationSubscriptionBody `json:"subscriptions"`
	CreatedAt        string                         `json:"created_at"`
	UpdatedAt        string                         `json:"updated_at"`
	HasSigningSecret bool                           `json:"has_signing_secret"`
	HasWorkflowURL   bool                           `json:"has_workflow_url"`
	// SigningSecret is the one-time reveal: non-nil only on the response to the
	// request that generated it (webhook create, or regenerate-secret).
	SigningSecret *string `json:"signing_secret"`
}

// notificationChannelListResponse is the envelope the list endpoints return.
type notificationChannelListResponse struct {
	Data []notificationChannelResponse `json:"data"`
}

// notificationWebhookSecretResponse is the rotate endpoint's one-time reveal.
type notificationWebhookSecretResponse struct {
	SigningSecret string `json:"signing_secret"`
}
