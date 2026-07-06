// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	frameworkresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// --- schema tests --------------------------------------------------------------

func TestIdpResource_Metadata(t *testing.T) {
	var resp frameworkresource.MetadataResponse
	NewIdpResource().Metadata(context.Background(),
		frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_idp"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestIdpResource_Schema(t *testing.T) {
	var resp frameworkresource.SchemaResponse
	NewIdpResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{
		"id", "display_name", "issuer", "client_id", "client_secret",
		"authorization_url", "token_url", "userinfo_url", "jwks_url",
		"scopes", "domain", "admin_group",
	} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
	for _, required := range []string{
		"display_name", "issuer", "client_id", "client_secret",
		"authorization_url", "token_url", "jwks_url",
	} {
		if !resp.Schema.Attributes[required].IsRequired() {
			t.Errorf("%s should be Required", required)
		}
	}
	for _, optional := range []string{"userinfo_url", "scopes", "domain", "admin_group"} {
		if !resp.Schema.Attributes[optional].IsOptional() {
			t.Errorf("%s should be Optional", optional)
		}
	}
	if !resp.Schema.Attributes["id"].IsComputed() {
		t.Error("id should be Computed")
	}
	if !resp.Schema.Attributes["client_secret"].IsSensitive() {
		t.Error("client_secret should be Sensitive")
	}
	if resp.Schema.Attributes["client_id"].IsSensitive() {
		t.Error("client_id is not a secret and should not be Sensitive")
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

// idpConfig renders a barndoor_idp block with the required attributes plus
// any extra lines.
func idpConfig(displayName, issuer, clientSecret, extra string) string {
	return fmt.Sprintf(`
resource "barndoor_idp" "test" {
  display_name      = %q
  issuer            = %q
  client_id         = "barndoor-sso"
  client_secret     = %q
  authorization_url = "https://idp.example.com/oauth2/v1/authorize"
  token_url         = "https://idp.example.com/oauth2/v1/token"
  jwks_url          = "https://idp.example.com/oauth2/v1/keys"
%s}
`, displayName, issuer, clientSecret, extra)
}

// checkIdpConnectionDeleted is the CheckDestroy for barndoor_idp tests:
// destroy must remove the connection from the platform.
func checkIdpConnectionDeleted(fake *fakeIdentityServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if fake.connection != nil {
			return fmt.Errorf("IdP connection %q was not deleted on destroy", fake.connection.DisplayName)
		}
		return nil
	}
}

func TestIdpResource_lifecycle(t *testing.T) {
	fake := setupIdentityTest(t)
	const resourceName = "barndoor_idp.test"

	fullExtra := `  userinfo_url      = "https://idp.example.com/oauth2/v1/userinfo"
  scopes            = "openid profile email groups"
  domain            = "acme.com"
  admin_group       = "barndoor-admins"
`

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkIdpConnectionDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: idpConfig("Okta", "https://idp.example.com", "s3cr3t-1", fullExtra),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "id", fakeIdentityIdpAlias),
					resource.TestCheckResourceAttr(resourceName, "display_name", "Okta"),
					resource.TestCheckResourceAttr(resourceName, "issuer", "https://idp.example.com"),
					resource.TestCheckResourceAttr(resourceName, "scopes", "openid profile email groups"),
					resource.TestCheckResourceAttr(resourceName, "domain", "acme.com"),
					resource.TestCheckResourceAttr(resourceName, "admin_group", "barndoor-admins"),
					// Write-only credentials: state follows configuration.
					resource.TestCheckResourceAttr(resourceName, "client_id", "barndoor-sso"),
					resource.TestCheckResourceAttr(resourceName, "client_secret", "s3cr3t-1"),
					// ... and the platform received them.
					func(*terraform.State) error {
						fake.mu.Lock()
						defer fake.mu.Unlock()
						if fake.connection == nil {
							return fmt.Errorf("no connection stored on the fake")
						}
						if fake.connection.ClientSecret != "s3cr3t-1" {
							return fmt.Errorf("fake stored client_secret %q, want %q",
								fake.connection.ClientSecret, "s3cr3t-1")
						}
						if fake.adminGroup != "barndoor-admins" {
							return fmt.Errorf("fake stored admin_group %q, want %q",
								fake.adminGroup, "barndoor-admins")
						}
						return nil
					},
				),
			},
			{
				// A second plan over unchanged configuration must be empty — in
				// particular the write-only credentials must not drift.
				Config:   idpConfig("Okta", "https://idp.example.com", "s3cr3t-1", fullExtra),
				PlanOnly: true,
			},
			{
				// In-place update: new issuer, display name, rotated secret, and
				// a different admin group. The platform mutates the existing
				// Keycloak IdP — nothing is recreated.
				Config: idpConfig("Okta (prod)", "https://sso.example.com", "s3cr3t-2",
					`  userinfo_url      = "https://idp.example.com/oauth2/v1/userinfo"
  scopes            = "openid profile email groups"
  domain            = "acme.com"
  admin_group       = "platform-admins"
`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "id", fakeIdentityIdpAlias),
					resource.TestCheckResourceAttr(resourceName, "display_name", "Okta (prod)"),
					resource.TestCheckResourceAttr(resourceName, "issuer", "https://sso.example.com"),
					resource.TestCheckResourceAttr(resourceName, "client_secret", "s3cr3t-2"),
					resource.TestCheckResourceAttr(resourceName, "admin_group", "platform-admins"),
					func(*terraform.State) error {
						fake.mu.Lock()
						defer fake.mu.Unlock()
						if fake.createCount != 1 {
							return fmt.Errorf("createCount = %d after update, want 1 (update must be in place)",
								fake.createCount)
						}
						if fake.connection.Issuer != "https://sso.example.com" {
							return fmt.Errorf("fake issuer %q not updated", fake.connection.Issuer)
						}
						if fake.connection.ClientSecret != "s3cr3t-2" {
							return fmt.Errorf("fake client_secret %q, want rotated value", fake.connection.ClientSecret)
						}
						return nil
					},
				),
			},
			{
				// Removing admin_group deletes the role mapping (its own PUT);
				// the connection is untouched.
				Config: idpConfig("Okta (prod)", "https://sso.example.com", "s3cr3t-2",
					`  userinfo_url      = "https://idp.example.com/oauth2/v1/userinfo"
  scopes            = "openid profile email groups"
  domain            = "acme.com"
`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(resourceName, "admin_group"),
					func(*terraform.State) error {
						fake.mu.Lock()
						defer fake.mu.Unlock()
						if fake.adminGroup != "" {
							return fmt.Errorf("fake still has admin_group %q, want the mapping deleted",
								fake.adminGroup)
						}
						if fake.createCount != 1 {
							return fmt.Errorf("createCount = %d, want 1", fake.createCount)
						}
						return nil
					},
				),
			},
			{
				Config: idpConfig("Okta (prod)", "https://sso.example.com", "s3cr3t-2",
					`  userinfo_url      = "https://idp.example.com/oauth2/v1/userinfo"
  scopes            = "openid profile email groups"
  domain            = "acme.com"
`),
				PlanOnly: true,
			},
			{
				// Import the singleton by the organization id. The write-only
				// credentials cannot be read back, so they are excluded from
				// the verification.
				Config: idpConfig("Okta (prod)", "https://sso.example.com", "s3cr3t-2",
					`  userinfo_url      = "https://idp.example.com/oauth2/v1/userinfo"
  scopes            = "openid profile email groups"
  domain            = "acme.com"
`),
				ResourceName:            resourceName,
				ImportState:             true,
				ImportStateId:           fakeIdentityOrgID,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"client_id", "client_secret"},
			},
		},
	})
}

