// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

// Acceptance tests for the Barndoor provider.
//
// # Running
//
// These tests are gated on TF_ACC (the standard Terraform acceptance-test
// switch) AND the BARNDOOR_* connection environment variables. With either
// unset they skip cleanly, so `go test ./...` and the CI test job (which sets
// TF_ACC=1 but not the credentials) never reach a live backend.
//
// To run them, point the provider at a reachable Barndoor environment and a
// service-account credential scoped to the organization under test:
//
//	export TF_ACC=1
//	export BARNDOOR_BASE_URL=https://platform.barndoor.ai
//	export BARNDOOR_TOKEN_URL=https://auth.barndoor.ai/realms/barndoor/protocol/openid-connect/token
//	export BARNDOOR_CLIENT_ID=...            # a client_credentials client
//	export BARNDOOR_CLIENT_SECRET=...        # the client's secret
//	export BARNDOOR_ORGANIZATION_ID=...      # the org the credential is scoped to
//	go test ./internal/provider/ -run TestAcc -v
//
// # Safety: do not clobber a real export
//
// An organization's audit-log export may be actively delivering. The write
// tests must never create, modify, or delete the export configuration of an
// organization you care about.
//
// Therefore:
//
//   - TestAccConnectivity is READ-ONLY (a single GET) and safe to run against
//     any org: it proves a client_credentials token mints and the
//     system-management public read path is reachable and authorized.
//
//   - The write tests (TestAccLogExportResource_lifecycle,
//     TestAccLogExportResource_azureLifecycle, and
//     TestAccLogExportAWSTrustInfoDataSource — the last mints/persists an
//     external ID) only run when BARNDOOR_ACC_TEST_ORGANIZATION_ID names a
//     DISPOSABLE test org whose export configuration may be freely changed.
//     They skip with an explicit reason otherwise. As an extra guard, set
//     BARNDOOR_ACC_PROTECTED_ORGANIZATION_ID to an organization that must never
//     be touched (e.g. a production org) and the write tests hard-fail if the
//     disposable-org variable is ever pointed at it.
//
//   - TestAccLogExportResource_azureLifecycle additionally needs
//     BARNDOOR_ACC_TEST_AZURE_BLOB, because Azure destinations are gated per
//     organization by the log-export-azure-blob feature flag and the API
//     answers 403 while it is off.
//
//   - TestAccPolicyResource_lifecycle additionally needs
//     BARNDOOR_TEST_MCP_SERVER_ID (a real MCP server id in the credential's
//     org) and skips without it. It only touches policies it creates itself
//     (timestamped tf-acc-policy-* names); destroy archives them.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

const (
	// envTestOrgID opts in to the destructive (write) acceptance tests by
	// naming a disposable org whose export config may be freely changed.
	envTestOrgID = "BARNDOOR_ACC_TEST_ORGANIZATION_ID"

	// envProtectedOrgID optionally names an organization that must never be used
	// for a destructive test (e.g. a production org). When set, the write tests
	// hard-fail if envTestOrgID is pointed at it.
	envProtectedOrgID = "BARNDOOR_ACC_PROTECTED_ORGANIZATION_ID"

	// envTestAzureBlob opts in to the azure_blob log-export acceptance test.
	// Azure destinations are gated per organization by a platform feature flag
	// (log-export-azure-blob); with it off the API answers 403 and the test
	// would fail for a reason that has nothing to do with the provider. Set it
	// to any non-empty value once the flag is enabled for the disposable test
	// org named by envTestOrgID.
	envTestAzureBlob = "BARNDOOR_ACC_TEST_AZURE_BLOB"

	// envTestMCPServerID opts in to the barndoor_policy acceptance test by
	// naming a real MCP server in the credential's organization for the
	// policy's required (and immutable) mcp_server_id.
	envTestMCPServerID = "BARNDOOR_TEST_MCP_SERVER_ID"

	// envTestMCPServerDirectoryID opts in to the barndoor_mcp_server
	// acceptance test by naming a directory entry (public catalog or
	// org-owned) the test server instantiates. The test only touches servers
	// it creates itself (timestamped tf-acc-server-* names) and soft-deletes
	// them on destroy.
	envTestMCPServerDirectoryID = "BARNDOOR_TEST_MCP_SERVER_DIRECTORY_ID"

	// envTestApplicationDirectoryID opts in to the barndoor_agent acceptance
	// test by naming an agent directory entry that is visible to — and not
	// already registered in — the credential's organization. The test
	// registers and unregisters that one entry.
	envTestApplicationDirectoryID = "BARNDOOR_TEST_APPLICATION_DIRECTORY_ID"
)

