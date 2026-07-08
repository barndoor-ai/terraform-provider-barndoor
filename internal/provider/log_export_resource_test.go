// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

func newLogExportSchema(t *testing.T) schema.Schema {
	t.Helper()
	var resp resource.SchemaResponse
	NewLogExportResource().Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}
	return resp.Schema
}

// TestLogExportImportState_SeedsSettings guards the import path: ImportState
// must seed a non-nil settings block so the follow-up Read overlays the
// server-computed batch_size/flush_interval_seconds/max_retries. Without the
// seed, mapServerToState's non-nil guard skips settings and ImportStateVerify
// fails on the dropped attributes.
func TestLogExportImportState_SeedsSettings(t *testing.T) {
	ctx := context.Background()
	schemaObj := newLogExportSchema(t)
	resp := &resource.ImportStateResponse{
		State: tfsdk.State{
			Schema: schemaObj,
			Raw:    tftypes.NewValue(schemaObj.Type().TerraformType(ctx), nil),
		},
	}

	(&logExportResource{}).ImportState(ctx, resource.ImportStateRequest{ID: "org-123/audit-log"}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("ImportState diagnostics: %+v", resp.Diagnostics)
	}

	var state logExportResourceModel
	if diags := resp.State.Get(ctx, &state); diags.HasError() {
		t.Fatalf("state.Get: %+v", diags)
	}

	if got := state.OrganizationID.ValueString(); got != "org-123" {
		t.Errorf("organization_id = %q, want org-123", got)
	}
	if got := state.ExportType.ValueString(); got != "audit-log" {
		t.Errorf("export_type = %q, want audit-log", got)
	}
	if state.Settings == nil {
		t.Fatal("settings not seeded on import: Read will not hydrate server-computed values")
	}
	if !state.Settings.BatchSize.IsNull() {
		t.Errorf("seeded batch_size = %v, want null (Read overlays the real value)", state.Settings.BatchSize)
	}
}

