// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
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
		if want := "/api/system-management/public/v1/exports/org-123/datadog-json/destination/aws-trust-info"; gotPath != want {
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

func TestAddTrustInfoError(t *testing.T) {
	apiErr := func(status int) error {
		return &apiError{method: http.MethodGet, path: "exports/o/t/destination/aws-trust-info", status: status, body: "boom"}
	}

	tests := map[string]struct {
		err           error
		wantSummary   string
		wantDetailSub string // optional substring the detail must contain
	}{
		"403 leads with iam_role but does not claim it is the only cause": {
			err:           apiErr(http.StatusForbidden),
			wantSummary:   "Access denied reading AWS trust info (HTTP 403)",
			wantDetailSub: "iam_role",
		},
		"404 is export not found": {
			err:         apiErr(http.StatusNotFound),
			wantSummary: "Export not found",
		},
		"other status falls through to the generic error": {
			err:         apiErr(http.StatusInternalServerError),
			wantSummary: "Failed to read AWS trust info",
		},
		"non-API error falls through to the generic error": {
			err:         errors.New("dial tcp: connection refused"),
			wantSummary: "Failed to read AWS trust info",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var diags diag.Diagnostics
			addTrustInfoError(&diags, "org-123", "datadog-json", tc.err)

			if !diags.HasError() {
				t.Fatal("expected an error diagnostic, got none")
			}
			got := diags.Errors()[0]
			if got.Summary() != tc.wantSummary {
				t.Errorf("summary = %q, want %q", got.Summary(), tc.wantSummary)
			}
			if tc.wantDetailSub != "" && !strings.Contains(got.Detail(), tc.wantDetailSub) {
				t.Errorf("detail %q does not contain %q", got.Detail(), tc.wantDetailSub)
			}
		})
	}
}

func TestResolveTrustInfoTarget(t *testing.T) {
	tests := map[string]struct {
		cfgOrg      types.String
		cfgType     types.String
		providerOrg string
		wantOrg     string
		wantType    string
		wantErr     bool
	}{
		"both unset fall back to provider org and default type": {
			cfgOrg: types.StringNull(), cfgType: types.StringNull(), providerOrg: "org-prov",
			wantOrg: "org-prov", wantType: defaultExportType,
		},
		"config overrides both": {
			cfgOrg: types.StringValue("org-cfg"), cfgType: types.StringValue("custom-json"), providerOrg: "org-prov",
			wantOrg: "org-cfg", wantType: "custom-json",
		},
		"empty-string config org falls back to provider": {
			cfgOrg: types.StringValue(""), cfgType: types.StringNull(), providerOrg: "org-prov",
			wantOrg: "org-prov", wantType: defaultExportType,
		},
		"no org from config or provider is an error": {
			cfgOrg: types.StringNull(), cfgType: types.StringNull(), providerOrg: "",
			wantErr: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			orgID, exportType, diags := resolveTrustInfoTarget(tc.cfgOrg, tc.cfgType, tc.providerOrg)
			if diags.HasError() != tc.wantErr {
				t.Fatalf("HasError() = %v, want %v (diags: %+v)", diags.HasError(), tc.wantErr, diags)
			}
			if tc.wantErr {
				return
			}
			if orgID != tc.wantOrg {
				t.Errorf("orgID = %q, want %q", orgID, tc.wantOrg)
			}
			if exportType != tc.wantType {
				t.Errorf("exportType = %q, want %q", exportType, tc.wantType)
			}
		})
	}
}
