// Copyright (c) Barndoor AI, Inc.
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
)

func newTrustInfoSchema(t *testing.T) schema.Schema {
	t.Helper()
	var resp datasource.SchemaResponse
	NewLogExportAWSTrustInfoDataSource().Schema(context.Background(), datasource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}
	return resp.Schema
}

func TestLogExportAWSTrustInfoDataSource_Metadata(t *testing.T) {
	var resp datasource.MetadataResponse
	NewLogExportAWSTrustInfoDataSource().Metadata(context.Background(), datasource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_log_export_aws_trust_info"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestLogExportAWSTrustInfoDataSource_Schema(t *testing.T) {
	s := newTrustInfoSchema(t)

	for _, attr := range []string{"organization_id", "export_type", "principal_arn", "external_id"} {
		if _, ok := s.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}

	// The outputs are server-provided and never set by the practitioner.
	for _, computed := range []string{"principal_arn", "external_id"} {
		if !s.Attributes[computed].IsComputed() {
			t.Errorf("%s should be Computed", computed)
		}
		if s.Attributes[computed].IsOptional() {
			t.Errorf("%s should not be Optional", computed)
		}
	}

	// The inputs default from the provider, so they are optional + computed.
	for _, in := range []string{"organization_id", "export_type"} {
		if !s.Attributes[in].IsOptional() || !s.Attributes[in].IsComputed() {
			t.Errorf("%s should be Optional and Computed", in)
		}
	}
}

func TestFetchAWSTrustInfo(t *testing.T) {
	t.Run("decodes response and builds the expected path", func(t *testing.T) {
		var gotPath, gotMethod string
		h := func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/token" {
				writeToken(w)
				return
			}
			gotPath, gotMethod = r.URL.Path, r.Method
			_, _ = w.Write([]byte(`{"principal_arn":"arn:aws:iam::111122223333:role/barndoor-export","external_id":"ext-abc123"}`))
		}
		c := newTestClient(t, h)

		info, err := fetchAWSTrustInfo(context.Background(), c, "org-123", "datadog-json")
		if err != nil {
			t.Fatalf("fetchAWSTrustInfo: %v", err)
		}
		if info.PrincipalARN != "arn:aws:iam::111122223333:role/barndoor-export" {
			t.Errorf("principal_arn = %q", info.PrincipalARN)
		}
		if info.ExternalID != "ext-abc123" {
			t.Errorf("external_id = %q", info.ExternalID)
		}
		if want := "/public/v1/exports/org-123/datadog-json/destination/aws-trust-info"; gotPath != want {
			t.Errorf("request path = %q, want %q", gotPath, want)
		}
		if gotMethod != http.MethodGet {
			t.Errorf("method = %q, want GET", gotMethod)
		}
	})

	t.Run("403 surfaces as a non-NotFound apiError the Read can classify", func(t *testing.T) {
		c := newTestClient(t, statusHandler(http.StatusForbidden, "iam_role auth method is not enabled for this organization"))

		_, err := fetchAWSTrustInfo(context.Background(), c, "org-123", "datadog-json")
		if err == nil {
			t.Fatal("expected an error for 403")
		}
		if isNotFound(err) {
			t.Error("403 must not be treated as NotFound")
		}
		var apiErr *apiError
		if !errors.As(err, &apiErr) || apiErr.status != http.StatusForbidden {
			t.Errorf("expected *apiError with status 403, got %v", err)
		}
	})

	t.Run("404 is classified as NotFound", func(t *testing.T) {
		c := newTestClient(t, statusHandler(http.StatusNotFound, "export not found"))

		_, err := fetchAWSTrustInfo(context.Background(), c, "org-123", "datadog-json")
		if !isNotFound(err) {
			t.Errorf("expected NotFound, got %v", err)
		}
	})
}