// connEnv lists the connection variables every acceptance test needs.
var connEnv = []string{
	"BARNDOOR_BASE_URL",
	"BARNDOOR_TOKEN_URL",
	"BARNDOOR_CLIENT_ID",
	"BARNDOOR_CLIENT_SECRET",
	"BARNDOOR_ORGANIZATION_ID",
}

// testAccProtoV6ProviderFactories builds the in-process provider server the
// terraform-plugin-testing harness drives. The provider reads its connection
// settings from the BARNDOOR_* environment variables.
var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"barndoor": providerserver.NewProtocol6WithError(New("test")()),
}

// testAccPreCheck skips the test unless the full BARNDOOR_* connection
// environment is present. (resource.TestCase additionally requires TF_ACC.)
func testAccPreCheck(t *testing.T) {
	t.Helper()
	for _, env := range connEnv {
		if os.Getenv(env) == "" {
			t.Skipf("%s not set; skipping acceptance test (set TF_ACC and the BARNDOOR_* connection env to run)", env)
		}
	}
}

// requireDisposableTestOrg returns the disposable test org for a destructive
// test, or skips. If BARNDOOR_ACC_PROTECTED_ORGANIZATION_ID is set, it hard-fails
// when the opt-in variable is pointed at that org, so a misconfiguration can
// never clobber an organization you have marked off-limits.
func requireDisposableTestOrg(t *testing.T) string {
	t.Helper()
	org := os.Getenv(envTestOrgID)
	if org == "" {
		t.Skipf("%s not set: refusing to run a destructive (write) acceptance test, which would clobber a "+
			"real export configuration. Set it to a DISPOSABLE test org (whose export config may be freely "+
			"changed) backed by a credential scoped to that org. See the file header in acceptance_test.go for details.", envTestOrgID)
	}
	if protected := os.Getenv(envProtectedOrgID); protected != "" && org == protected {
		t.Fatalf("%s is set to the protected org named by %s, which must never be used for a destructive "+
			"acceptance test. Refusing to run.", envTestOrgID, envProtectedOrgID)
	}
	return org
}

