// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"testing"

	frameworkresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const llmConnectionResourceName = "barndoor_llm_connection.test"

func TestLlmConnectionResource_Schema(t *testing.T) {
	var meta frameworkresource.MetadataResponse
	NewLlmConnectionResource().Metadata(context.Background(),
		frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &meta)
	if got, want := meta.TypeName, "barndoor_llm_connection"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}

	var resp frameworkresource.SchemaResponse
	NewLlmConnectionResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}
	for _, secret := range []string{"api_key", "credentials"} {
		attr := resp.Schema.Attributes[secret]
		if !attr.IsSensitive() {
			t.Errorf("%s should be Sensitive", secret)
		}
		if attr.IsComputed() {
			t.Errorf("%s is write-only and must not be Computed — the API never returns it", secret)
		}
	}
}

func TestLlmConnectionResource_lifecycle(t *testing.T) {
	fake := setupLlmGatewayTest(t)

	config := func(name, key string) string {
		return fmt.Sprintf(`
resource "barndoor_llm_connection" "test" {
  name           = %q
  model_provider = "anthropic"
  base_url       = "https://api.anthropic.com"
  api_key        = %q
}
`, name, key)
	}

	var connID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmConnectionsDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: config("Anthropic", "sk-ant-first-1111"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(llmConnectionResourceName, "id"),
					resource.TestCheckResourceAttr(llmConnectionResourceName, "org_id", fakeLlmOrgID),
					resource.TestCheckResourceAttr(llmConnectionResourceName, "auth_type", "x_api_key"),
					resource.TestCheckResourceAttr(llmConnectionResourceName, "key_last4", "1111"),
					resource.TestCheckResourceAttr(llmConnectionResourceName, "stores_key_material", "true"),
					resource.TestCheckNoResourceAttr(llmConnectionResourceName, "settings"),
					resource.TestCheckResourceAttr(llmConnectionResourceName, "effective_settings", "{}"),
					func(s *terraform.State) error {
						connID = s.RootModule().Resources[llmConnectionResourceName].Primary.ID
						if got := fake.connectionSecret(t, connID); got != "sk-ant-first-1111" {
							return fmt.Errorf("stored secret = %q, want the configured key", got)
						}
						return nil
					},
				),
			},
			{
				Config:   config("Anthropic", "sk-ant-first-1111"),
				PlanOnly: true,
			},
			{
				// Rotating the key and renaming are both in place.
				Config: config("Anthropic (prod)", "sk-ant-second-2222"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(llmConnectionResourceName, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(llmConnectionResourceName, "name", "Anthropic (prod)"),
					resource.TestCheckResourceAttr(llmConnectionResourceName, "key_last4", "2222"),
					func(*terraform.State) error {
						if got := fake.connectionSecret(t, connID); got != "sk-ant-second-2222" {
							return fmt.Errorf("stored secret = %q, want the rotated key", got)
						}
						return nil
					},
				),
			},
			{
				// The secret is write-only, so import cannot fill it.
				ResourceName:            llmConnectionResourceName,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"api_key"},
			},
		},
	})
}

// An aws_role Bedrock connection: the platform generates the trust policy's
// external_id and defaults model_api_family. Neither may produce a diff
// against the configured settings, and both must be visible in
// effective_settings.
func TestLlmConnectionResource_derivedSettings(t *testing.T) {
	fake := setupLlmGatewayTest(t)

	config := func(region string) string {
		return fmt.Sprintf(`
resource "barndoor_llm_connection" "test" {
  name           = "Bedrock role"
  model_provider = "bedrock"
  base_url       = "https://bedrock-runtime.%s.amazonaws.com"
  settings = jsonencode({
    region       = %q
    iam_role_arn = "arn:aws:iam::123456789012:role/barndoor"
  })
}

output "external_id" {
  value = jsondecode(barndoor_llm_connection.test.effective_settings).external_id
}
`, region, region)
	}

	var externalID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmConnectionsDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: config("us-east-1"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(llmConnectionResourceName, "auth_type", "aws_role"),
					resource.TestCheckResourceAttr(llmConnectionResourceName, "stores_key_material", "false"),
					resource.TestCheckNoResourceAttr(llmConnectionResourceName, "key_last4"),
					resource.TestCheckResourceAttr(llmConnectionResourceName, "settings",
						`{"iam_role_arn":"arn:aws:iam::123456789012:role/barndoor","region":"us-east-1"}`),
					func(s *terraform.State) error {
						out, ok := s.RootModule().Outputs["external_id"]
						if !ok || out.Value == "" {
							return fmt.Errorf("external_id output is empty")
						}
						externalID, _ = out.Value.(string)
						var eff map[string]any
						raw := s.RootModule().Resources[llmConnectionResourceName].Primary.Attributes["effective_settings"]
						if err := json.Unmarshal([]byte(raw), &eff); err != nil {
							return err
						}
						if eff["model_api_family"] != "bedrock_converse" {
							return fmt.Errorf("effective_settings = %s, want the defaulted model_api_family", raw)
						}
						return nil
					},
				),
			},
			{
				Config:   config("us-east-1"),
				PlanOnly: true,
			},
			{
				// Changing a configured key is an in-place update that keeps
				// the generated external_id (the trust policy stays valid).
				Config: config("eu-west-1"),
				Check: func(s *terraform.State) error {
					if got := s.RootModule().Outputs["external_id"].Value; got != externalID {
						return fmt.Errorf("external_id changed on update: %v → %v", externalID, got)
					}
					return nil
				},
			},
			{
				Config:   config("eu-west-1"),
				PlanOnly: true,
			},
		},
	})
}