func TestLogExportResource_Metadata(t *testing.T) {
	var resp resource.MetadataResponse
	NewLogExportResource().Metadata(context.Background(), resource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_log_export"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestLogExportResource_Schema(t *testing.T) {
	s := newLogExportSchema(t)

	for _, attr := range []string{"organization_id", "export_type", "enabled", "destination", "settings"} {
		if _, ok := s.Attributes[attr]; !ok {
			t.Errorf("schema missing top-level attribute %q", attr)
		}
	}

	dest, ok := s.Attributes["destination"].(schema.SingleNestedAttribute)
	if !ok {
		t.Fatalf("destination is %T, want schema.SingleNestedAttribute", s.Attributes["destination"])
	}
	if !dest.IsRequired() {
		t.Error("destination should be required")
	}

	for _, attr := range []string{
		"endpoint", "region", "bucket", "path_prefix", "use_ssl", "use_path_style",
		"auth_method", "iam_role_arn", "access_key_id", "secret_access_key",
		"external_id", "has_credentials",
	} {
		if _, ok := dest.Attributes[attr]; !ok {
			t.Errorf("destination missing attribute %q", attr)
		}
	}

	if secret := dest.Attributes["secret_access_key"]; !secret.IsSensitive() {
		t.Error("destination.secret_access_key must be Sensitive")
	}
	for _, computed := range []string{"external_id", "has_credentials"} {
		if !dest.Attributes[computed].IsComputed() {
			t.Errorf("destination.%s should be Computed", computed)
		}
	}
	for _, req := range []string{"endpoint", "bucket"} {
		if !dest.Attributes[req].IsRequired() {
			t.Errorf("destination.%s should be Required", req)
		}
	}

	settings, ok := s.Attributes["settings"].(schema.SingleNestedAttribute)
	if !ok {
		t.Fatalf("settings is %T, want schema.SingleNestedAttribute", s.Attributes["settings"])
	}
	for _, attr := range []string{"batch_size", "flush_interval_seconds", "max_retries", "included_event_types"} {
		if _, ok := settings.Attributes[attr]; !ok {
			t.Errorf("settings missing attribute %q", attr)
		}
	}
}

func TestBuildDestinationRequest_AccessKeys(t *testing.T) {
	d := &destinationModel{
		Endpoint:        types.StringValue("https://s3.us-east-1.amazonaws.com"),
		Region:          types.StringValue("us-east-1"),
		Bucket:          types.StringValue("audit-logs"),
		PathPrefix:      types.StringValue("acme/"),
		UseSSL:          types.BoolValue(true),
		UsePathStyle:    types.BoolValue(false),
		AuthMethod:      types.StringValue(authMethodAccessKeys),
		IAMRoleArn:      types.StringNull(),
		AccessKeyID:     types.StringValue("AKIA..."),
		SecretAccessKey: types.StringValue("secret"),
	}

	req := buildDestinationRequest(d)

	if req.AuthMethod != authMethodAccessKeys {
		t.Errorf("auth_method = %q, want %q", req.AuthMethod, authMethodAccessKeys)
	}
	if req.AccessKeyID != "AKIA..." || req.SecretAccessKey != "secret" {
		t.Errorf("access keys not propagated: %+v", req)
	}
	if req.IAMRoleArn != "" {
		t.Errorf("iam_role_arn = %q, want empty for access_keys", req.IAMRoleArn)
	}
	if !req.UseSSL || req.UsePathStyle {
		t.Errorf("ssl/path-style flags = (%v,%v), want (true,false)", req.UseSSL, req.UsePathStyle)
	}
}

func TestBuildDestinationRequest_IAMRole(t *testing.T) {
	d := &destinationModel{
		Endpoint:        types.StringValue("https://s3.us-east-1.amazonaws.com"),
		Region:          types.StringValue("us-east-1"),
		Bucket:          types.StringValue("audit-logs"),
		AuthMethod:      types.StringValue(authMethodIAMRole),
		IAMRoleArn:      types.StringValue("arn:aws:iam::123456789012:role/barndoor"),
		AccessKeyID:     types.StringNull(),
		SecretAccessKey: types.StringNull(),
	}

	req := buildDestinationRequest(d)

	if req.AuthMethod != authMethodIAMRole {
		t.Errorf("auth_method = %q, want %q", req.AuthMethod, authMethodIAMRole)
	}
	if req.IAMRoleArn != "arn:aws:iam::123456789012:role/barndoor" {
		t.Errorf("iam_role_arn not propagated: %q", req.IAMRoleArn)
	}
	if req.AccessKeyID != "" || req.SecretAccessKey != "" {
		t.Errorf("access keys must be empty for iam_role: %+v", req)
	}
}

func TestValidateDestinationConfig(t *testing.T) {
	tests := map[string]struct {
		dest      *destinationModel
		wantError bool
	}{
		"access_keys with keys": {
			dest: &destinationModel{
				AuthMethod:      types.StringValue(authMethodAccessKeys),
				AccessKeyID:     types.StringValue("AKIA..."),
				SecretAccessKey: types.StringValue("secret"),
				IAMRoleArn:      types.StringNull(),
			},
		},
		"access_keys missing secret": {
			dest: &destinationModel{
				AuthMethod:      types.StringValue(authMethodAccessKeys),
				AccessKeyID:     types.StringValue("AKIA..."),
				SecretAccessKey: types.StringNull(),
				IAMRoleArn:      types.StringNull(),
			},
			wantError: true,
		},
		"iam_role with arn": {
			dest: &destinationModel{
				AuthMethod:      types.StringValue(authMethodIAMRole),
				IAMRoleArn:      types.StringValue("arn:aws:iam::123:role/x"),
				AccessKeyID:     types.StringNull(),
				SecretAccessKey: types.StringNull(),
			},
		},
		"iam_role with stray access key": {
			dest: &destinationModel{
				AuthMethod:      types.StringValue(authMethodIAMRole),
				IAMRoleArn:      types.StringValue("arn:aws:iam::123:role/x"),
				AccessKeyID:     types.StringValue("AKIA..."),
				SecretAccessKey: types.StringNull(),
			},
			wantError: true,
		},
		"bad auth_method": {
			dest: &destinationModel{
				AuthMethod:      types.StringValue("token"),
				AccessKeyID:     types.StringNull(),
				SecretAccessKey: types.StringNull(),
				IAMRoleArn:      types.StringNull(),
			},
			wantError: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var diags diag.Diagnostics
			validateDestinationConfig(tc.dest, &diags)
			if got := diags.HasError(); got != tc.wantError {
				t.Errorf("HasError() = %v, want %v (diags: %+v)", got, tc.wantError, diags)
			}
		})
	}
}

