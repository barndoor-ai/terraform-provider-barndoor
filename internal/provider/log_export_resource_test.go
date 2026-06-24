// Copyright (c) Barndoor AI, Inc.
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
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
