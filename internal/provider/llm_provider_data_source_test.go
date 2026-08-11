// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"regexp"
	"testing"

	frameworkdatasource "github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// --- schema tests --------------------------------------------------------------

func TestLlmProviderDataSource_Metadata(t *testing.T) {
	var resp frameworkdatasource.MetadataResponse
	NewLlmProviderDataSource().Metadata(context.Background(),
		frameworkdatasource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_llm_provider"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestLlmProviderDataSource_Schema(t *testing.T) {
	var resp frameworkdatasource.SchemaResponse
	NewLlmProviderDataSource().Schema(context.Background(), frameworkdatasource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{
		"id", "org_id", "name", "model_provider", "base_url", "auth_type",
		"settings", "enabled", "enforce_health_check", "health_status",
		"health_detail", "health_checked_at", "created_at", "updated_at",
	} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}

	// The provider credential is write-only on the resource and never returned
	// by the API in any form; the data source must not carry a slot for it at
	// all — its absence from the schema is the no-secret guarantee.
	if _, ok := resp.Schema.Attributes["api_key"]; ok {
		t.Error("data source schema must not have an api_key attribute — the credential is a write-only secret")
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

func TestLlmProviderDataSource_lookupByID(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	seeded := fake.seedProvider()
	const dataName = "data.barndoor_llm_provider.by_id"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
data "barndoor_llm_provider" "by_id" {
  id = %q
}
`, seeded.ID),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(dataName, "id", seeded.ID),
					resource.TestCheckResourceAttr(dataName, "org_id", fakeLlmOrgID),
					resource.TestCheckResourceAttr(dataName, "name", "Seeded openai"),
					resource.TestCheckResourceAttr(dataName, "model_provider", "openai"),
					resource.TestCheckResourceAttr(dataName, "auth_type", "bearer_api_key"),
					resource.TestCheckResourceAttr(dataName, "base_url", "https://upstream.example.com/v1"),
					resource.TestCheckResourceAttr(dataName, "enabled", "true"),
					resource.TestCheckResourceAttr(dataName, "enforce_health_check", "true"),
					resource.TestCheckResourceAttr(dataName, "health_status", "unverified"),
					resource.TestCheckResourceAttr(dataName, "created_at", fakeLlmTime),
					resource.TestCheckNoResourceAttr(dataName, "settings"),
					resource.TestCheckNoResourceAttr(dataName, "health_detail"),
					resource.TestCheckNoResourceAttr(dataName, "health_checked_at"),
					// The seeded provider has a stored credential ("seeded-key");
					// it must not surface anywhere in the data source state.
					resource.TestCheckNoResourceAttr(dataName, "api_key"),
				),
			},
		},
	})
}

func TestLlmProviderDataSource_lookupByName(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	seeded := fake.seedProvider()

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// The name deliberately differs from the seeded one in case:
				// matching mirrors the API's uniqueness rule (a unique index on
				// lower(name)), so this must still resolve.
				Config: `
data "barndoor_llm_provider" "by_name" {
  name = "seeded OPENAI"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.barndoor_llm_provider.by_name", "id", seeded.ID),
					resource.TestCheckResourceAttr("data.barndoor_llm_provider.by_name", "name", "Seeded openai"),
					resource.TestCheckResourceAttr("data.barndoor_llm_provider.by_name", "model_provider", "openai"),
				),
			},
		},
	})
}

func TestLlmProviderDataSource_notFound(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	fake.seedProvider()

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_llm_provider" "missing_name" {
  name = "No Such Provider"
}
`,
				ExpectError: regexp.MustCompile(`No LLM provider named`),
			},
			{
				Config: `
data "barndoor_llm_provider" "missing_id" {
  id = "aaaa0000-0000-0000-0000-999999999999"
}
`,
				ExpectError: regexp.MustCompile(`No LLM provider with id`),
			},
		},
	})
}

func TestLlmProviderDataSource_ambiguousName(t *testing.T) {
	// The platform enforces case-insensitive name uniqueness per organization
	// (providers_org_id_name_unique on lower(name)), so two same-named
	// providers indicate an API contract change. The fake does not enforce the
	// index, letting this test prove the data source refuses to guess.
	fake := setupLlmGatewayTest(t)
	fake.seedProvider()
	fake.seedProvider()

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_llm_provider" "dup" {
  name = "Seeded openai"
}
`,
				// The diagnostic must list the candidate ids so the practitioner
				// can switch to an `id` lookup without a portal round trip.
				ExpectError: regexp.MustCompile(
					`(?s)2 LLM providers matched.*aaaa0000-0000-0000-0000-000000000001.*aaaa0000-0000-0000-0000-000000000002`),
			},
		},
	})
}

func TestLlmProviderDataSource_exactlyOneLookupKey(t *testing.T) {
	setupLlmGatewayTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_llm_provider" "none" {}
`,
				ExpectError: regexp.MustCompile(`Missing Attribute Configuration|Invalid Attribute Combination`),
			},
			{
				Config: `
data "barndoor_llm_provider" "two" {
  id   = "aaaa0000-0000-0000-0000-000000000001"
  name = "Seeded openai"
}
`,
				ExpectError: regexp.MustCompile(`Invalid Attribute Combination`),
			},
		},
	})
}