// TestAccConnectivity is a read-only smoke test: it mints a client_credentials
// token and reads the configured org's export over the SMS public API path. It
// never mutates anything, so it is safe to run against any org, and it proves
// the auth + read path end to end (token mint + authorized read).
func TestAccConnectivity(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set; skipping acceptance test")
	}
	testAccPreCheck(t)

	c := client.New(client.Config{
		BaseURL:        os.Getenv("BARNDOOR_BASE_URL"),
		TokenURL:       os.Getenv("BARNDOOR_TOKEN_URL"),
		ClientID:       os.Getenv("BARNDOOR_CLIENT_ID"),
		ClientSecret:   os.Getenv("BARNDOOR_CLIENT_SECRET"),
		OrganizationID: os.Getenv("BARNDOOR_ORGANIZATION_ID"),
	})

	// GET .../exports/{org}/{exportType} is purely read-only. Calling Do mints
	// the token, so a successful (or authorized-but-404) response proves the
	// whole auth + edge read path end to end.
	path := exportPath(c.OrganizationID(), defaultExportType)
	resp, err := c.Do(context.Background(), http.MethodGet, path, nil)
	if err != nil {
		t.Fatalf("connectivity check failed (token mint or request): %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		// Token minted and the authorized edge read worked; the org just has no
		// export row provisioned. Connectivity is still verified.
		t.Logf("connectivity OK: authorized, but no %q export provisioned for org %s", defaultExportType, c.OrganizationID())
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		t.Logf("connectivity OK: authorized read of %q export for org %s", defaultExportType, c.OrganizationID())
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		t.Fatalf("connectivity check failed: GET %s -> %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

// TestAccLogExportResource_lifecycle exercises create → update → import →
// destroy against a disposable dev org. Destructive: gated on a disposable test
// org (see requireDisposableTestOrg).
func TestAccLogExportResource_lifecycle(t *testing.T) {
	testOrg := requireDisposableTestOrg(t)
	const resourceName = "barndoor_log_export.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Create: configure a destination + settings, streaming disabled
				// (enabled=true would trigger the server's connectivity probe
				// against the bucket, which a placeholder destination fails).
				Config: testAccLogExportConfig(testOrg, 100),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "organization_id", testOrg),
					resource.TestCheckResourceAttr(resourceName, "export_type", defaultExportType),
					resource.TestCheckResourceAttr(resourceName, "enabled", "false"),
					resource.TestCheckResourceAttr(resourceName, "destination.bucket", "barndoor-acc-test-disposable"),
					resource.TestCheckResourceAttr(resourceName, "destination.auth_method", authMethodAccessKeys),
					resource.TestCheckResourceAttr(resourceName, "destination.has_credentials", "true"),
					resource.TestCheckResourceAttr(resourceName, "settings.batch_size", "100"),
				),
			},
			{
				// Update: change a setting in place.
				Config: testAccLogExportConfig(testOrg, 250),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "settings.batch_size", "250"),
				),
			},
			{
				// Import: secrets are config-only (never returned by the API), so
				// they read back null on import and are excluded from the diff.
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateId:     fmt.Sprintf("%s/%s", testOrg, defaultExportType),
				ImportStateVerify: true,
				// This resource is an org-singleton with no "id" attribute; its
				// identifier is organization_id (the framework defaults to "id").
				ImportStateVerifyIdentifierAttribute: "organization_id",
				ImportStateVerifyIgnore: []string{
					"destination.access_key_id",
					"destination.secret_access_key",
				},
			},
		},
	})
}

// TestAccLogExportResource_azureLifecycle exercises an azure_blob destination
// end to end (create → update → import → destroy) against a disposable dev org.
// Destructive: gated on a disposable test org, and additionally on
// BARNDOOR_ACC_TEST_AZURE_BLOB because Azure destinations are feature-flagged
// per organization.
//
// The interesting assertion is the plan itself: with `provider = "azure_blob"`
// the S3-only attributes are never written, so `use_ssl` comes from the schema
// default (true) while the API reports false for every Azure row. A create step
// that leaves a non-empty plan behind fails the harness, which is what pins the
// state-mapping behaviour end to end.
func TestAccLogExportResource_azureLifecycle(t *testing.T) {
	testOrg := requireDisposableTestOrg(t)
	if os.Getenv(envTestAzureBlob) == "" {
		t.Skipf("%s not set; skipping the azure_blob log-export acceptance test. Azure destinations are "+
			"gated by the log-export-azure-blob feature flag — set this once the flag is enabled for the "+
			"disposable test org.", envTestAzureBlob)
	}
	const resourceName = "barndoor_log_export.test_azure"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Create: streaming stays disabled so the placeholder container
				// is stored but never probed.
				Config: testAccLogExportAzureConfig(testOrg, "barndoor/"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "organization_id", testOrg),
					resource.TestCheckResourceAttr(resourceName, "enabled", "false"),
					resource.TestCheckResourceAttr(resourceName, "destination.provider", storageProviderAzureBlob),
					resource.TestCheckResourceAttr(resourceName, "destination.bucket", "barndoor-acc-test-disposable"),
					resource.TestCheckResourceAttr(resourceName, "destination.auth_method", authMethodAccountKey),
					resource.TestCheckResourceAttr(resourceName, "destination.path_prefix", "barndoor/"),
					resource.TestCheckResourceAttr(resourceName, "destination.has_credentials", "true"),
					// The S3-only attributes keep their schema defaults rather
					// than the empty values the API reports for an Azure row.
					resource.TestCheckResourceAttr(resourceName, "destination.use_ssl", "true"),
					resource.TestCheckResourceAttr(resourceName, "destination.use_path_style", "false"),
					resource.TestCheckNoResourceAttr(resourceName, "destination.region"),
					resource.TestCheckNoResourceAttr(resourceName, "destination.iam_role_arn"),
					resource.TestCheckNoResourceAttr(resourceName, "destination.external_id"),
				),
			},
			{
				// Update: change the prefix in place; the destination is
				// rewritten with the same provider and auth method.
				Config: testAccLogExportAzureConfig(testOrg, "barndoor/audit/"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "destination.path_prefix", "barndoor/audit/"),
					resource.TestCheckResourceAttr(resourceName, "destination.use_ssl", "true"),
				),
			},
			{
				// Import: the account key is config-only (never returned by the
				// API), so it reads back null and is excluded from the diff.
				ResourceName:                         resourceName,
				ImportState:                          true,
				ImportStateId:                        fmt.Sprintf("%s/%s", testOrg, defaultExportType),
				ImportStateVerify:                    true,
				ImportStateVerifyIdentifierAttribute: "organization_id",
				ImportStateVerifyIgnore: []string{
					"destination.account_key",
				},
			},
		},
	})
}