func TestLlmConnectionResource_deleteRefusedWhileInUse(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	config := `
resource "barndoor_llm_connection" "test" {
  name           = "Shared key"
  model_provider = "openai"
  base_url       = "https://api.openai.com"
  api_key        = "sk-shared"
}
`
	removed := `# connection removed from configuration`

	var connID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmConnectionsDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: func(s *terraform.State) error {
					connID = s.RootModule().Resources[llmConnectionResourceName].Primary.ID
					return nil
				},
			},
			{
				// A provider managed elsewhere still reads its key from the
				// connection: the platform's 409 must surface.
				PreConfig: func() {
					p := fake.seedProvider()
					fake.mu.Lock()
					defer fake.mu.Unlock()
					p.Name = "App-managed provider"
					p.ConnectionID = &connID
				},
				Config:      removed,
				ExpectError: regexp.MustCompile(`in use by 1 provider\(s\): App-managed provider`),
			},
			{
				PreConfig: func() {
					fake.mu.Lock()
					defer fake.mu.Unlock()
					fake.providers = nil
				},
				Config: removed,
			},
		},
	})
}

func TestLlmConnectionResource_rejectedAtPlan(t *testing.T) {
	cases := []struct {
		name, body string
		want       *regexp.Regexp
	}{
		{
			name: "redundant /v1",
			body: `
  model_provider = "openai"
  base_url       = "https://api.openai.com/v1"
  api_key        = "sk"`,
			want: regexp.MustCompile(`base_url must not end in /v1`),
		},
		{
			name: "api_key and credentials",
			body: `
  model_provider = "bedrock"
  auth_type      = "aws_static_credentials"
  base_url       = "https://bedrock-runtime.us-east-1.amazonaws.com"
  api_key        = "sk"
  credentials    = jsonencode({ access_key_id = "AKIA", secret_access_key = "s" })`,
			want: regexp.MustCompile(`Invalid Attribute Combination`),
		},
		{
			name: "unknown model_provider",
			body: `
  model_provider = "openia"
  base_url       = "https://api.openai.com"
  api_key        = "sk"`,
			want: regexp.MustCompile(`value must be one of`),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := setupLlmGatewayTest(t)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{
					{
						Config:      fmt.Sprintf("resource \"barndoor_llm_connection\" \"test\" {\n  name = \"Rejected\"%s\n}\n", c.body),
						PlanOnly:    true,
						ExpectError: c.want,
					},
				},
			})
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if len(fake.connections) != 0 {
				t.Fatal("a rejected configuration reached the API")
			}
		})
	}
}

// Structured credentials round-trip as write-only too.
func TestLlmConnectionResource_structuredCredentials(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmConnectionsDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: `
resource "barndoor_llm_connection" "test" {
  name           = "Bedrock static"
  model_provider = "bedrock"
  auth_type      = "aws_static_credentials"
  base_url       = "https://bedrock-runtime.us-east-1.amazonaws.com"
  settings       = jsonencode({ region = "us-east-1" })
  credentials    = jsonencode({ access_key_id = "AKIAEXAMPLE", secret_access_key = "wJalr" })
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(llmConnectionResourceName, "auth_type", "aws_static_credentials"),
					func(s *terraform.State) error {
						id := s.RootModule().Resources[llmConnectionResourceName].Primary.ID
						if got := fake.connectionSecret(t, id); got == "" {
							return fmt.Errorf("structured credentials did not reach the platform")
						}
						return nil
					},
				),
			},
		},
	})
}
