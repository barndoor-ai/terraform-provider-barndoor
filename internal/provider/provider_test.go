// Copyright (c) Barndoor AI, Inc.
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
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