// testAccLogExportAzureConfig renders an azure_blob log-export resource for the
// disposable org. The account key is Azurite's published emulator key — a
// documented public placeholder, stored but never probed while streaming is
// disabled. Note what the config does NOT set: region, use_ssl, use_path_style,
// iam_role_arn, and the S3 keys are all rejected by the API for Azure.
func testAccLogExportAzureConfig(orgID, pathPrefix string) string {
	return fmt.Sprintf(`
resource "barndoor_log_export" "test_azure" {
  organization_id = %[1]q

  destination = {
    provider    = "azure_blob"
    endpoint    = "https://barndooracctest.blob.core.windows.net"
    bucket      = "barndoor-acc-test-disposable"
    path_prefix = %[2]q
    auth_method = "account_key"
    account_key = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
  }

  enabled = false
}
`, orgID, pathPrefix)
}

// TestAccLogExportAWSTrustInfoDataSource reads the trust-info data source.
// Destructive-ish: the read mints and persists an external ID on the
// destination, and requires the iam_role feature to be enabled for the org, so
// it is gated on a disposable test org. (Lives here so the acceptance suite is
// in one place; the data source itself ships in the trust-info PR.)
func TestAccLogExportAWSTrustInfoDataSource(t *testing.T) {
	testOrg := requireDisposableTestOrg(t)
	const dataName = "data.barndoor_log_export_aws_trust_info.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccTrustInfoConfig(testOrg),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(dataName, "organization_id", testOrg),
					resource.TestCheckResourceAttr(dataName, "export_type", defaultExportType),
					resource.TestCheckResourceAttrSet(dataName, "principal_arn"),
					resource.TestCheckResourceAttrSet(dataName, "external_id"),
				),
			},
		},
	})
}

