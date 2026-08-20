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
	"time"
)

// --- in-process fake system-management-service --------------------------------
//
// fakeExportServer emulates the SMS public export surface the provider binds
// (`/api/system-management/public/v1/exports/{org}/{type}` plus
// `/destination`, `/destination/aws-trust-info`, `/settings`, `/start`,
// `/pause`) faithfully enough to drive real plan/apply cycles through the
// terraform-plugin-testing harness.
//
// It exists for one thing the pure unit tests structurally cannot prove: that a
// full plan → apply → re-plan cycle is clean. "Provider produced inconsistent
// result after apply" is raised by Terraform core when the applied value
// differs from the planned one, so only a real apply driven by real Terraform
// can catch it. The mapping-level tests assert the values; this asserts core
// accepts them.
//
// The behaviours modelled from the BCP-3714 contract (bdai-platform #7042,
// #7043) are the ones the provider can get wrong:
//
//   - azure_blob rejects region, use_ssl: true, use_path_style: true,
//     iam_role_arn, access_key_id and secret_access_key — checked on key
//     PRESENCE in the request body, which is what makes a stray serialized
//     `"use_ssl": true` a 400 here exactly as in production;
//   - s3 rejects account_key and sas_token;
//   - the wrong secret for the declared auth_method is rejected;
//   - a secret is required on first configure, and may be omitted on
//     reconfigure only when provider and auth_method are unchanged;
//   - an azure row reads back with empty region/use_ssl/use_path_style/
//     iam_role_arn/external_id;
//   - aws-trust-info answers 400 for an azure destination.
type fakeExportServer struct {
	mu         sync.Mutex
	enabled    bool
	dest       *fakeExportDestination
	settings   fakeExportSettings
	externalID string

	// putBodies records every destination PUT body, so a test can assert on
	// what actually went over the wire.
	putBodies []map[string]json.RawMessage
}

type fakeExportDestination struct {
	Provider     string
	Endpoint     string
	Region       string
	Bucket       string
	PathPrefix   string
	UseSSL       bool
	UsePathStyle bool
	AuthMethod   string
	IAMRoleArn   string
	Secret       string
}

type fakeExportSettings struct {
	BatchSize            int64
	FlushIntervalSeconds int64
	MaxRetries           int64
	IncludedEventTypes   []string
}

// fakeExportOrgID matches the BARNDOOR_ORGANIZATION_ID set by setupExportTest.
const fakeExportOrgID = "org-123"

// fakeExportExternalID is the sts:ExternalId the fake mints for an iam_role
// destination. Production mints it lazily on the first aws-trust-info read; the
// fake also mints it when an iam_role destination is configured, so a test can
// get a known external_id into state without depending on the relative order of
// a data-source read and a resource apply within one config. Both paths produce
// the same stable value, which is the property the provider depends on.
const fakeExportExternalID = "ext-abc123"

const fakeExportPrincipalARN = "arn:aws:iam::111122223333:role/barndoor-export"

func newFakeExportServer() *fakeExportServer {
	return &fakeExportServer{
		settings: fakeExportSettings{BatchSize: 100, FlushIntervalSeconds: 30, MaxRetries: 3},
	}
}

func setupExportTest(t *testing.T) *fakeExportServer {
	t.Helper()

	fake := newFakeExportServer()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	t.Setenv("BARNDOOR_BASE_URL", srv.URL)
	t.Setenv("BARNDOOR_TOKEN_URL", srv.URL+"/token")
	t.Setenv("BARNDOOR_CLIENT_ID", "test-client")
	t.Setenv("BARNDOOR_CLIENT_SECRET", "test-secret")
	t.Setenv("BARNDOOR_ORGANIZATION_ID", fakeExportOrgID)

	return fake
}

func (f *fakeExportServer) handler() http.HandlerFunc {
	base := "/" + smsAPIPrefix + "/exports/" + fakeExportOrgID + "/" + defaultExportType

	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			writeToken(w)
			return
		}

		switch strings.TrimPrefix(r.URL.Path, base) {
		case "":
			if r.Method != http.MethodGet {
				writeExportError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			f.writeExport(w)
		case "/destination":
			f.handleDestination(w, r)
		case "/destination/aws-trust-info":
			f.handleTrustInfo(w, r)
		case "/settings":
			f.handleSettings(w, r)
		case "/start", "/pause":
			f.handleAction(w, r, strings.HasSuffix(r.URL.Path, "/start"))
		default:
			writeExportError(w, http.StatusNotFound, "export not found")
		}
	}
}

