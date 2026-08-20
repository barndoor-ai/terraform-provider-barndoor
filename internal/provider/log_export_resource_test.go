// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
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
		"provider", "endpoint", "region", "bucket", "path_prefix", "use_ssl", "use_path_style",
		"auth_method", "iam_role_arn", "access_key_id", "secret_access_key",
		"account_key", "sas_token", "external_id", "has_credentials",
	} {
		if _, ok := dest.Attributes[attr]; !ok {
			t.Errorf("destination missing attribute %q", attr)
		}
	}

	// Every credential attribute must be Sensitive so it is redacted in plan
	// output. (access_key_id is an identifier, not a secret, and is not.)
	for _, secret := range []string{"secret_access_key", "account_key", "sas_token"} {
		if !dest.Attributes[secret].IsSensitive() {
			t.Errorf("destination.%s must be Sensitive", secret)
		}
	}

	// provider carries the s3 default, so an existing S3 configuration that
	// never mentions it keeps planning empty.
	if p := dest.Attributes["provider"]; !p.IsOptional() || !p.IsComputed() {
		t.Error("destination.provider should be Optional and Computed (it defaults to s3)")
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

// s3Request asserts buildDestinationRequest dispatched to the S3 body and
// returns it. A model that should serialize as S3 but comes back as the Azure
// type is a routing bug, not an assertion failure on a field.
func s3Request(t *testing.T, d *destinationModel) configureDestinationRequest {
	t.Helper()
	req, ok := buildDestinationRequest(d).(configureDestinationRequest)
	if !ok {
		t.Fatalf("buildDestinationRequest returned %T, want configureDestinationRequest", buildDestinationRequest(d))
	}
	return req
}

func azureRequest(t *testing.T, d *destinationModel) configureAzureDestinationRequest {
	t.Helper()
	req, ok := buildDestinationRequest(d).(configureAzureDestinationRequest)
	if !ok {
		t.Fatalf("buildDestinationRequest returned %T, want configureAzureDestinationRequest", buildDestinationRequest(d))
	}
	return req
}

func TestBuildDestinationRequest_AccessKeys(t *testing.T) {
	d := &destinationModel{
		Provider:        types.StringValue(storageProviderS3),
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

	req := s3Request(t, d)

	if req.Provider != storageProviderS3 {
		t.Errorf("provider = %q, want %q (sent explicitly, not left to the server default)", req.Provider, storageProviderS3)
	}
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

// TestBuildDestinationRequest_NullProviderIsS3 covers the upgrade path: a
// configuration written before `provider` existed leaves it null in state until
// the next plan applies the default, and must still serialize as S3.
func TestBuildDestinationRequest_NullProviderIsS3(t *testing.T) {
	d := &destinationModel{
		Provider:        types.StringNull(),
		Endpoint:        types.StringValue("https://s3.us-east-1.amazonaws.com"),
		Bucket:          types.StringValue("audit-logs"),
		AuthMethod:      types.StringValue(authMethodAccessKeys),
		AccessKeyID:     types.StringValue("AKIA..."),
		SecretAccessKey: types.StringValue("secret"),
	}

	if req := s3Request(t, d); req.Provider != storageProviderS3 {
		t.Errorf("provider = %q, want %q", req.Provider, storageProviderS3)
	}
}

func TestBuildDestinationRequest_IAMRole(t *testing.T) {
	d := &destinationModel{
		Provider:        types.StringValue(storageProviderS3),
		Endpoint:        types.StringValue("https://s3.us-east-1.amazonaws.com"),
		Region:          types.StringValue("us-east-1"),
		Bucket:          types.StringValue("audit-logs"),
		AuthMethod:      types.StringValue(authMethodIAMRole),
		IAMRoleArn:      types.StringValue("arn:aws:iam::123456789012:role/barndoor"),
		AccessKeyID:     types.StringNull(),
		SecretAccessKey: types.StringNull(),
	}

	req := s3Request(t, d)

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

func TestBuildDestinationRequest_AzureAccountKey(t *testing.T) {
	req := azureRequest(t, azureAccountKeyModel())

	if req.Provider != storageProviderAzureBlob {
		t.Errorf("provider = %q, want %q", req.Provider, storageProviderAzureBlob)
	}
	if req.Endpoint != "https://acmeaudit.blob.core.windows.net" {
		t.Errorf("endpoint = %q", req.Endpoint)
	}
	if req.Bucket != "barndoor-audit" {
		t.Errorf("bucket (container) = %q", req.Bucket)
	}
	if req.PathPrefix != "acme/" {
		t.Errorf("path_prefix = %q", req.PathPrefix)
	}
	if req.AuthMethod != authMethodAccountKey {
		t.Errorf("auth_method = %q, want %q", req.AuthMethod, authMethodAccountKey)
	}
	if req.AccountKey != "azure-account-key" {
		t.Errorf("account_key not propagated: %q", req.AccountKey)
	}
	if req.SASToken != "" {
		t.Errorf("sas_token = %q, want empty for account_key auth", req.SASToken)
	}
}

func TestBuildDestinationRequest_AzureSASToken(t *testing.T) {
	req := azureRequest(t, azureSASTokenModel())

	if req.AuthMethod != authMethodSASToken {
		t.Errorf("auth_method = %q, want %q", req.AuthMethod, authMethodSASToken)
	}
	if req.SASToken != "sv=2024-01-01&sig=abc" {
		t.Errorf("sas_token not propagated: %q", req.SASToken)
	}
	if req.AccountKey != "" {
		t.Errorf("account_key = %q, want empty for sas_token auth", req.AccountKey)
	}
}

// TestBuildDestinationRequest_AzureJSONKeys is the load-bearing guard for the
// Azure body: it asserts on the marshalled JSON, not the Go struct. The API
// rejects an Azure destination that carries region/use_ssl/use_path_style/
// iam_role_arn/access_key_id/secret_access_key, and `use_ssl` has a schema
// Default of true — so a regression that serialized the shared S3 struct would
// put "use_ssl": true on the wire and 400 on every apply, while every
// field-level assertion above still passed.
func TestBuildDestinationRequest_AzureJSONKeys(t *testing.T) {
	for name, d := range map[string]*destinationModel{
		"account_key": azureAccountKeyModel(),
		"sas_token":   azureSASTokenModel(),
	} {
		t.Run(name, func(t *testing.T) {
			// The model deliberately carries the S3 attributes at their schema
			// defaults, exactly as a plan would.
			raw, err := json.Marshal(buildDestinationRequest(d))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got map[string]json.RawMessage
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			for _, forbidden := range []string{
				"region", "use_ssl", "use_path_style", "iam_role_arn", "access_key_id", "secret_access_key",
			} {
				if _, present := got[forbidden]; present {
					t.Errorf("azure request body must not carry %q (the API rejects it); body = %s", forbidden, raw)
				}
			}
			for _, required := range []string{"provider", "endpoint", "bucket", "auth_method"} {
				if _, present := got[required]; !present {
					t.Errorf("azure request body is missing %q; body = %s", required, raw)
				}
			}
			// Exactly one secret, matching the declared auth method.
			_, hasAccountKey := got["account_key"]
			_, hasSAS := got["sas_token"]
			if hasAccountKey == hasSAS {
				t.Errorf("azure body must carry exactly one of account_key/sas_token; body = %s", raw)
			}
			if name == "account_key" && !hasAccountKey {
				t.Errorf("account_key auth must send account_key; body = %s", raw)
			}
			if name == "sas_token" && !hasSAS {
				t.Errorf("sas_token auth must send sas_token; body = %s", raw)
			}
		})
	}
}

// azureAccountKeyModel is a plan-shaped Azure destination: the S3-only
// attributes sit at the values the schema's defaults would give them, which is
// what the request builder and the state mapper must both cope with.
func azureAccountKeyModel() *destinationModel {
	return &destinationModel{
		Provider:        types.StringValue(storageProviderAzureBlob),
		Endpoint:        types.StringValue("https://acmeaudit.blob.core.windows.net"),
		Bucket:          types.StringValue("barndoor-audit"),
		PathPrefix:      types.StringValue("acme/"),
		AuthMethod:      types.StringValue(authMethodAccountKey),
		AccountKey:      types.StringValue("azure-account-key"),
		SASToken:        types.StringNull(),
		Region:          types.StringNull(),
		IAMRoleArn:      types.StringNull(),
		AccessKeyID:     types.StringNull(),
		SecretAccessKey: types.StringNull(),
		UseSSL:          types.BoolValue(true),  // schema Default
		UsePathStyle:    types.BoolValue(false), // schema Default
	}
}

func azureSASTokenModel() *destinationModel {
	d := azureAccountKeyModel()
	d.AuthMethod = types.StringValue(authMethodSASToken)
	d.AccountKey = types.StringNull()
	d.SASToken = types.StringValue("sv=2024-01-01&sig=abc")
	return d
}

// s3ConfigModel is a *config*-shaped S3 destination: every attribute the
// practitioner did not write is null, including the ones carrying a schema
// Default. ValidateConfig sees the config, not the plan, and the null-vs-set
// distinction is exactly what the Azure branch keys on.
func s3ConfigModel() *destinationModel {
	return &destinationModel{
		Provider:        types.StringNull(),
		Endpoint:        types.StringValue("https://s3.us-east-1.amazonaws.com"),
		Bucket:          types.StringValue("audit-logs"),
		Region:          types.StringValue("us-east-1"),
		PathPrefix:      types.StringNull(),
		UseSSL:          types.BoolNull(),
		UsePathStyle:    types.BoolNull(),
		AuthMethod:      types.StringValue(authMethodAccessKeys),
		IAMRoleArn:      types.StringNull(),
		AccessKeyID:     types.StringValue("AKIA..."),
		SecretAccessKey: types.StringValue("secret"),
		AccountKey:      types.StringNull(),
		SASToken:        types.StringNull(),
	}
}

// azureConfigModel is a config-shaped, valid azure_blob destination using the
// given auth method.
func azureConfigModel(authMethod string) *destinationModel {
	d := &destinationModel{
		Provider:        types.StringValue(storageProviderAzureBlob),
		Endpoint:        types.StringValue("https://acmeaudit.blob.core.windows.net"),
		Bucket:          types.StringValue("barndoor-audit"),
		Region:          types.StringNull(),
		PathPrefix:      types.StringNull(),
		UseSSL:          types.BoolNull(),
		UsePathStyle:    types.BoolNull(),
		AuthMethod:      types.StringValue(authMethod),
		IAMRoleArn:      types.StringNull(),
		AccessKeyID:     types.StringNull(),
		SecretAccessKey: types.StringNull(),
		AccountKey:      types.StringNull(),
		SASToken:        types.StringNull(),
	}
	if authMethod == authMethodSASToken {
		d.SASToken = types.StringValue("sv=2024-01-01&sig=abc")
	} else {
		d.AccountKey = types.StringValue("azure-account-key")
	}
	return d
}

// withProvider overrides a model's provider and applies an optional mutation,
// so a table entry can express "this valid config, but with X" in one line.
func withProvider(d *destinationModel, provider string, mutate func(*destinationModel)) *destinationModel {
	d.Provider = types.StringValue(provider)
	if mutate != nil {
		mutate(d)
	}
	return d
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
		"s3 rejects an azure account_key": {
			dest: withProvider(s3ConfigModel(), storageProviderS3, func(d *destinationModel) {
				d.AccountKey = types.StringValue("azure-account-key")
			}),
			wantError: true,
		},
		"s3 rejects an azure sas_token": {
			dest: withProvider(s3ConfigModel(), storageProviderS3, func(d *destinationModel) {
				d.SASToken = types.StringValue("sv=2024-01-01&sig=abc")
			}),
			wantError: true,
		},
		"azure with account_key": {
			dest: azureConfigModel(authMethodAccountKey),
		},
		"azure with sas_token": {
			dest: azureConfigModel(authMethodSASToken),
		},
		"azure missing auth_method": {
			// auth_method's `access_keys` default does not apply to Azure, so an
			// omitted (null in config) value must be rejected at plan time
			// rather than defaulted into an API 400.
			dest: withProvider(azureConfigModel(authMethodAccountKey), storageProviderAzureBlob, func(d *destinationModel) {
				d.AuthMethod = types.StringNull()
			}),
			wantError: true,
		},
		"azure with an s3 auth_method": {
			dest: withProvider(azureConfigModel(authMethodAccountKey), storageProviderAzureBlob, func(d *destinationModel) {
				d.AuthMethod = types.StringValue(authMethodAccessKeys)
			}),
			wantError: true,
		},
		"azure account_key auth missing the account key": {
			dest: withProvider(azureConfigModel(authMethodAccountKey), storageProviderAzureBlob, func(d *destinationModel) {
				d.AccountKey = types.StringNull()
			}),
			wantError: true,
		},
		"azure account_key auth with a stray sas_token": {
			dest: withProvider(azureConfigModel(authMethodAccountKey), storageProviderAzureBlob, func(d *destinationModel) {
				d.SASToken = types.StringValue("sv=2024-01-01&sig=abc")
			}),
			wantError: true,
		},
		"azure sas_token auth missing the token": {
			dest: withProvider(azureConfigModel(authMethodSASToken), storageProviderAzureBlob, func(d *destinationModel) {
				d.SASToken = types.StringNull()
			}),
			wantError: true,
		},
		"azure sas_token auth with a stray account_key": {
			dest: withProvider(azureConfigModel(authMethodSASToken), storageProviderAzureBlob, func(d *destinationModel) {
				d.AccountKey = types.StringValue("azure-account-key")
			}),
			wantError: true,
		},
		"azure rejects region": {
			dest: withProvider(azureConfigModel(authMethodAccountKey), storageProviderAzureBlob, func(d *destinationModel) {
				d.Region = types.StringValue("us-east-1")
			}),
			wantError: true,
		},
		"azure rejects iam_role_arn": {
			dest: withProvider(azureConfigModel(authMethodAccountKey), storageProviderAzureBlob, func(d *destinationModel) {
				d.IAMRoleArn = types.StringValue("arn:aws:iam::123:role/x")
			}),
			wantError: true,
		},
		"azure rejects access_key_id": {
			dest: withProvider(azureConfigModel(authMethodAccountKey), storageProviderAzureBlob, func(d *destinationModel) {
				d.AccessKeyID = types.StringValue("AKIA...")
			}),
			wantError: true,
		},
		"azure rejects secret_access_key": {
			dest: withProvider(azureConfigModel(authMethodAccountKey), storageProviderAzureBlob, func(d *destinationModel) {
				d.SecretAccessKey = types.StringValue("secret")
			}),
			wantError: true,
		},
		"azure rejects an explicit use_ssl = true": {
			// The schema default is also true, but a *config* value means the
			// practitioner set it, and Azure takes its scheme from `endpoint`.
			dest: withProvider(azureConfigModel(authMethodAccountKey), storageProviderAzureBlob, func(d *destinationModel) {
				d.UseSSL = types.BoolValue(true)
			}),
			wantError: true,
		},
		"azure rejects an explicit use_ssl = false": {
			dest: withProvider(azureConfigModel(authMethodAccountKey), storageProviderAzureBlob, func(d *destinationModel) {
				d.UseSSL = types.BoolValue(false)
			}),
			wantError: true,
		},
		"azure rejects an explicit use_path_style": {
			dest: withProvider(azureConfigModel(authMethodAccountKey), storageProviderAzureBlob, func(d *destinationModel) {
				d.UsePathStyle = types.BoolValue(false)
			}),
			wantError: true,
		},
		"s3 with an unknown auth_method is deferred, not rejected": {
			// auth_method comes from an unresolved expression and is paired with
			// iam_role_arn. Folding the unknown into the `access_keys` default
			// would reject this at validate time even though it is a valid
			// iam_role configuration once the variable resolves.
			dest: withProvider(s3ConfigModel(), storageProviderS3, func(d *destinationModel) {
				d.AuthMethod = types.StringUnknown()
				d.IAMRoleArn = types.StringValue("arn:aws:iam::123:role/x")
				d.AccessKeyID = types.StringNull()
				d.SecretAccessKey = types.StringNull()
			}),
		},
		"s3 with an unknown auth_method still rejects an azure secret": {
			// The provider-level checks run before the auth_method deferral, so
			// a cross-provider attribute is still caught.
			dest: withProvider(s3ConfigModel(), storageProviderS3, func(d *destinationModel) {
				d.AuthMethod = types.StringUnknown()
				d.AccountKey = types.StringValue("azure-account-key")
			}),
			wantError: true,
		},
		"unknown provider is deferred, not rejected": {
			// provider comes from an unresolved expression: neither branch can be
			// checked yet, and erroring here would fail a legitimate plan.
			dest: withProvider(azureConfigModel(authMethodAccountKey), storageProviderAzureBlob, func(d *destinationModel) {
				d.Provider = types.StringUnknown()
			}),
		},
		"invalid provider": {
			dest:      withProvider(azureConfigModel(authMethodAccountKey), "gcs", nil),
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

// azureDTO is the destination the API reports for the model built by
// azureAccountKeyModel: the Azure attributes echoed back, every S3 attribute at
// its zero value (Azure rows simply do not have them).
func azureDTO() destinationDTO {
	return destinationDTO{
		Provider:       storageProviderAzureBlob,
		Endpoint:       "https://acmeaudit.blob.core.windows.net",
		Bucket:         "barndoor-audit",
		PathPrefix:     "acme/",
		AuthMethod:     authMethodAccountKey,
		HasCredentials: true,
		Region:         "",
		UseSSL:         false,
		UsePathStyle:   false,
		IAMRoleArn:     "",
		ExternalID:     "",
	}
}

// destinationFieldValues reflects over every tfsdk-tagged field of a
// destinationModel. Reflection rather than a hand-written field list means a
// destination attribute added later is covered by the round-trip assertions
// automatically, instead of silently escaping them.
func destinationFieldValues(t *testing.T, d *destinationModel) map[string]attr.Value {
	t.Helper()
	out := map[string]attr.Value{}
	v := reflect.ValueOf(*d)
	typ := v.Type()
	for i := range typ.NumField() {
		name := typ.Field(i).Tag.Get("tfsdk")
		if name == "" {
			t.Fatalf("destinationModel field %s has no tfsdk tag", typ.Field(i).Name)
		}
		val, ok := v.Field(i).Interface().(attr.Value)
		if !ok {
			t.Fatalf("destinationModel field %s is %T, not an attr.Value", typ.Field(i).Name, v.Field(i).Interface())
		}
		out[name] = val
	}
	if len(out) == 0 {
		t.Fatal("destinationModel has no tfsdk-tagged fields; the round-trip assertions would prove nothing")
	}
	return out
}

// TestAzureDestination_PlanRoundTrip is the plan-stability guard. It pairs the
// real request builder with the real state mapper against a realistic API
// response and asserts every model field lands back on the value the plan held.
// Any field that does not is a perpetual diff (or, right after apply, a
// "provider produced an inconsistent result" error): use_ssl is the one that
// actually bites, since its schema Default is true while the API reports false
// for every Azure row.
func TestAzureDestination_PlanRoundTrip(t *testing.T) {
	ctx := context.Background()

	// The plan: Azure attributes from config, S3-only attributes at their
	// schema defaults, Computed attributes still unknown.
	plan := azureAccountKeyModel()
	plan.ExternalID = types.StringUnknown()
	plan.HasCredentials = types.BoolUnknown()

	// Sanity-check that the response really is derived from what we would send,
	// so the fixture cannot drift away from the builder.
	req := azureRequest(t, plan)
	dto := azureDTO()
	if req.Endpoint != dto.Endpoint || req.Bucket != dto.Bucket || req.AuthMethod != dto.AuthMethod || req.PathPrefix != dto.PathPrefix {
		t.Fatalf("fixture drift: request %+v does not match the response fixture %+v", req, dto)
	}

	state := &logExportResourceModel{Destination: plan}
	if err := mapServerToState(ctx, state, "org-123", &exportResponse{Destination: dto}, &destinationResponse{Destination: dto}); err != nil {
		t.Fatalf("mapServerToState: %v", err)
	}
	if state.Destination == nil {
		t.Fatal("destination should not be nil")
		return
	}

	// Computed attributes are the only ones allowed to move off the plan: they
	// were unknown and must settle on the server's answer.
	wantComputed := map[string]attr.Value{
		"external_id":     types.StringNull(), // Azure has no sts:ExternalId
		"has_credentials": types.BoolValue(true),
	}

	got := destinationFieldValues(t, state.Destination)
	want := destinationFieldValues(t, azureAccountKeyModel())
	for name, wantVal := range want {
		if computed, ok := wantComputed[name]; ok {
			if !got[name].Equal(computed) {
				t.Errorf("computed %s = %v, want %v", name, got[name], computed)
			}
			continue
		}
		if !got[name].Equal(wantVal) {
			t.Errorf("%s = %v after apply, but the plan had %v — a re-plan would show a diff", name, got[name], wantVal)
		}
	}

	// A refresh maps the same response over the state it just produced. It must
	// be a fixed point, or every `terraform plan` after the first shows a diff.
	refreshed := &logExportResourceModel{Destination: state.Destination}
	if err := mapServerToState(ctx, refreshed, "org-123", &exportResponse{Destination: dto}, &destinationResponse{Destination: dto}); err != nil {
		t.Fatalf("mapServerToState (refresh): %v", err)
	}
	after := destinationFieldValues(t, refreshed.Destination)
	for name, before := range got {
		if !after[name].Equal(before) {
			t.Errorf("refresh moved %s from %v to %v; mapping is not idempotent", name, before, after[name])
		}
	}
}

// TestMapServerToState_AzureImport covers the no-prior-model path (terraform
// import): each S3-only attribute must settle on its schema default so a config
// that omits them plans empty.
func TestMapServerToState_AzureImport(t *testing.T) {
	ctx := context.Background()
	dto := azureDTO()

	// ImportState seeds no destination, so mapServerToState builds one from
	// nothing.
	state := &logExportResourceModel{}
	if err := mapServerToState(ctx, state, "org-123", &exportResponse{Destination: dto}, &destinationResponse{Destination: dto}); err != nil {
		t.Fatalf("mapServerToState: %v", err)
	}
	d := state.Destination
	if d == nil {
		t.Fatal("destination should not be nil")
		return
	}

	if d.Provider.ValueString() != storageProviderAzureBlob {
		t.Errorf("provider = %q, want %q", d.Provider.ValueString(), storageProviderAzureBlob)
	}
	if !d.UseSSL.ValueBool() {
		t.Error("use_ssl = false, want the schema default true (the API reports false for every Azure row)")
	}
	if d.UsePathStyle.ValueBool() {
		t.Error("use_path_style = true, want the schema default false")
	}
	for name, v := range map[string]types.String{
		"region":            d.Region,
		"iam_role_arn":      d.IAMRoleArn,
		"external_id":       d.ExternalID,
		"access_key_id":     d.AccessKeyID,
		"secret_access_key": d.SecretAccessKey,
		"account_key":       d.AccountKey,
		"sas_token":         d.SASToken,
	} {
		if !v.IsNull() {
			t.Errorf("%s = %v, want null on an imported Azure destination", name, v)
		}
	}
	if d.AuthMethod.ValueString() != authMethodAccountKey {
		t.Errorf("auth_method = %q, want %q", d.AuthMethod.ValueString(), authMethodAccountKey)
	}
}

// TestMapServerToState_AzureSecretsAreConfigOnly pins that the Azure secrets are
// carried from the prior model, exactly like the S3 access keys: the API never
// returns them, so mapping would otherwise blank them out of state.
func TestMapServerToState_AzureSecretsAreConfigOnly(t *testing.T) {
	ctx := context.Background()
	dto := azureDTO()
	dto.AuthMethod = authMethodSASToken

	prior := azureSASTokenModel()
	state := &logExportResourceModel{Destination: prior}
	if err := mapServerToState(ctx, state, "org-123", &exportResponse{Destination: dto}, &destinationResponse{Destination: dto}); err != nil {
		t.Fatalf("mapServerToState: %v", err)
	}
	if got := state.Destination.SASToken.ValueString(); got != "sv=2024-01-01&sig=abc" {
		t.Errorf("sas_token = %q; it must be carried from the prior model (the API never returns it)", got)
	}
	if !state.Destination.AccountKey.IsNull() {
		t.Errorf("account_key = %v, want null", state.Destination.AccountKey)
	}
}

// TestMapServerToState_LegacyRowIsS3 covers a destination configured before the
// API had a provider field: the field is absent, and the row is S3.
func TestMapServerToState_LegacyRowIsS3(t *testing.T) {
	ctx := context.Background()
	dto := destinationDTO{
		Endpoint: "https://s3", Bucket: "b", Region: "us-east-1",
		AuthMethod: "access_keys", UseSSL: true, HasCredentials: true,
		// Provider intentionally absent.
	}
	state := &logExportResourceModel{Destination: &destinationModel{}}
	if err := mapServerToState(ctx, state, "org-123", &exportResponse{Destination: dto}, &destinationResponse{Destination: dto}); err != nil {
		t.Fatalf("mapServerToState: %v", err)
	}
	if got := state.Destination.Provider.ValueString(); got != storageProviderS3 {
		t.Errorf("provider = %q, want %q for a legacy row with no provider field", got, storageProviderS3)
	}
	// The S3 mapping must stay server-driven — the Azure preservation path must
	// not leak into it.
	if got := state.Destination.Region.ValueString(); got != "us-east-1" {
		t.Errorf("region = %q, want the server's us-east-1", got)
	}
	if !state.Destination.UseSSL.ValueBool() {
		t.Error("use_ssl should come from the server for an S3 destination")
	}
}

// TestMapServerToState_S3RegionStillTracksTheServer guards the other direction:
// an S3 destination whose region the server changed (or cleared) must follow the
// server, since region is server-owned for S3.
func TestMapServerToState_S3RegionStillTracksTheServer(t *testing.T) {
	ctx := context.Background()
	dto := destinationDTO{
		Provider: storageProviderS3, Endpoint: "https://s3", Bucket: "b",
		Region: "eu-west-1", AuthMethod: authMethodAccessKeys, UseSSL: false,
	}
	state := &logExportResourceModel{Destination: &destinationModel{
		Region: types.StringValue("us-east-1"),
		UseSSL: types.BoolValue(true),
	}}
	if err := mapServerToState(ctx, state, "o", &exportResponse{Destination: dto}, &destinationResponse{Destination: dto}); err != nil {
		t.Fatalf("mapServerToState: %v", err)
	}
	if got := state.Destination.Region.ValueString(); got != "eu-west-1" {
		t.Errorf("region = %q, want the server's eu-west-1 (drift must be visible for S3)", got)
	}
	if state.Destination.UseSSL.ValueBool() {
		t.Error("use_ssl should follow the server for S3, not be pinned to the plan")
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