// TestAccPolicyResource_lifecycle exercises the barndoor_policy resource
// end to end (create → update → import → destroy) against a real environment.
// Destroy ARCHIVES the policy (the platform never hard-deletes), so each run
// leaves one archived tf-acc-policy-* row behind; the timestamped name keeps
// runs from colliding with prior archives' live siblings.
func TestAccPolicyResource_lifecycle(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set; skipping acceptance test")
	}
	testAccPreCheck(t)

	mcpServerID := os.Getenv(envTestMCPServerID)
	if mcpServerID == "" {
		t.Skipf("%s not set; skipping the barndoor_policy acceptance test. Set it to the id of an MCP "+
			"server in the credential's organization — the policy's required, immutable mcp_server_id.", envTestMCPServerID)
	}

	name := fmt.Sprintf("tf-acc-policy-%d", time.Now().UnixNano())
	const resourceName = "barndoor_policy.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Create with no status (server defaults to DRAFT) and one
				// ALLOW rule.
				Config: testAccPolicyConfig(mcpServerID, name, "DRAFT", 100),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckResourceAttr(resourceName, "name", name),
					resource.TestCheckResourceAttr(resourceName, "mcp_server_id", mcpServerID),
					resource.TestCheckResourceAttr(resourceName, "status", "DRAFT"),
					resource.TestCheckResourceAttr(resourceName, "rules.#", "1"),
					resource.TestCheckResourceAttr(resourceName, "rules.0.effect", "ALLOW"),
					resource.TestCheckResourceAttr(resourceName, "rules.0.active", "true"),
				),
			},
			{
				// Read the created policy back through the data source, by name.
				Config: testAccPolicyConfig(mcpServerID, name, "DRAFT", 100) + `
data "barndoor_policy" "by_name" {
  name = barndoor_policy.test.name
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("data.barndoor_policy.by_name", "id", resourceName, "id"),
					resource.TestCheckResourceAttr("data.barndoor_policy.by_name", "rules.#", "1"),
				),
			},
			{
				// In-place update: activate and change the rule's condition.
				Config: testAccPolicyConfig(mcpServerID, name, "ACTIVE", 250),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "status", "ACTIVE"),
				),
			},
			{
				// Import by policy id.
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

// testAccPolicyConfig renders a barndoor_policy with one conditional ALLOW
// rule. The condition is written in the canonical compact JSON form so the
// import-verify step compares byte-identical strings.
func testAccPolicyConfig(mcpServerID, name, status string, sizeLimit int) string {
	return fmt.Sprintf(`
resource "barndoor_policy" "test" {
  name          = %[1]q
  mcp_server_id = %[2]q
  description   = "Terraform acceptance test policy"
  status        = %[3]q
  tags          = ["tf-acc"]

  rules = [{
    name      = "allow small reads"
    effect    = "ALLOW"
    actions   = ["*"]
    roles     = ["*"]
    condition = "{\"expr\":{\"expr\":\"request.size < %[4]d\"}}"
  }]
}
`, name, mcpServerID, status, sizeLimit)
}

// testAccLogExportConfig renders a log-export resource for the disposable org.
// The provider self-configures from the BARNDOOR_* environment. The S3 values
// are placeholders (AWS's documented example access key) — streaming stays
// disabled so they are stored but never probed.
func testAccLogExportConfig(orgID string, batchSize int) string {
	return fmt.Sprintf(`
resource "barndoor_log_export" "test" {
  organization_id = %[1]q

  destination = {
    endpoint          = "https://s3.us-east-1.amazonaws.com"
    region            = "us-east-1"
    bucket            = "barndoor-acc-test-disposable"
    auth_method       = "access_keys"
    access_key_id     = "AKIAIOSFODNN7EXAMPLE"
    secret_access_key = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
  }

  settings = {
    batch_size = %[2]d
  }

  enabled = false
}
`, orgID, batchSize)
}

func testAccTrustInfoConfig(orgID string) string {
	return fmt.Sprintf(`
data "barndoor_log_export_aws_trust_info" "test" {
  organization_id = %[1]q
}
`, orgID)
}

// TestAccMcpServerResource_lifecycle exercises the barndoor_mcp_server
// resource end to end (create → update → import → destroy) against a real
// environment. Destroy soft-deletes the server (freeing its name/slug), so
// runs are self-cleaning; the timestamped name keeps concurrent runs apart.
func TestAccMcpServerResource_lifecycle(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set; skipping acceptance test")
	}
	testAccPreCheck(t)

	directoryID := os.Getenv(envTestMCPServerDirectoryID)
	if directoryID == "" {
		t.Skipf("%s not set; skipping the barndoor_mcp_server acceptance test. Set it to the id of an MCP "+
			"server directory entry visible to the credential's organization.", envTestMCPServerDirectoryID)
	}

	name := fmt.Sprintf("tf-acc-server-%d", time.Now().UnixNano())
	const resourceName = "barndoor_mcp_server.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Create without credentials: the server stays pending and no
				// upstream OAuth flow is triggered.
				Config: testAccMcpServerConfig(directoryID, name, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckResourceAttrSet(resourceName, "slug"),
					resource.TestCheckResourceAttr(resourceName, "name", name),
					resource.TestCheckResourceAttr(resourceName, "mcp_server_directory_id", directoryID),
					resource.TestCheckResourceAttrSet(resourceName, "status"),
				),
			},
			{
				// Read the created server back through the data source, by id
				// and by slug.
				Config: testAccMcpServerConfig(directoryID, name, "") + `
data "barndoor_mcp_server" "by_id" {
  id = barndoor_mcp_server.test.id
}

data "barndoor_mcp_server" "by_slug" {
  slug = barndoor_mcp_server.test.slug
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("data.barndoor_mcp_server.by_id", "id", resourceName, "id"),
					resource.TestCheckResourceAttrPair("data.barndoor_mcp_server.by_slug", "id", resourceName, "id"),
				),
			},
			{
				// In-place update: rename and add a scope override.
				Config: testAccMcpServerConfig(directoryID, name+"-renamed", "\n  scopes = [\"read\"]\n"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "name", name+"-renamed"),
					resource.TestCheckResourceAttr(resourceName, "scopes.#", "1"),
				),
			},
			{
				// Import by server id. Write-only attributes read back null.
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateVerifyIgnore: []string{
					"client_id", "client_secret", "meta", "prepopulated_credentials", "cascaded_fields",
				},
			},
		},
	})
}