func (f *fakeExportServer) handleDestination(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.Method {
	case http.MethodGet:
		f.writeDestination(w)
	case http.MethodDelete:
		f.dest = nil
		f.enabled = false
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPut:
		f.configureDestination(w, r)
	default:
		writeExportError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// configureDestination applies the PUT contract. Every rejection below is one
// the real API performs; getting any of them wrong in the provider shows up
// here as a failed apply rather than a silently-wrong request.
func (f *fakeExportServer) configureDestination(w http.ResponseWriter, r *http.Request) {
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeExportError(w, http.StatusBadRequest, "malformed body: "+err.Error())
		return
	}
	f.putBodies = append(f.putBodies, raw)

	provider := jsonString(raw, "provider")
	if provider == "" {
		provider = storageProviderS3
	}
	if provider != storageProviderS3 && provider != storageProviderAzureBlob {
		writeExportError(w, http.StatusBadRequest, "unsupported provider "+provider)
		return
	}

	endpoint, bucket := jsonString(raw, "endpoint"), jsonString(raw, "bucket")
	if endpoint == "" || bucket == "" {
		writeExportError(w, http.StatusBadRequest, "endpoint and bucket are required")
		return
	}

	authMethod := jsonString(raw, "auth_method")
	next := &fakeExportDestination{
		Provider:   provider,
		Endpoint:   endpoint,
		Bucket:     bucket,
		PathPrefix: jsonString(raw, "path_prefix"),
		AuthMethod: authMethod,
	}

	if provider == storageProviderAzureBlob {
		// S3-only attributes are rejected on presence, matching production.
		// use_ssl/use_path_style are rejected only when true (the API tolerates
		// an explicit false), which is precisely the trap: use_ssl carries a
		// schema Default of true, so a provider that serialized the shared S3
		// body would land here with a 400 on every apply.
		for _, key := range []string{"region", "iam_role_arn", "access_key_id", "secret_access_key"} {
			if jsonString(raw, key) != "" {
				writeExportError(w, http.StatusBadRequest,
					fmt.Sprintf("%s is not valid for an azure_blob destination", key))
				return
			}
		}
		for _, key := range []string{"use_ssl", "use_path_style"} {
			if jsonBool(raw, key) {
				writeExportError(w, http.StatusBadRequest,
					fmt.Sprintf("%s: true is not valid for an azure_blob destination", key))
				return
			}
		}
		if authMethod != authMethodAccountKey && authMethod != authMethodSASToken {
			writeExportError(w, http.StatusBadRequest,
				"auth_method must be account_key or sas_token for an azure_blob destination")
			return
		}
		wrong, want := "sas_token", "account_key"
		if authMethod == authMethodSASToken {
			wrong, want = "account_key", "sas_token"
		}
		if jsonString(raw, wrong) != "" {
			writeExportError(w, http.StatusBadRequest,
				fmt.Sprintf("%s is not valid when auth_method is %s", wrong, authMethod))
			return
		}
		// A leading '?' on a SAS token is tolerated and normalized away.
		next.Secret = strings.TrimPrefix(jsonString(raw, want), "?")
	} else {
		for _, key := range []string{"account_key", "sas_token"} {
			if jsonString(raw, key) != "" {
				writeExportError(w, http.StatusBadRequest,
					fmt.Sprintf("%s is not valid for an s3 destination", key))
				return
			}
		}
		if authMethod == "" {
			authMethod = authMethodAccessKeys
			next.AuthMethod = authMethod
		}
		next.Region = jsonString(raw, "region")
		next.UseSSL = jsonBool(raw, "use_ssl")
		next.UsePathStyle = jsonBool(raw, "use_path_style")
		switch authMethod {
		case authMethodAccessKeys:
			if jsonString(raw, "iam_role_arn") != "" {
				writeExportError(w, http.StatusBadRequest, "iam_role_arn is not valid when auth_method is access_keys")
				return
			}
			next.Secret = jsonString(raw, "secret_access_key")
		case authMethodIAMRole:
			next.IAMRoleArn = jsonString(raw, "iam_role_arn")
			if next.IAMRoleArn == "" {
				writeExportError(w, http.StatusBadRequest, "iam_role_arn is required when auth_method is iam_role")
				return
			}
			// iam_role carries no stored secret; the trust relationship is the
			// credential. Mint the external ID now (see fakeExportExternalID).
			next.Secret = "n/a"
			f.externalID = fakeExportExternalID
		default:
			writeExportError(w, http.StatusBadRequest,
				"auth_method must be access_keys or iam_role for an s3 destination")
			return
		}
	}

	// The secret is required on first configure, and may be omitted on a
	// reconfigure only when provider and auth_method are unchanged.
	if next.Secret == "" {
		unchanged := f.dest != nil && f.dest.Provider == next.Provider && f.dest.AuthMethod == next.AuthMethod
		if !unchanged {
			writeExportError(w, http.StatusBadRequest,
				"a credential is required when configuring a destination or changing its provider or auth_method")
			return
		}
		next.Secret = f.dest.Secret
	}

	// Switching away from iam_role drops the minted external ID.
	if next.Provider != storageProviderS3 || next.AuthMethod != authMethodIAMRole {
		f.externalID = ""
	}

	f.dest = next
	f.writeDestinationResponse(w)
}

func (f *fakeExportServer) handleTrustInfo(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.Method != http.MethodGet {
		writeExportError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if f.dest == nil {
		writeExportError(w, http.StatusNotFound, "export destination not found")
		return
	}
	if f.dest.Provider == storageProviderAzureBlob {
		writeExportError(w, http.StatusBadRequest,
			"aws trust info is not applicable to an azure_blob destination")
		return
	}
	if f.externalID == "" {
		f.externalID = fakeExportExternalID
	}
	writeExportJSON(w, http.StatusOK, map[string]any{
		"principal_arn": fakeExportPrincipalARN,
		"external_id":   f.externalID,
	})
}

func (f *fakeExportServer) handleSettings(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.Method != http.MethodPatch {
		writeExportError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		BatchSize            *int64    `json:"batch_size"`
		FlushIntervalSeconds *int64    `json:"flush_interval_seconds"`
		MaxRetries           *int64    `json:"max_retries"`
		IncludedEventTypes   *[]string `json:"included_event_types"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeExportError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.BatchSize != nil {
		f.settings.BatchSize = *body.BatchSize
	}
	if body.FlushIntervalSeconds != nil {
		f.settings.FlushIntervalSeconds = *body.FlushIntervalSeconds
	}
	if body.MaxRetries != nil {
		f.settings.MaxRetries = *body.MaxRetries
	}
	if body.IncludedEventTypes != nil {
		f.settings.IncludedEventTypes = *body.IncludedEventTypes
	}
	f.writeExport(w)
}

func (f *fakeExportServer) handleAction(w http.ResponseWriter, r *http.Request, start bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.Method != http.MethodPost {
		writeExportError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if start && f.dest == nil {
		writeExportError(w, http.StatusBadRequest, "cannot start an export with no destination")
		return
	}
	f.enabled = start
	f.writeExport(w)
}

// destinationJSON renders the stored destination the way the API does. An
// azure row reports empty region/use_ssl/use_path_style/iam_role_arn/
// external_id — the shape the provider's state mapping has to absorb without
// producing a diff.
func (f *fakeExportServer) destinationJSON() map[string]any {
	if f.dest == nil {
		return map[string]any{}
	}
	out := map[string]any{
		"provider":        f.dest.Provider,
		"endpoint":        f.dest.Endpoint,
		"bucket":          f.dest.Bucket,
		"auth_method":     f.dest.AuthMethod,
		"has_credentials": f.dest.Secret != "",
		// Present on every real response and deliberately absent from the
		// provider's schema: the decoder must ignore it.
		"credentials_rotated_at": time.Now().UTC().Format(time.RFC3339),
	}
	if f.dest.PathPrefix != "" {
		out["path_prefix"] = f.dest.PathPrefix
	}
	if f.dest.Provider == storageProviderS3 {
		out["region"] = f.dest.Region
		out["use_ssl"] = f.dest.UseSSL
		out["use_path_style"] = f.dest.UsePathStyle
		if f.dest.IAMRoleArn != "" {
			out["iam_role_arn"] = f.dest.IAMRoleArn
		}
		if f.externalID != "" {
			out["external_id"] = f.externalID
		}
	}
	return out
}

func (f *fakeExportServer) writeExport(w http.ResponseWriter) {
	writeExportJSON(w, http.StatusOK, map[string]any{
		"organization_id": fakeExportOrgID,
		"export_type":     defaultExportType,
		"enabled":         f.enabled,
		"settings": map[string]any{
			"batch_size":             f.settings.BatchSize,
			"flush_interval_seconds": f.settings.FlushIntervalSeconds,
			"max_retries":            f.settings.MaxRetries,
			"included_event_types":   f.settings.IncludedEventTypes,
		},
		"destination": f.destinationJSON(),
	})
}

func (f *fakeExportServer) writeDestination(w http.ResponseWriter) {
	f.writeDestinationResponse(w)
}

func (f *fakeExportServer) writeDestinationResponse(w http.ResponseWriter) {
	writeExportJSON(w, http.StatusOK, map[string]any{
		"organization_id": fakeExportOrgID,
		"export_type":     defaultExportType,
		"enabled":         f.enabled,
		"destination":     f.destinationJSON(),
	})
}

// currentDestination exposes the stored destination for assertions.
func (f *fakeExportServer) currentDestination() *fakeExportDestination {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dest == nil {
		return nil
	}
	cp := *f.dest
	return &cp
}

// lastRecordedPutBody returns the most recent destination PUT body, for
// asserting on what the provider actually serialized. It returns nil when no
// PUT has been made, so callers report that as their own failure.
func (f *fakeExportServer) lastRecordedPutBody() map[string]json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.putBodies) == 0 {
		return nil
	}
	return f.putBodies[len(f.putBodies)-1]
}

func jsonString(raw map[string]json.RawMessage, key string) string {
	v, ok := raw[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return ""
	}
	return s
}

func jsonBool(raw map[string]json.RawMessage, key string) bool {
	v, ok := raw[key]
	if !ok {
		return false
	}
	var b bool
	if err := json.Unmarshal(v, &b); err != nil {
		return false
	}
	return b
}

func writeExportJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeExportError(w http.ResponseWriter, status int, message string) {
	writeExportJSON(w, status, map[string]string{"detail": message})
}