func TestResolvedAuthMethod(t *testing.T) {
	if got := resolvedAuthMethod(""); got != authMethodAccessKeys {
		t.Errorf("resolvedAuthMethod(\"\") = %q, want %q", got, authMethodAccessKeys)
	}
	if got := resolvedAuthMethod(authMethodIAMRole); got != authMethodIAMRole {
		t.Errorf("resolvedAuthMethod(iam_role) = %q, want %q", got, authMethodIAMRole)
	}
}

func TestValidateDestinationStrings(t *testing.T) {
	tests := map[string]struct {
		dest      *destinationModel
		wantError bool
	}{
		"clean values": {
			dest: &destinationModel{
				Endpoint: types.StringValue("https://s3"),
				Bucket:   types.StringValue("b"),
				Region:   types.StringValue("us-east-1"),
			},
		},
		"trailing whitespace in bucket": {
			dest: &destinationModel{
				Endpoint: types.StringValue("https://s3"),
				Bucket:   types.StringValue("b "),
			},
			wantError: true,
		},
		"leading whitespace in endpoint": {
			dest: &destinationModel{
				Endpoint: types.StringValue(" https://s3"),
				Bucket:   types.StringValue("b"),
			},
			wantError: true,
		},
		"empty optional region": {
			dest: &destinationModel{
				Endpoint: types.StringValue("https://s3"),
				Bucket:   types.StringValue("b"),
				Region:   types.StringValue(""),
			},
			wantError: true,
		},
		"null and unknown are skipped": {
			dest: &destinationModel{
				Endpoint:   types.StringValue("https://s3"),
				Bucket:     types.StringValue("b"),
				Region:     types.StringNull(),
				PathPrefix: types.StringUnknown(),
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var diags diag.Diagnostics
			validateDestinationStrings(tc.dest, &diags)
			if got := diags.HasError(); got != tc.wantError {
				t.Errorf("HasError() = %v, want %v (diags: %+v)", got, tc.wantError, diags)
			}
		})
	}
}

func TestValidateSettingsConfig(t *testing.T) {
	ok := &settingsModel{BatchSize: types.Int64Value(1), FlushIntervalSeconds: types.Int64Value(30), MaxRetries: types.Int64Value(3)}
	var diags diag.Diagnostics
	validateSettingsConfig(ok, &diags)
	if diags.HasError() {
		t.Errorf("valid settings flagged: %+v", diags)
	}

	bad := &settingsModel{BatchSize: types.Int64Value(0)}
	diags = diag.Diagnostics{}
	validateSettingsConfig(bad, &diags)
	if !diags.HasError() {
		t.Error("batch_size 0 should be rejected (API coerces 0 to its default)")
	}
}

func TestMapServerToState_AccessKeys(t *testing.T) {
	ctx := context.Background()
	dto := destinationDTO{
		Endpoint: "https://s3", Bucket: "b", Region: "us-east-1",
		AuthMethod: "access_keys", UseSSL: true, HasCredentials: true,
	}
	state := &logExportResourceModel{
		Destination: &destinationModel{
			AccessKeyID:     types.StringValue("AKIA"),
			SecretAccessKey: types.StringValue("secret"),
		},
		Settings: &settingsModel{IncludedEventTypes: types.ListNull(types.StringType)},
	}
	exp := &exportResponse{
		ExportType:  "datadog-json",
		Enabled:     true,
		Settings:    settingsDTO{BatchSize: 100, FlushIntervalSeconds: 30, MaxRetries: 3},
		Destination: dto,
	}
	if err := mapServerToState(ctx, state, "org-123", exp, &destinationResponse{Enabled: true, Destination: dto}); err != nil {
		t.Fatalf("mapServerToState: %v", err)
	}

	if state.OrganizationID.ValueString() != "org-123" {
		t.Errorf("organization_id = %q", state.OrganizationID.ValueString())
	}
	if !state.Enabled.ValueBool() {
		t.Error("enabled should be true")
	}
	d := state.Destination
	if d == nil {
		// The explicit return keeps staticcheck's SA5011 nil-deref analysis
		// happy even when the t.Fatal noreturn fact is unavailable (seen on
		// CI's cached-analysis runs).
		t.Fatal("destination should not be nil")
		return
	}
	if d.Endpoint.ValueString() != "https://s3" || d.AuthMethod.ValueString() != "access_keys" {
		t.Errorf("destination mismapped: %+v", d)
	}
	if !d.HasCredentials.ValueBool() {
		t.Error("has_credentials should be true")
	}
	if !d.ExternalID.IsNull() {
		t.Errorf("external_id should be null for access_keys, got %v", d.ExternalID)
	}
	if d.AccessKeyID.ValueString() != "AKIA" || d.SecretAccessKey.ValueString() != "secret" {
		t.Error("access keys must be carried from the prior model, not the server")
	}
	if state.Settings.BatchSize.ValueInt64() != 100 {
		t.Errorf("batch_size = %d, want 100", state.Settings.BatchSize.ValueInt64())
	}
}

