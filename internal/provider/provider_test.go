// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/provider"
)

func TestProvider_Metadata(t *testing.T) {
	var resp provider.MetadataResponse
	New("test")().Metadata(context.Background(), provider.MetadataRequest{}, &resp)

	if resp.TypeName != "barndoor" {
		t.Errorf("TypeName = %q, want %q", resp.TypeName, "barndoor")
	}
	if resp.Version != "test" {
		t.Errorf("Version = %q, want %q", resp.Version, "test")
	}
}

func TestProvider_Schema(t *testing.T) {
	var resp provider.SchemaResponse
	New("test")().Schema(context.Background(), provider.SchemaRequest{}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	for _, attr := range []string{"base_url", "token_url", "client_id", "client_secret", "organization_id"} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
	if !resp.Schema.Attributes["client_secret"].IsSensitive() {
		t.Error("client_secret attribute should be marked Sensitive")
	}
}

func TestInvalidBaseURLDetail(t *testing.T) {
	tests := map[string]struct {
		baseURL     string
		wantOK      bool   // true = accepted (empty detail)
		wantContain string // substring the detail must contain when rejected
	}{
		"host root is accepted":                     {baseURL: "https://platform.barndoor.ai", wantOK: true},
		"host root with trailing slash is accepted": {baseURL: "https://platform.barndoor.ai/", wantOK: true},
		"host root with port is accepted":           {baseURL: "https://platform.example.com:8443", wantOK: true},
		"pre-0.2.0 SMS-scoped URL is rejected with the migration": {
			baseURL:     "https://platform.barndoor.ai/api/system-management/public/v1",
			wantContain: "/api/system-management/public/v1",
		},
		"any other path suffix is rejected": {
			baseURL:     "https://platform.barndoor.ai/api",
			wantContain: "host root",
		},
		"unparsable URL is rejected": {
			baseURL:     "://nope",
			wantContain: "not a valid URL",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			detail := invalidBaseURLDetail(tc.baseURL)
			if tc.wantOK {
				if detail != "" {
					t.Errorf("invalidBaseURLDetail(%q) = %q, want it accepted", tc.baseURL, detail)
				}
				return
			}
			if detail == "" {
				t.Fatalf("invalidBaseURLDetail(%q) accepted, want a rejection", tc.baseURL)
			}
			if !strings.Contains(detail, tc.wantContain) {
				t.Errorf("detail %q does not contain %q", detail, tc.wantContain)
			}
		})
	}
}
