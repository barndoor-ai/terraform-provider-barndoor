// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// --- in-process fake notification-service ------------------------------------
//
// fakeChannelServer emulates the notification-service public channel surface
// (`/api/notification/public/v1/channels` plus `/{id}` and
// `/{id}/regenerate-secret`) faithfully enough to drive real plan/apply cycles
// through the terraform-plugin-testing harness.
//
// It exists for the things the pure mapping tests structurally cannot prove:
//
//   - that a full plan → apply → re-plan cycle is CLEAN. "Provider produced
//     inconsistent result after apply" is raised by Terraform core when the
//     applied value differs from the planned one, and only a real apply driven
//     by real Terraform can catch it. `signing_secret` is the hazard: it is
//     Computed, never refreshable, and ModifyPlan pins it from state — exactly
//     the shape that produces that error when it is subtly wrong.
//   - that a rotation is an in-place UPDATE (not a replacement) AND changes the
//     secret in state.
//   - that a non-rotating update leaves the secret untouched with no diff.
//
// Behaviours modelled from the BCP-3758 contract, chosen as the ones the
// provider can get wrong:
//
//   - the write path is an idempotent UPSERT on natural identity (webhook by
//     url), so a create against an existing url returns the existing channel
//     with NO signing_secret;
//   - signing_secret is revealed exactly once, on the create that generated it
//     and on regenerate-secret — never on a list read;
//   - subscriptions are REPLACED, not merged;
//   - a field that does not belong to the channel's type is a 422 (checked on
//     key presence in the request body, which is what makes a stray serialized
//     field fail here exactly as in production);
//   - DELETE answers 204 with no body.
type fakeChannelServer struct {
	mu       sync.Mutex
	channels map[string]*notificationChannelResponse
	nextID   int
	secretN  int

	// putBodies records every upsert body so a test can assert what actually
	// went over the wire.
	putBodies []map[string]json.RawMessage
	// rotateCalls counts regenerate-secret calls, so a test can prove a
	// rotation happened (or did not).
	rotateCalls int
}

const fakeChannelOrgID = "11111111-1111-1111-1111-111111111111"

func newFakeChannelServer() *fakeChannelServer {
	return &fakeChannelServer{channels: map[string]*notificationChannelResponse{}}
}

func setupChannelTest(t *testing.T) *fakeChannelServer {
	t.Helper()

	fake := newFakeChannelServer()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	t.Setenv("BARNDOOR_BASE_URL", srv.URL)
	t.Setenv("BARNDOOR_TOKEN_URL", srv.URL+"/token")
	t.Setenv("BARNDOOR_CLIENT_ID", "test-client")
	t.Setenv("BARNDOOR_CLIENT_SECRET", "test-secret")
	t.Setenv("BARNDOOR_ORGANIZATION_ID", fakeChannelOrgID)

	return fake
}

// permittedChannelFields mirrors the API's polymorphic identity rules. Keyed on
// PRESENCE in the request body, so a stray serialized field is a 422 here just
// as it is in production.
var permittedChannelFields = map[string]map[string]bool{
	channelTypeEmail:   {"email_address": true},
	channelTypeWebhook: {"url": true},
	channelTypeSlack:   {"slack_channel_id": true, "label": true},
	channelTypeTeams:   {"label": true, "teams_workflow_url": true},
}

var allChannelDestinationFields = []string{
	"email_address", "url", "label", "slack_channel_id", "teams_workflow_url",
}

func (f *fakeChannelServer) nextSecret() string {
	f.secretN++
	return fmt.Sprintf("whsec_fake%d", f.secretN)
}

// naturalIdentity returns the dedup key for a channel type, mirroring the API.
func naturalIdentity(chType string, body map[string]json.RawMessage) string {
	var key string
	switch chType {
	case channelTypeEmail:
		key = string(body["email_address"])
	case channelTypeWebhook:
		key = string(body["url"])
	case channelTypeSlack:
		key = string(body["slack_channel_id"])
	default:
		// teams has no non-secret natural identity; treat every create as new.
		return ""
	}
	return chType + "|" + key
}

func (f *fakeChannelServer) handler() http.HandlerFunc {
	base := "/" + notificationAPIPrefix + "/channels"

	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			writeToken(w)
			return
		}

		f.mu.Lock()
		defer f.mu.Unlock()

		rest := strings.TrimPrefix(r.URL.Path, base)

		switch {
		case rest == "" && r.Method == http.MethodPut:
			f.upsert(w, r)
		case rest == "" && r.Method == http.MethodGet:
			f.list(w)
		case strings.HasSuffix(rest, "/regenerate-secret") && r.Method == http.MethodPost:
			id := strings.TrimSuffix(strings.TrimPrefix(rest, "/"), "/regenerate-secret")
			f.rotate(w, id)
		case rest != "" && r.Method == http.MethodDelete:
			f.remove(w, strings.TrimPrefix(rest, "/"))
		default:
			http.NotFound(w, r)
		}
	}
}