func TestMapServerToState_IAMRole(t *testing.T) {
	ctx := context.Background()
	dto := destinationDTO{
		Endpoint: "https://s3", Bucket: "b", AuthMethod: "iam_role",
		IAMRoleArn: "arn:aws:iam::123:role/x", ExternalID: "ext-123",
	}
	state := &logExportResourceModel{Destination: &destinationModel{}}
	if err := mapServerToState(ctx, state, "org-123", &exportResponse{Destination: dto}, &destinationResponse{Destination: dto}); err != nil {
		t.Fatalf("mapServerToState: %v", err)
	}
	d := state.Destination
	if d.AuthMethod.ValueString() != "iam_role" {
		t.Errorf("auth_method = %q", d.AuthMethod.ValueString())
	}
	if d.ExternalID.ValueString() != "ext-123" {
		t.Errorf("external_id = %q, want ext-123", d.ExternalID.ValueString())
	}
	if d.IAMRoleArn.ValueString() != "arn:aws:iam::123:role/x" {
		t.Errorf("iam_role_arn = %q", d.IAMRoleArn.ValueString())
	}
}

func TestMapServerToState_NoDestination(t *testing.T) {
	ctx := context.Background()
	state := &logExportResourceModel{Destination: &destinationModel{Endpoint: types.StringValue("stale")}}
	if err := mapServerToState(ctx, state, "org-123", &exportResponse{}, &destinationResponse{}); err != nil {
		t.Fatalf("mapServerToState: %v", err)
	}
	if state.Destination != nil {
		t.Errorf("destination should be nil when the server reports none, got %+v", state.Destination)
	}
}

func TestMapServerToState_IncludedEventTypes(t *testing.T) {
	ctx := context.Background()
	dto := destinationDTO{Endpoint: "https://s3", Bucket: "b"}

	t.Run("server has values", func(t *testing.T) {
		state := &logExportResourceModel{Settings: &settingsModel{IncludedEventTypes: types.ListNull(types.StringType)}}
		exp := &exportResponse{Destination: dto, Settings: settingsDTO{IncludedEventTypes: []string{"a", "b"}}}
		if err := mapServerToState(ctx, state, "o", exp, &destinationResponse{Destination: dto}); err != nil {
			t.Fatal(err)
		}
		if l := len(state.Settings.IncludedEventTypes.Elements()); l != 2 {
			t.Errorf("included_event_types len = %d, want 2", l)
		}
	})

	t.Run("server empty, prior unknown settles to null", func(t *testing.T) {
		state := &logExportResourceModel{Settings: &settingsModel{IncludedEventTypes: types.ListUnknown(types.StringType)}}
		if err := mapServerToState(ctx, state, "o", &exportResponse{Destination: dto}, &destinationResponse{Destination: dto}); err != nil {
			t.Fatal(err)
		}
		if !state.Settings.IncludedEventTypes.IsNull() {
			t.Errorf("want null, got %v", state.Settings.IncludedEventTypes)
		}
	})

	t.Run("server empty, prior known empty list is preserved", func(t *testing.T) {
		empty := mustStringList(t)
		state := &logExportResourceModel{Settings: &settingsModel{IncludedEventTypes: empty}}
		if err := mapServerToState(ctx, state, "o", &exportResponse{Destination: dto}, &destinationResponse{Destination: dto}); err != nil {
			t.Fatal(err)
		}
		if state.Settings.IncludedEventTypes.IsNull() {
			t.Error("known empty list must not be replaced with null (would be an inconsistent result)")
		}
	})
}