func TestIdpResource_adoptsServerDefaults(t *testing.T) {
	fake := setupIdentityTest(t)
	const resourceName = "barndoor_idp.test"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkIdpConnectionDeleted(fake),
		Steps: []resource.TestStep{
			{
				// Only the required attributes: scopes settle on the platform
				// default, userinfo_url and domain stay unset.
				Config: idpConfig("Okta", "https://idp.example.com", "s3cr3t", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "scopes", "openid profile email"),
					resource.TestCheckNoResourceAttr(resourceName, "userinfo_url"),
					resource.TestCheckNoResourceAttr(resourceName, "domain"),
					resource.TestCheckNoResourceAttr(resourceName, "admin_group"),
				),
			},
			{
				Config:   idpConfig("Okta", "https://idp.example.com", "s3cr3t", ""),
				PlanOnly: true,
			},
		},
	})
}

// TestIdpResource_createConflictDirectsToImport covers the singleton
// semantics: when SSO was already configured out-of-band, Create must fail
// with import guidance instead of adopting (or overwriting) the connection.
func TestIdpResource_createConflictDirectsToImport(t *testing.T) {
	fake := setupIdentityTest(t)
	fake.seedIdpConnection(fakeIdpConnection{
		DisplayName:  "Pre-existing SSO",
		Issuer:       "https://old-idp.example.com",
		ClientID:     "old-client",
		ClientSecret: "old-secret",
		AuthzURL:     "https://old-idp.example.com/authorize",
		TokenURL:     "https://old-idp.example.com/token",
		JwksURL:      "https://old-idp.example.com/keys",
		Scopes:       "openid profile email",
	})

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      idpConfig("Okta", "https://idp.example.com", "s3cr3t", ""),
				ExpectError: regexp.MustCompile(`(?s)already exists.*terraform import`),
			},
		},
	})

	// The pre-existing connection must be untouched.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.connection == nil || fake.connection.Issuer != "https://old-idp.example.com" {
		t.Errorf("pre-existing connection was modified: %+v", fake.connection)
	}
}