func testAccMcpServerConfig(directoryID, name, extra string) string {
	return fmt.Sprintf(`
resource "barndoor_mcp_server" "test" {
  name                    = %[1]q
  mcp_server_directory_id = %[2]q
%[3]s
}
`, name, directoryID, extra)
}

// TestAccAgentResource_lifecycle exercises the barndoor_agent resource end to
// end (register → toggle flags → import → unregister) against a real
// environment. Destroy soft-deletes the registration (freeing the directory
// for re-registration), so runs are self-cleaning. Note the platform allows
// one live registration per directory entry — point the env var at a
// directory that is NOT already registered in the test org.
func TestAccAgentResource_lifecycle(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set; skipping acceptance test")
	}
	testAccPreCheck(t)

	directoryID := os.Getenv(envTestApplicationDirectoryID)
	if directoryID == "" {
		t.Skipf("%s not set; skipping the barndoor_agent acceptance test. Set it to the id of an agent "+
			"directory entry that is visible to (and not already registered in) the credential's organization.",
			envTestApplicationDirectoryID)
	}

	const resourceName = "barndoor_agent.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccAgentConfig(directoryID, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckResourceAttr(resourceName, "application_directory_id", directoryID),
					resource.TestCheckResourceAttr(resourceName, "write_confirmations_required", "true"),
					resource.TestCheckResourceAttr(resourceName, "llm_gateway_enabled", "false"),
					resource.TestCheckResourceAttrSet(resourceName, "name"),
					resource.TestCheckResourceAttrSet(resourceName, "agent_type"),
				),
			},
			{
				// Read the registration back through the data source, by id.
				Config: testAccAgentConfig(directoryID, "") + `
data "barndoor_agent" "by_id" {
  id = barndoor_agent.test.id
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("data.barndoor_agent.by_id", "id", resourceName, "id"),
					resource.TestCheckResourceAttrPair("data.barndoor_agent.by_id", "name", resourceName, "name"),
				),
			},
			{
				// Toggle the per-agent flags in place.
				Config: testAccAgentConfig(directoryID, "\n  write_confirmations_required = false\n"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "write_confirmations_required", "false"),
				),
			},
			{
				// Import by registration id; every attribute is
				// server-authoritative so no ignores are needed.
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func testAccAgentConfig(directoryID, extra string) string {
	return fmt.Sprintf(`
resource "barndoor_agent" "test" {
  application_directory_id = %[1]q
%[2]s
}
`, directoryID, extra)
}

// TestAccNotificationChannelResource_lifecycle exercises barndoor_notification_channel
// against a live notification-service (BCP-3760).
//
// Destructive, so it is gated on the disposable-org variable — but note the blast
// radius is genuinely small compared with the log-export tests: it only ever touches
// a channel it creates itself, on a webhook URL nothing delivers to, and destroy
// removes it. It does not disturb any existing channel or in-flight delivery.
//
// What only a live run can prove, over and above the httptest fakes:
//
//   - the platform really does reveal the signing secret exactly once, and really
//     does withhold it on subsequent reads (the fake asserts our understanding of
//     the contract; this asserts the contract);
//   - the rotate endpoint really rotates in place, preserving the channel id;
//   - the per-type 422 rules match what the provider enforces at plan time;
//   - import works against a real channel id.
func TestAccNotificationChannelResource_lifecycle(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set; skipping acceptance test")
	}
	testAccPreCheck(t)
	requireDisposableTestOrg(t)

	const resourceName = "barndoor_notification_channel.test"
	// Unique per run so concurrent or repeated runs cannot collide on the
	// webhook URL's natural identity (which would hit the upsert-dedup path and
	// surface as the import-instead error).
	url := fmt.Sprintf("https://example.com/tf-acc-channel-%d", time.Now().UnixNano())

	var createdID, createdSecret string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccNotificationChannelConfig(url, `["break_glass_used"]`, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckResourceAttr(resourceName, "type", channelTypeWebhook),
					resource.TestCheckResourceAttr(resourceName, "url", url),
					resource.TestCheckResourceAttr(resourceName, "has_signing_secret", "true"),
					resource.TestCheckResourceAttrSet(resourceName, "signing_secret"),
					func(st *terraform.State) error {
						rs := st.RootModule().Resources[resourceName]
						createdID = rs.Primary.Attributes["id"]
						createdSecret = rs.Primary.Attributes["signing_secret"]
						if createdSecret == "" {
							return fmt.Errorf("the platform revealed no signing secret on create")
						}
						if !strings.HasPrefix(createdSecret, "whsec_") {
							return fmt.Errorf("signing_secret = %q, want a whsec_ prefix", createdSecret)
						}
						return nil
					},
				),
			},
			{
				// The secret is NOT re-returned by a read, so a refresh must not
				// disturb it and the plan must be empty.
				Config:   testAccNotificationChannelConfig(url, `["break_glass_used"]`, ""),
				PlanOnly: true,
			},
			{
				// Replace the subscription set in place; the secret must survive.
				Config: testAccNotificationChannelConfig(url, `["break_glass_used", "policy_changed"]`, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "subscriptions.#", "2"),
					func(st *terraform.State) error {
						rs := st.RootModule().Resources[resourceName]
						if got := rs.Primary.Attributes["signing_secret"]; got != createdSecret {
							return fmt.Errorf("signing_secret changed on a non-rotating update")
						}
						if got := rs.Primary.Attributes["id"]; got != createdID {
							return fmt.Errorf("channel was replaced on a subscription edit: %q -> %q", createdID, got)
						}
						return nil
					},
				),
			},
			{
				// Rotate in place against the real rotate endpoint.
				Config: testAccNotificationChannelConfig(url, `["break_glass_used", "policy_changed"]`, "rotation-1"),
				Check: resource.ComposeAggregateTestCheckFunc(
					func(st *terraform.State) error {
						rs := st.RootModule().Resources[resourceName]
						if got := rs.Primary.Attributes["id"]; got != createdID {
							return fmt.Errorf("rotation replaced the channel: %q -> %q", createdID, got)
						}
						got := rs.Primary.Attributes["signing_secret"]
						if got == createdSecret {
							return fmt.Errorf("rotation returned the same secret")
						}
						if !strings.HasPrefix(got, "whsec_") {
							return fmt.Errorf("rotated signing_secret = %q, want a whsec_ prefix", got)
						}
						return nil
					},
				),
			},
			{
				// Import by real channel id. signing_secret cannot be recovered on
				// import (the platform revealed it once, to the earlier apply) and
				// rotate_when_changed is config-only, so both are ignored.
				ResourceName:            resourceName,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"signing_secret", "rotate_when_changed"},
			},
		},
	})
}

func testAccNotificationChannelConfig(url, subs, rotate string) string {
	rotateBlock := ""
	if rotate != "" {
		rotateBlock = fmt.Sprintf("\n  rotate_when_changed = { token = %q }\n", rotate)
	}
	return fmt.Sprintf(`
resource "barndoor_notification_channel" "test" {
  type          = "webhook"
  url           = %[1]q
  subscriptions = %[2]s
%[3]s}
`, url, subs, rotateBlock)
}