func (f *fakeChannelServer) upsert(w http.ResponseWriter, r *http.Request) {
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	f.putBodies = append(f.putBodies, body)

	var chType string
	_ = json.Unmarshal(body["type"], &chType)
	permitted, ok := permittedChannelFields[chType]
	if !ok {
		http.Error(w, fmt.Sprintf("unsupported channel type %q", chType), http.StatusUnprocessableEntity)
		return
	}
	// Reject a field that does not belong to this type, by presence.
	for _, field := range allChannelDestinationFields {
		if _, present := body[field]; present && !permitted[field] {
			http.Error(w, fmt.Sprintf("%s channel takes no %s", chType, field), http.StatusUnprocessableEntity)
			return
		}
	}
	if _, present := body["subscriptions"]; !present {
		http.Error(w, "subscriptions must be sent explicitly", http.StatusUnprocessableEntity)
		return
	}

	var enabled bool
	_ = json.Unmarshal(body["enabled"], &enabled)

	var subs []notificationSubscriptionBody
	_ = json.Unmarshal(body["subscriptions"], &subs)

	// Edit-by-id when an id is supplied; 404 when unknown.
	if raw, present := body["id"]; present {
		var id string
		_ = json.Unmarshal(raw, &id)
		ch, found := f.channels[id]
		if !found {
			http.Error(w, "channel not found", http.StatusNotFound)
			return
		}
		ch.Enabled = enabled
		ch.Subscriptions = subs
		applyDestinationFields(ch, body)
		// An edit is never a reveal.
		f.writeChannel(w, ch, nil)
		return
	}

	// Create-or-dedup on natural identity.
	if key := naturalIdentity(chType, body); key != "" {
		for _, ch := range f.channels {
			if naturalIdentityOf(ch) == key {
				ch.Enabled = enabled
				ch.Subscriptions = subs
				// Dedup path: existing channel, NO secret reveal.
				f.writeChannel(w, ch, nil)
				return
			}
		}
	}

	f.nextID++
	ch := &notificationChannelResponse{
		ID:            fmt.Sprintf("chan-%d", f.nextID),
		Type:          chType,
		Enabled:       enabled,
		Subscriptions: subs,
		CreatedAt:     "2026-08-28T00:00:00Z",
		UpdatedAt:     "2026-08-28T00:00:00Z",
	}
	applyDestinationFields(ch, body)
	f.channels[ch.ID] = ch

	var revealed *string
	if chType == channelTypeWebhook {
		s := f.nextSecret()
		ch.HasSigningSecret = true
		revealed = &s
	}
	if chType == channelTypeTeams {
		if _, present := body["teams_workflow_url"]; present {
			ch.HasWorkflowURL = true
		}
	}
	f.writeChannel(w, ch, revealed)
}

func applyDestinationFields(ch *notificationChannelResponse, body map[string]json.RawMessage) {
	assign := func(field string, dst **string) {
		raw, present := body[field]
		if !present {
			return
		}
		var v string
		_ = json.Unmarshal(raw, &v)
		*dst = &v
	}
	assign("email_address", &ch.EmailAddress)
	assign("url", &ch.URL)
	assign("label", &ch.Label)
	assign("slack_channel_id", &ch.SlackChannelID)
}

func naturalIdentityOf(ch *notificationChannelResponse) string {
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return fmt.Sprintf("%q", *p)
	}
	switch ch.Type {
	case channelTypeEmail:
		return ch.Type + "|" + deref(ch.EmailAddress)
	case channelTypeWebhook:
		return ch.Type + "|" + deref(ch.URL)
	case channelTypeSlack:
		return ch.Type + "|" + deref(ch.SlackChannelID)
	}
	return ""
}

// writeChannel serializes a channel, including signing_secret ONLY when this
// response is the one-time reveal.
func (f *fakeChannelServer) writeChannel(w http.ResponseWriter, ch *notificationChannelResponse, revealed *string) {
	out := *ch
	out.SigningSecret = revealed
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(&out)
}

func (f *fakeChannelServer) list(w http.ResponseWriter) {
	resp := notificationChannelListResponse{Data: make([]notificationChannelResponse, 0, len(f.channels))}
	for _, ch := range f.channels {
		out := *ch
		// A list read is NEVER the reveal.
		out.SigningSecret = nil
		resp.Data = append(resp.Data, out)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(&resp)
}

func (f *fakeChannelServer) rotate(w http.ResponseWriter, id string) {
	ch, found := f.channels[id]
	if !found {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	if ch.Type != channelTypeWebhook {
		http.Error(w, "only webhook channels have a signing secret", http.StatusUnprocessableEntity)
		return
	}
	f.rotateCalls++
	secret := f.nextSecret()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(notificationWebhookSecretResponse{SigningSecret: secret})
}

func (f *fakeChannelServer) remove(w http.ResponseWriter, id string) {
	if _, found := f.channels[id]; !found {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	delete(f.channels, id)
	// 204 with no body, matching the API.
	w.WriteHeader(http.StatusNoContent)
}

// lastPutBody returns the most recent upsert body, for wire-level assertions.
func (f *fakeChannelServer) lastPutBody(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.putBodies) == 0 {
		t.Fatal("no upsert body recorded")
	}
	return f.putBodies[len(f.putBodies)-1]
}

func (f *fakeChannelServer) rotations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rotateCalls
}