func TestBuildSettingsRequest(t *testing.T) {
	ctx := context.Background()

	t.Run("known sent, unknown and null omitted", func(t *testing.T) {
		s := &settingsModel{
			BatchSize:            types.Int64Value(50),
			FlushIntervalSeconds: types.Int64Unknown(),
			MaxRetries:           types.Int64Null(),
			IncludedEventTypes:   mustStringList(t, "a", "b"),
		}
		body, err := buildSettingsRequest(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		if body.BatchSize == nil || *body.BatchSize != 50 {
			t.Errorf("batch_size = %v, want 50", body.BatchSize)
		}
		if body.FlushIntervalSeconds != nil {
			t.Error("unknown flush_interval_seconds should be omitted")
		}
		if body.MaxRetries != nil {
			t.Error("null max_retries should be omitted")
		}
		if body.IncludedEventTypes == nil || len(*body.IncludedEventTypes) != 2 {
			t.Errorf("included_event_types = %v, want 2 entries", body.IncludedEventTypes)
		}
	})

	t.Run("empty list is sent to clear", func(t *testing.T) {
		s := &settingsModel{IncludedEventTypes: mustStringList(t)}
		body, err := buildSettingsRequest(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		if body.IncludedEventTypes == nil {
			t.Fatal("a known empty list must be sent (to clear server-side) — got nil")
		}
		if len(*body.IncludedEventTypes) != 0 {
			t.Errorf("want empty slice, got %v", *body.IncludedEventTypes)
		}
	})

	t.Run("null list omitted", func(t *testing.T) {
		s := &settingsModel{IncludedEventTypes: types.ListNull(types.StringType)}
		body, err := buildSettingsRequest(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		if body.IncludedEventTypes != nil {
			t.Error("null included_event_types should be omitted")
		}
	})
}

func TestDestinationEqual(t *testing.T) {
	base := func() *destinationModel {
		return &destinationModel{
			Endpoint:        types.StringValue("e"),
			Bucket:          types.StringValue("b"),
			Region:          types.StringValue("r"),
			AccessKeyID:     types.StringValue("ak"),
			SecretAccessKey: types.StringValue("sk"),
			ExternalID:      types.StringValue("x"),
			HasCredentials:  types.BoolValue(true),
		}
	}

	if !destinationEqual(base(), base()) {
		t.Error("identical destinations should be equal")
	}
	if !destinationEqual(nil, nil) {
		t.Error("nil == nil")
	}
	if destinationEqual(nil, base()) {
		t.Error("nil != non-nil")
	}

	computedDiff := base()
	computedDiff.ExternalID = types.StringValue("different")
	computedDiff.HasCredentials = types.BoolValue(false)
	if !destinationEqual(base(), computedDiff) {
		t.Error("computed-only differences (external_id/has_credentials) must not count as a change")
	}

	secretRotated := base()
	secretRotated.SecretAccessKey = types.StringValue("rotated")
	if destinationEqual(base(), secretRotated) {
		t.Error("a secret_access_key change must be detected")
	}
}

func TestSettingsEqual(t *testing.T) {
	a := &settingsModel{BatchSize: types.Int64Value(100), IncludedEventTypes: types.ListNull(types.StringType)}
	b := &settingsModel{BatchSize: types.Int64Value(100), IncludedEventTypes: types.ListNull(types.StringType)}
	if !settingsEqual(a, b) {
		t.Error("identical settings should be equal")
	}
	b.BatchSize = types.Int64Value(200)
	if settingsEqual(a, b) {
		t.Error("batch_size change should be detected")
	}
	if !settingsEqual(nil, nil) || settingsEqual(a, nil) {
		t.Error("nil handling is wrong")
	}
}

func TestReconcileEnabled(t *testing.T) {
	tests := []struct {
		name     string
		desired  bool
		current  bool
		wantCall string
	}{
		{"start when newly enabled", true, false, "POST /api/system-management/public/v1/exports/org-123/datadog-json/start"},
		{"pause when newly disabled", false, true, "POST /api/system-management/public/v1/exports/org-123/datadog-json/pause"},
		{"no-op when already enabled", true, true, ""},
		{"no-op when already disabled", false, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			r := &logExportResource{client: newTestClient(t, recordingHandler(&calls, "{}"))}
			if err := r.reconcileEnabled(context.Background(), "org-123", "datadog-json", tc.desired, tc.current); err != nil {
				t.Fatalf("reconcileEnabled: %v", err)
			}
			if tc.wantCall == "" {
				if len(calls) != 0 {
					t.Errorf("expected no API calls, got %v", calls)
				}
				return
			}
			if len(calls) != 1 || calls[0] != tc.wantCall {
				t.Errorf("calls = %v, want [%s]", calls, tc.wantCall)
			}
		})
	}
}

func TestReadInto(t *testing.T) {
	const exportJSON = `{"organization_id":"org-123","export_type":"datadog-json","enabled":true,` +
		`"settings":{"batch_size":200,"flush_interval_seconds":45,"max_retries":5},` +
		`"destination":{"endpoint":"https://s3","bucket":"b","region":"us-east-1","auth_method":"access_keys","use_ssl":true,"has_credentials":true}}`
	const destJSON = `{"organization_id":"org-123","export_type":"datadog-json","enabled":true,` +
		`"destination":{"endpoint":"https://s3","bucket":"b","region":"us-east-1","auth_method":"access_keys","use_ssl":true,"has_credentials":true}}`

	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			writeToken(w)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/destination"):
			_, _ = w.Write([]byte(destJSON))
		case r.Method == http.MethodGet && r.URL.Path == "/api/system-management/public/v1/exports/org-123/datadog-json":
			_, _ = w.Write([]byte(exportJSON))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}

	r := &logExportResource{client: newTestClient(t, handler)}
	state := &logExportResourceModel{
		OrganizationID: types.StringValue("org-123"),
		ExportType:     types.StringValue("datadog-json"),
		Destination: &destinationModel{
			AccessKeyID:     types.StringValue("AKIA"),
			SecretAccessKey: types.StringValue("secret"),
		},
		Settings: &settingsModel{IncludedEventTypes: types.ListNull(types.StringType)},
	}

	if err := r.readInto(context.Background(), state); err != nil {
		t.Fatalf("readInto: %v", err)
	}
	if !state.Enabled.ValueBool() {
		t.Error("enabled should be true")
	}
	if state.Settings.BatchSize.ValueInt64() != 200 {
		t.Errorf("batch_size = %d, want 200", state.Settings.BatchSize.ValueInt64())
	}
	if state.Destination == nil || state.Destination.Bucket.ValueString() != "b" {
		t.Errorf("destination mismapped: %+v", state.Destination)
	}
	if state.Destination.AccessKeyID.ValueString() != "AKIA" || state.Destination.SecretAccessKey.ValueString() != "secret" {
		t.Error("access keys must be preserved across a read (the API never returns them)")
	}
}

func TestDoJSONErrors(t *testing.T) {
	t.Run("404 is NotFound", func(t *testing.T) {
		r := &logExportResource{client: newTestClient(t, statusHandler(http.StatusNotFound, "export not found"))}
		err := r.doJSON(context.Background(), http.MethodGet, "exports/o/t", nil, nil)
		if !isNotFound(err) {
			t.Errorf("expected NotFound, got %v", err)
		}
	})
	t.Run("500 is a non-NotFound error", func(t *testing.T) {
		r := &logExportResource{client: newTestClient(t, statusHandler(http.StatusInternalServerError, "boom"))}
		err := r.doJSON(context.Background(), http.MethodGet, "exports/o/t", nil, nil)
		if err == nil {
			t.Fatal("expected an error")
		}
		if isNotFound(err) {
			t.Error("500 must not be treated as NotFound")
		}
	})
}

// --- test helpers ------------------------------------------------------------

func mustStringList(t *testing.T, elems ...string) types.List {
	t.Helper()
	if elems == nil {
		elems = []string{}
	}
	l, diags := types.ListValueFrom(context.Background(), types.StringType, elems)
	if diags.HasError() {
		t.Fatalf("build list: %+v", diags)
	}
	return l
}

func newTestClient(t *testing.T, h http.HandlerFunc) *client.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return client.New(client.Config{
		// base_url is the host root (v0.2.0); resource paths carry the
		// service prefix themselves.
		BaseURL:        srv.URL,
		TokenURL:       srv.URL + "/token",
		ClientID:       "id",
		ClientSecret:   "secret",
		OrganizationID: "org-123",
	})
}

func writeToken(w http.ResponseWriter) {
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "test-token", "expires_in": 3600})
}

func recordingHandler(calls *[]string, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			writeToken(w)
			return
		}
		*calls = append(*calls, r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte(body))
	}
}

func statusHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			writeToken(w)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}
