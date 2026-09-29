// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework-jsontypes/jsontypes"
	frameworkresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// --- schema tests --------------------------------------------------------------

func TestLlmProviderResource_Metadata(t *testing.T) {
	var resp frameworkresource.MetadataResponse
	NewLlmProviderResource().Metadata(context.Background(),
		frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_llm_provider"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestLlmProviderResource_Schema(t *testing.T) {
	var resp frameworkresource.SchemaResponse
	NewLlmProviderResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{
		"id", "org_id", "name", "model_provider", "base_url", "connection_id", "auth_type", "api_key",
		"settings", "enabled", "enforce_health_check", "health_status", "health_detail",
		"health_checked_at", "created_at", "updated_at",
	} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
	for _, required := range []string{"name", "model_provider"} {
		if !resp.Schema.Attributes[required].IsRequired() {
			t.Errorf("%s should be Required", required)
		}
	}
	for _, computed := range []string{
		"id", "org_id", "base_url", "auth_type", "settings", "enabled", "enforce_health_check",
		"health_status", "health_detail", "health_checked_at", "created_at", "updated_at",
	} {
		if !resp.Schema.Attributes[computed].IsComputed() {
			t.Errorf("%s should be Computed", computed)
		}
	}
	apiKey := resp.Schema.Attributes["api_key"]
	if !apiKey.IsSensitive() {
		t.Error("api_key should be Sensitive")
	}
	if apiKey.IsComputed() {
		t.Error("api_key is write-only and must not be Computed — the API never returns it")
	}
	if apiKey.GetDeprecationMessage() == "" {
		t.Error("api_key should be deprecated: the platform rejects inline keys since v2.40.0")
	}
}

// --- conversion tests ------------------------------------------------------------

// A bound provider's update carries the connection and never its own
// auth_type: the connection supplies it, and after a rebind it is unknown.
func TestBuildLlmProviderUpdateRequest_BoundToConnection(t *testing.T) {
	plan := &llmProviderResourceModel{
		Name:               types.StringValue("OpenAI"),
		BaseURL:            types.StringValue("https://api.openai.com"),
		ConnectionID:       types.StringValue("cccc0000-0000-0000-0000-000000000001"),
		AuthType:           types.StringValue("bearer_api_key"),
		APIKey:             types.StringNull(),
		Settings:           jsontypes.NewNormalizedNull(),
		Enabled:            types.BoolValue(false),
		EnforceHealthCheck: types.BoolValue(true),
	}
	body, err := buildLlmProviderUpdateRequest(plan)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if v := keys["connection_id"]; string(v) != `"cccc0000-0000-0000-0000-000000000001"` {
		t.Errorf("connection_id = %s, want the planned connection", v)
	}
	for _, absent := range []string{"auth_type", "api_key", "settings"} {
		if _, ok := keys[absent]; ok {
			t.Errorf("%s must be omitted (an omitted key leaves the column unchanged)", absent)
		}
	}
	if v := keys["enabled"]; string(v) != "false" {
		t.Errorf("enabled = %s, want false", v)
	}
}

func TestSettleLlmSettings(t *testing.T) {
	cases := []struct {
		name, server string
		prior        jsontypes.Normalized
		want         string // "" means null
	}{
		{"empty with nothing configured settles to null", `{}`, jsontypes.NewNormalizedNull(), ""},
		{"unconfigured takes the server's object", `{"region":"us-east-1"}`, jsontypes.NewNormalizedNull(), `{"region":"us-east-1"}`},
		{"derived keys do not displace the configured object",
			`{"region":"us-east-1","external_id":"x","model_api_family":"bedrock_converse"}`,
			jsontypes.NewNormalizedValue(`{"region":"us-east-1"}`), `{"region":"us-east-1"}`},
		{"a changed configured key records the server's object", `{"region":"eu-west-1"}`,
			jsontypes.NewNormalizedValue(`{"region":"us-east-1"}`), `{"region":"eu-west-1"}`},
		{"a vanished configured key records the server's object", `{"other":1}`,
			jsontypes.NewNormalizedValue(`{"region":"us-east-1"}`), `{"other":1}`},
		{"nested values compare deeply", `{"a":{"b":[1,2]},"c":true}`,
			jsontypes.NewNormalizedValue(`{"a":{"b":[1,2]}}`), `{"a":{"b":[1,2]}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := settleLlmSettings(json.RawMessage(c.server), c.prior)
			if c.want == "" {
				if !got.IsNull() {
					t.Fatalf("got %s, want null", got.ValueString())
				}
				return
			}
			if got.ValueString() != c.want {
				t.Fatalf("got %s, want %s", got.ValueString(), c.want)
			}
		})
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

// llmConnectionHCL renders a barndoor_llm_connection named name for
// modelProvider with an API key, for provider tests to bind to.
func llmConnectionHCL(name, modelProvider, apiKey string) string {
	return fmt.Sprintf(`
resource "barndoor_llm_connection" %q {
  name           = "%s key"
  model_provider = %q
  base_url       = "https://%s.example.com"
  api_key        = %q
}
`, name, name, modelProvider, name, apiKey)
}

func TestLlmProviderResource_lifecycle(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_provider.test"

	connection := llmConnectionHCL("openai", "openai", "sk-test-1")
	minimalConfig := connection + `
resource "barndoor_llm_provider" "test" {
  name           = "OpenAI"
  model_provider = "openai"
  connection_id  = barndoor_llm_connection.openai.id
}
`
	expandedConfig := connection + `
resource "barndoor_llm_provider" "test" {
  name           = "OpenAI (EU)"
  model_provider = "openai"
  connection_id  = barndoor_llm_connection.openai.id
  base_url       = "https://eu.api.openai.com"
  settings       = jsonencode({ organization = "acme" })

  enabled              = false
  enforce_health_check = false
}
`

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkAllLlmProvidersDeleted(fake),
			checkAllLlmConnectionsDeleted(fake),
		),
		Steps: []resource.TestStep{
			{
				// Minimal provider: base_url follows the connection, auth_type
				// comes from it, and nothing secret is stored on the provider.
				Config: minimalConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckResourceAttr(resourceName, "org_id", fakeLlmOrgID),
					resource.TestCheckResourceAttrPair(resourceName, "connection_id", "barndoor_llm_connection.openai", "id"),
					resource.TestCheckResourceAttr(resourceName, "base_url", "https://openai.example.com"),
					resource.TestCheckResourceAttr(resourceName, "auth_type", "bearer_api_key"),
					resource.TestCheckResourceAttr(resourceName, "enabled", "true"),
					resource.TestCheckResourceAttr(resourceName, "enforce_health_check", "true"),
					resource.TestCheckResourceAttr(resourceName, "health_status", "unverified"),
					resource.TestCheckNoResourceAttr(resourceName, "settings"),
					resource.TestCheckNoResourceAttr(resourceName, "api_key"),
				),
			},
			{
				// A second plan over unchanged configuration must be empty.
				Config:   minimalConfig,
				PlanOnly: true,
			},
			{
				// In-place update: rename, pin an own base_url, add settings,
				// disable the provider and its health gate.
				Config: expandedConfig,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(resourceName, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "name", "OpenAI (EU)"),
					resource.TestCheckResourceAttr(resourceName, "base_url", "https://eu.api.openai.com"),
					resource.TestCheckResourceAttr(resourceName, "enabled", "false"),
					resource.TestCheckResourceAttr(resourceName, "enforce_health_check", "false"),
					resource.TestCheckResourceAttr(resourceName, "settings", `{"organization":"acme"}`),
				),
			},
			{
				Config:   expandedConfig,
				PlanOnly: true,
			},
			{
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

// Rebinding to another connection is in place and takes the new
// connection's auth type; removing connection_id is a replacement, which the
// platform then refuses without a credential.
func TestLlmProviderResource_rebindAndDetach(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_provider.test"

	connections := llmConnectionHCL("primary", "anthropic", "sk-ant-1") + `
resource "barndoor_llm_connection" "gateway" {
  name           = "Anthropic via bearer"
  model_provider = "anthropic"
  auth_type      = "bearer_api_key"
  base_url       = "https://gateway.example.com"
  api_key        = "sk-ant-2"
}
`
	bound := func(conn string) string {
		return connections + fmt.Sprintf(`
resource "barndoor_llm_provider" "test" {
  name           = "Claude"
  model_provider = "anthropic"
  connection_id  = barndoor_llm_connection.%s.id
}
`, conn)
	}

	var firstID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: bound("primary"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "auth_type", "x_api_key"),
					func(s *terraform.State) error {
						firstID = s.RootModule().Resources[resourceName].Primary.ID
						return nil
					},
				),
			},
			{
				Config: bound("gateway"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(resourceName, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(resourceName, "connection_id", "barndoor_llm_connection.gateway", "id"),
					resource.TestCheckResourceAttr(resourceName, "auth_type", "bearer_api_key"),
					func(s *terraform.State) error {
						if got := s.RootModule().Resources[resourceName].Primary.ID; got != firstID {
							return fmt.Errorf("rebind replaced the provider (%s → %s), want in place", firstID, got)
						}
						return nil
					},
				),
			},
			{
				Config:   bound("gateway"),
				PlanOnly: true,
			},
			{
				Config: connections + `
resource "barndoor_llm_provider" "test" {
  name           = "Claude"
  model_provider = "anthropic"
}
`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(resourceName, plancheck.ResourceActionReplace),
					},
				},
				ExpectError: regexp.MustCompile(`connection_id is required`),
			},
		},
	})
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.providers) != 0 {
		t.Fatalf("%d providers left behind", len(fake.providers))
	}
}

// A provider that doesn't set base_url follows its connection's endpoint,
// including when the connection moves.
func TestLlmProviderResource_followsConnectionEndpoint(t *testing.T) {
	setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_provider.test"

	config := func(endpoint string) string {
		return fmt.Sprintf(`
resource "barndoor_llm_connection" "key" {
  name           = "OpenAI key"
  model_provider = "openai"
  base_url       = %q
  api_key        = "sk-test"
}

resource "barndoor_llm_provider" "test" {
  name           = "OpenAI"
  model_provider = "openai"
  connection_id  = barndoor_llm_connection.key.id
}
`, endpoint)
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config("https://api.openai.com"),
				Check:  resource.TestCheckResourceAttr(resourceName, "base_url", "https://api.openai.com"),
			},
			{
				Config: config("https://proxy.example.com"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("barndoor_llm_connection.key", "base_url", "https://proxy.example.com"),
				),
			},
			{
				// The platform retargeted the provider; the refresh picks it
				// up with nothing to plan.
				Config:   config("https://proxy.example.com"),
				PlanOnly: true,
				Check:    resource.TestCheckResourceAttr(resourceName, "base_url", "https://proxy.example.com"),
			},
		},
	})
}

// Request-scoped OAuth passthroughs are the providers that need no
// connection: they store no upstream secret at all.
func TestLlmProviderResource_oauthPassthroughWithoutConnection(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_provider.test"
	config := `
resource "barndoor_llm_provider" "test" {
  name           = "Claude (subscription)"
  model_provider = "anthropic"
  auth_type      = "claude_oauth"
  base_url       = "https://api.anthropic.com"
}
`
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmProvidersDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "auth_type", "claude_oauth"),
					resource.TestCheckNoResourceAttr(resourceName, "connection_id"),
				),
			},
			{Config: config, PlanOnly: true},
		},
	})
}

func TestLlmProviderResource_rejectedConfigurations(t *testing.T) {
	cases := []struct {
		name, config string
		planOnly     bool
		want         *regexp.Regexp
	}{
		{
			// The platform refuses inline keys since v2.40.0; its message
			// must surface verbatim.
			name: "inline api_key",
			config: `
resource "barndoor_llm_provider" "test" {
  name           = "Inline"
  model_provider = "openai"
  base_url       = "https://api.openai.com"
  api_key        = "sk-inline"
}
`,
			want: regexp.MustCompile(`a provider cannot store its own key`),
		},
		{
			name: "api_key with connection_id",
			config: `
resource "barndoor_llm_provider" "test" {
  name           = "Both"
  model_provider = "openai"
  connection_id  = "cccc0000-0000-0000-0000-000000000001"
  api_key        = "sk-inline"
}
`,
			planOnly: true,
			want:     regexp.MustCompile(`Invalid Attribute Combination`),
		},
		{
			name: "auth_type with connection_id",
			config: `
resource "barndoor_llm_provider" "test" {
  name           = "Both"
  model_provider = "openai"
  connection_id  = "cccc0000-0000-0000-0000-000000000001"
  auth_type      = "bearer_api_key"
}
`,
			planOnly: true,
			want:     regexp.MustCompile(`Invalid Attribute Combination`),
		},
		{
			name: "redundant /v1",
			config: `
resource "barndoor_llm_provider" "test" {
  name           = "Versioned"
  model_provider = "openai"
  connection_id  = "cccc0000-0000-0000-0000-000000000001"
  base_url       = "https://api.openai.com/v1/"
}
`,
			planOnly: true,
			want:     regexp.MustCompile(`base_url must not end in /v1[\s\S]*"https://api.openai.com"`),
		},
		{
			name: "non-object settings",
			config: `
resource "barndoor_llm_connection" "key" {
  name           = "OpenAI key"
  model_provider = "openai"
  base_url       = "https://api.openai.com"
  api_key        = "sk-test"
}

resource "barndoor_llm_provider" "test" {
  name           = "Bad settings"
  model_provider = "openai"
  connection_id  = barndoor_llm_connection.key.id
  settings       = jsonencode(["not", "an", "object"])
}
`,
			want: regexp.MustCompile(`settings must be a JSON object`),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setupLlmGatewayTest(t)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{
					{Config: c.config, PlanOnly: c.planOnly, ExpectError: c.want},
				},
			})
		})
	}

	// Families whose base legitimately carries a version (Azure, Bedrock,
	// Vertex, Google AI) are not subject to the rule.
	t.Run("versioned base allowed for azure_openai", func(t *testing.T) {
		setupLlmGatewayTest(t)
		resource.UnitTest(t, resource.TestCase{
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
			Steps: []resource.TestStep{
				{
					Config: `
resource "barndoor_llm_provider" "test" {
  name           = "Azure"
  model_provider = "azure_openai"
  connection_id  = "cccc0000-0000-0000-0000-000000000001"
  base_url       = "https://acme.openai.azure.com/openai/v1"
}
`,
					PlanOnly:           true,
					ExpectNonEmptyPlan: true,
				},
			},
		})
	})
}

func TestLlmProviderResource_disabledOnCreateConverges(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_provider.test"

	// The create endpoint has no enabled field; the resource must converge
	// enabled = false with a follow-up update in the same apply.
	config := llmConnectionHCL("anthropic", "anthropic", "sk-ant-test") + `
resource "barndoor_llm_provider" "test" {
  name           = "Staged"
  model_provider = "anthropic"
  connection_id  = barndoor_llm_connection.anthropic.id
  enabled        = false
}
`

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllLlmProvidersDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "enabled", "false"),
					resource.TestCheckResourceAttr(resourceName, "auth_type", "x_api_key"),
					func(*terraform.State) error {
						fake.mu.Lock()
						defer fake.mu.Unlock()
						if len(fake.providers) != 1 {
							return fmt.Errorf("expected 1 provider, have %d", len(fake.providers))
						}
						if fake.providers[0].Enabled {
							return fmt.Errorf("provider is still enabled on the platform")
						}
						return nil
					},
				),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
		},
	})
}

func TestLlmProviderResource_outOfBandDeletePlansRecreate(t *testing.T) {
	fake := setupLlmGatewayTest(t)
	const resourceName = "barndoor_llm_provider.test"

	config := llmConnectionHCL("openai", "openai", "sk-oob") + `
resource "barndoor_llm_provider" "test" {
  name           = "tf-llm-oob"
  model_provider = "openai"
  connection_id  = barndoor_llm_connection.openai.id
}
`

	var firstID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: func(s *terraform.State) error {
					rs, ok := s.RootModule().Resources[resourceName]
					if !ok {
						return fmt.Errorf("%s not in state", resourceName)
					}
					firstID = rs.Primary.ID
					return nil
				},
			},
			{
				// Simulate an out-of-band delete; the refresh must drop the
				// resource and the apply must create a replacement.
				PreConfig: func() { fake.markProviderDeleted(t, firstID) },
				Config:    config,
				Check: func(s *terraform.State) error {
					rs, ok := s.RootModule().Resources[resourceName]
					if !ok {
						return fmt.Errorf("%s not in state", resourceName)
					}
					if rs.Primary.ID == firstID {
						return fmt.Errorf("expected a new provider id after out-of-band delete, still %s", firstID)
					}
					return nil
				},
			},
		},
	})
}