// TestIdpResource_outOfBandDeleteRecreates covers Read's gone-detection: the
// connection GET stays 200 with a null alias when the IdP was deleted
// out-of-band, and the next apply must recreate it.
func TestIdpResource_outOfBandDeleteRecreates(t *testing.T) {
	fake := setupIdentityTest(t)

	config := idpConfig("Okta", "https://idp.example.com", "s3cr3t", "  admin_group       = \"barndoor-admins\"\n")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkIdpConnectionDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
			},
			{
				PreConfig: func() { fake.markIdpConnectionDeleted(t) },
				Config:    config,
				Check: func(*terraform.State) error {
					fake.mu.Lock()
					defer fake.mu.Unlock()
					if fake.connection == nil {
						return fmt.Errorf("connection was not recreated")
					}
					if fake.createCount != 2 {
						return fmt.Errorf("createCount = %d, want 2 (delete out-of-band, then recreate)",
							fake.createCount)
					}
					if fake.adminGroup != "barndoor-admins" {
						return fmt.Errorf("admin_group %q was not re-applied on recreate", fake.adminGroup)
					}
					return nil
				},
			},
		},
	})
}

// TestIdpResource_permissionDenied covers the 403 diagnostic: the surface
// requires the organization admin role, and the error must say so.
func TestIdpResource_permissionDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			writeToken(w)
			return
		}
		writeJSONDetail(w, http.StatusForbidden, "Insufficient permissions to manage IdP configuration")
	}))
	t.Cleanup(srv.Close)

	t.Setenv("BARNDOOR_BASE_URL", srv.URL)
	t.Setenv("BARNDOOR_TOKEN_URL", srv.URL+"/token")
	t.Setenv("BARNDOOR_CLIENT_ID", "test-client")
	t.Setenv("BARNDOOR_CLIENT_SECRET", "test-secret")
	t.Setenv("BARNDOOR_ORGANIZATION_ID", fakeIdentityOrgID)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      idpConfig("Okta", "https://idp.example.com", "s3cr3t", ""),
				ExpectError: regexp.MustCompile(`(?s)Permission denied.*admin.*role`),
			},
		},
	})
}
