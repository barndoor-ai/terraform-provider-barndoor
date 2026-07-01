// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework-jsontypes/jsontypes"
	frameworkresource "github.com/hashicorp/terraform-plugin-framework/resource"
	frameworkschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	policyv2 "github.com/barndoor-ai/barndoor-go-sdk/proto/barndoor/public/policy/v2"
	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// --- in-process fake PolicyService ---------------------------------------------

// fakePolicyServer is an in-memory barndoor.policy.v2.PolicyService: enough of
// the contract for the resource's CRUD paths, including name uniqueness among
// non-archived policies, the DRAFT creation default, the DRAFT|ACTIVE-only
// creation matrix, rule active defaulting, and UpdateMask semantics (masked
// empty collections clear).
type fakePolicyServer struct {
	policyv2.UnimplementedPolicyServiceServer

	mu       sync.Mutex
	policies map[string]*policyv2.PolicyDetail
	nextID   int

	// lastUpdateMask records the UpdateMask paths of the most recent
	// UpdatePolicy call, for provider-behavior assertions.
	lastUpdateMask []string
}

func newFakePolicyServer() *fakePolicyServer {
	return &fakePolicyServer{policies: map[string]*policyv2.PolicyDetail{}}
}

// clonePolicy deep-copies a policy so callers never share memory with the
// fake's store.
func clonePolicy(p *policyv2.PolicyDetail) *policyv2.PolicyDetail {
	cloned, _ := proto.Clone(p).(*policyv2.PolicyDetail)
	return cloned
}

// nameTaken reports whether a non-archived policy other than excludeID
// already uses name. Callers hold f.mu.
func (f *fakePolicyServer) nameTaken(name, excludeID string) bool {
	for id, p := range f.policies {
		if id != excludeID && p.GetStatus() != policyv2.PolicyStatus_POLICY_STATUS_ARCHIVED && p.GetName() == name {
			return true
		}
	}
	return false
}

// normalizeRules applies the server-side rule defaults (active=true).
func normalizeRules(rules []*policyv2.PolicyRule) []*policyv2.PolicyRule {
	for _, r := range rules {
		if r.Active == nil {
			active := true
			r.Active = &active
		}
	}
	return rules
}

func (f *fakePolicyServer) CreatePolicy(_ context.Context, req *policyv2.CreatePolicyRequest) (*policyv2.CreatePolicyResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if req.GetName() == "" {
		return nil, grpcstatus.Error(codes.InvalidArgument, "policy name is required")
	}
	if f.nameTaken(req.GetName(), "") {
		return nil, grpcstatus.Errorf(codes.AlreadyExists, "policy with name %q already exists", req.GetName())
	}

	status := req.GetStatus()
	switch status {
	case policyv2.PolicyStatus_POLICY_STATUS_UNSPECIFIED:
		status = policyv2.PolicyStatus_POLICY_STATUS_DRAFT
	case policyv2.PolicyStatus_POLICY_STATUS_DRAFT, policyv2.PolicyStatus_POLICY_STATUS_ACTIVE:
	default:
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "a policy cannot be created with status %s", status)
	}

	f.nextID++
	detail := &policyv2.PolicyDetail{
		Id:             fmt.Sprintf("00000000-0000-0000-0000-%012d", f.nextID),
		OrganizationId: "org-123",
		Name:           req.GetName(),
		Status:         status,
		Description:    req.Description,
		McpServerId:    req.GetMcpServerId(),
		ApplicationIds: req.GetApplicationIds(),
		Tags:           req.GetTags(),
		Rules:          normalizeRules(req.GetRules()),
		SupportContact: req.SupportContact,
	}
	f.policies[detail.GetId()] = detail
	return &policyv2.CreatePolicyResponse{Policy: clonePolicy(detail)}, nil
}

func (f *fakePolicyServer) GetPolicy(_ context.Context, req *policyv2.GetPolicyRequest) (*policyv2.GetPolicyResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	p, ok := f.policies[req.GetPolicyId()]
	if !ok {
		return nil, grpcstatus.Errorf(codes.NotFound, "policy %q not found", req.GetPolicyId())
	}
	return &policyv2.GetPolicyResponse{Policy: clonePolicy(p)}, nil
}

func (f *fakePolicyServer) UpdatePolicy(_ context.Context, req *policyv2.UpdatePolicyRequest) (*policyv2.UpdatePolicyResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	p, ok := f.policies[req.GetPolicyId()]
	if !ok {
		return nil, grpcstatus.Errorf(codes.NotFound, "policy %q not found", req.GetPolicyId())
	}

	// The provider contract is to ALWAYS send a mask (legacy no-mask updates
	// cannot clear collections), so the fake refuses mask-less updates to keep
	// that regression loud.
	mask := req.GetUpdateMask()
	if mask == nil {
		return nil, grpcstatus.Error(codes.InvalidArgument, "fake requires update_mask (the provider must always send one)")
	}
	f.lastUpdateMask = slices.Clone(mask.GetPaths())

	for _, path := range mask.GetPaths() {
		switch path {
		case "name":
			if f.nameTaken(req.GetName(), req.GetPolicyId()) {
				return nil, grpcstatus.Errorf(codes.AlreadyExists, "policy with name %q already exists", req.GetName())
			}
			p.Name = req.GetName()
		case "description":
			p.Description = req.Description
		case "application_ids":
			p.ApplicationIds = req.GetApplicationIds()
		case "rules":
			p.Rules = normalizeRules(req.GetRules())
		case "status":
			if req.Status != nil {
				p.Status = req.GetStatus()
			}
		case "tags":
			p.Tags = req.GetTags()
		case "support_contact":
			p.SupportContact = req.SupportContact
		default:
			return nil, grpcstatus.Errorf(codes.InvalidArgument, "unsupported update_mask path %q", path)
		}
	}
	return &policyv2.UpdatePolicyResponse{Policy: clonePolicy(p)}, nil
}

// seed inserts a policy directly into the store (bypassing the RPC surface).
func (f *fakePolicyServer) seed(name string, status policyv2.PolicyStatus) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("00000000-0000-0000-0000-%012d", f.nextID)
	f.policies[id] = &policyv2.PolicyDetail{Id: id, Name: name, McpServerId: "mcp-server-1", Status: status}
	return id
}

// archive flips a stored policy to ARCHIVED out-of-band.
func (f *fakePolicyServer) archive(t *testing.T, id string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.policies[id]
	if !ok {
		t.Fatalf("fake has no policy %q to archive", id)
	}
	p.Status = policyv2.PolicyStatus_POLICY_STATUS_ARCHIVED
}

// get returns a clone of a stored policy.
func (f *fakePolicyServer) get(t *testing.T, id string) *policyv2.PolicyDetail {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.policies[id]
	if !ok {
		t.Fatalf("fake has no policy %q", id)
	}
	return clonePolicy(p)
}

// --- provider-under-test wiring --------------------------------------------------

// setupPolicyTest wires a full provider-under-test environment: an httptest
// token endpoint (so per-RPC credentials mint a token exactly like
// production), the BARNDOOR_* env the provider configures from, and a bufconn
// gRPC listener serving the fake, injected through the provider's test-only
// gRPC hook.
func setupPolicyTest(t *testing.T) *fakePolicyServer {
	t.Helper()

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			writeToken(w)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(tokenSrv.Close)

	// base_url is the host root (v0.2.0); only the token endpoint is HTTP in
	// these tests — policy traffic rides the bufconn gRPC channel.
	t.Setenv("BARNDOOR_BASE_URL", tokenSrv.URL)
	t.Setenv("BARNDOOR_TOKEN_URL", tokenSrv.URL+"/token")
	t.Setenv("BARNDOOR_CLIENT_ID", "test-client")
	t.Setenv("BARNDOOR_CLIENT_SECRET", "test-secret")
	t.Setenv("BARNDOOR_ORGANIZATION_ID", "org-123")

	fake := newFakePolicyServer()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	policyv2.RegisterPolicyServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	prev := testGRPCConfigHook
	testGRPCConfigHook = func(cfg *client.Config) {
		// passthrough:// skips DNS resolution of the fake target name;
		// the context dialer routes every connection onto the bufconn.
		cfg.GRPCTarget = "passthrough:///bufnet"
		cfg.GRPCDialOptions = []grpc.DialOption{
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		}
	}
	t.Cleanup(func() { testGRPCConfigHook = prev })

	return fake
}

// checkAllPoliciesArchived is the CheckDestroy for policy tests: destroy must
// archive (not remove) every policy the test created.
func checkAllPoliciesArchived(fake *fakePolicyServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if len(fake.policies) == 0 {
			return fmt.Errorf("fake holds no policies; destroy should archive, not remove")
		}
		for id, p := range fake.policies {
			if p.GetStatus() != policyv2.PolicyStatus_POLICY_STATUS_ARCHIVED {
				return fmt.Errorf("policy %s is %s after destroy, want ARCHIVED", id, p.GetStatus())
			}
		}
		return nil
	}
}

// --- test configurations ----------------------------------------------------------

func policyConfigMinimal(name string) string {
	return fmt.Sprintf(`
resource "barndoor_policy" "test" {
  name          = %[1]q
  mcp_server_id = "mcp-server-1"
}
`, name)
}

func policyConfigMinimalWithStatus(name, status string) string {
	return fmt.Sprintf(`
resource "barndoor_policy" "test" {
  name          = %[1]q
  mcp_server_id = "mcp-server-1"
  status        = %[2]q
}
`, name, status)
}

// testConditionConfigured is a deliberately non-canonical rendering of the
// condition (extra whitespace and newlines). The server round-trips the
// parsed tree back as compact canonical JSON, so managing this exact text
// without drift proves jsontypes.Normalized's semantic equality end to end.
const testConditionConfigured = `{ "all" : { "of" : [ { "expr" : { "expr" : "request.tool == 'search'" } },
  { "any" : { "of" : [ { "expr" : { "expr" : "request.size < 100" } } ] } } ] } }`

func policyConfigFull(name, condition string) string {
	return fmt.Sprintf(`
resource "barndoor_policy" "test" {
  name            = %[1]q
  mcp_server_id   = "mcp-server-1"
  description     = "Managed by Terraform"
  support_contact = "governance@example.com"
  status          = "ACTIVE"
  tags            = ["prod", "governed"]
  application_ids = ["app-1", "app-2"]

  rules = [{
    name      = "allow search"
    effect    = "ALLOW"
    actions   = ["search", "read"]
    roles     = ["role:analyst", "group:data-team"]
    condition = %[2]q
  }]
}
`, name, condition)
}

// --- resource lifecycle tests -------------------------------------------------------

func TestPolicyResource_basicLifecycle(t *testing.T) {
	fake := setupPolicyTest(t)
	const resourceName = "barndoor_policy.test"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllPoliciesArchived(fake),
		Steps: []resource.TestStep{
			{
				// Create with no status: the server defaults to DRAFT.
				Config: policyConfigMinimal("tf-test-policy"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckResourceAttr(resourceName, "name", "tf-test-policy"),
					resource.TestCheckResourceAttr(resourceName, "mcp_server_id", "mcp-server-1"),
					resource.TestCheckResourceAttr(resourceName, "status", "DRAFT"),
					resource.TestCheckNoResourceAttr(resourceName, "rules.#"),
				),
			},
			{
				// Import by policy id round-trips the full state.
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				// In-place update: rename + activate.
				Config: policyConfigMinimalWithStatus("tf-test-policy-renamed", "ACTIVE"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "name", "tf-test-policy-renamed"),
					resource.TestCheckResourceAttr(resourceName, "status", "ACTIVE"),
				),
			},
		},
	})
}

func TestPolicyResource_rulesRoundTrip(t *testing.T) {
	fake := setupPolicyTest(t)
	const resourceName = "barndoor_policy.test"
	var policyID string

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllPoliciesArchived(fake),
		Steps: []resource.TestStep{
			{
				// Create ACTIVE with a full ALLOW rule; server-computed rule
				// defaults (active) land in state. The configured condition
				// text is non-canonical, but semantic equality keeps it in
				// state verbatim rather than the server's compact rendering.
				Config: policyConfigFull("tf-test-rules", testConditionConfigured),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "status", "ACTIVE"),
					resource.TestCheckResourceAttr(resourceName, "description", "Managed by Terraform"),
					resource.TestCheckResourceAttr(resourceName, "support_contact", "governance@example.com"),
					resource.TestCheckResourceAttr(resourceName, "tags.#", "2"),
					resource.TestCheckResourceAttr(resourceName, "application_ids.#", "2"),
					resource.TestCheckResourceAttr(resourceName, "rules.#", "1"),
					resource.TestCheckResourceAttr(resourceName, "rules.0.name", "allow search"),
					resource.TestCheckResourceAttr(resourceName, "rules.0.effect", "ALLOW"),
					resource.TestCheckResourceAttr(resourceName, "rules.0.active", "true"),
					resource.TestCheckResourceAttr(resourceName, "rules.0.actions.0", "search"),
					resource.TestCheckResourceAttr(resourceName, "rules.0.roles.1", "group:data-team"),
					resource.TestCheckResourceAttr(resourceName, "rules.0.condition", testConditionConfigured),
					func(s *terraform.State) error {
						rs, ok := s.RootModule().Resources[resourceName]
						if !ok {
							return fmt.Errorf("%s not in state", resourceName)
						}
						policyID = rs.Primary.ID
						// The fake must hold the parsed condition tree, not a
						// string blob.
						stored := fake.get(t, policyID)
						if len(stored.GetRules()) != 1 {
							return fmt.Errorf("fake stored %d rules, want 1", len(stored.GetRules()))
						}
						if stored.GetRules()[0].GetCondition().GetAll() == nil {
							return fmt.Errorf("fake stored condition without the all operator: %v", stored.GetRules()[0].GetCondition())
						}
						return nil
					},
				),
			},
			{
				// Re-planning the same config forces a refresh: the server
				// echoes the condition as canonical compact JSON, which must
				// compare semantically equal to the configured text — an
				// empty plan here is the no-perpetual-drift guarantee.
				Config:   policyConfigFull("tf-test-rules", testConditionConfigured),
				PlanOnly: true,
			},
			{
				// Rename and drop the rules attribute entirely: the update must
				// carry a mask containing "rules" so the server clears them.
				Config: policyConfigMinimalWithStatus("tf-test-rules-renamed", "ACTIVE"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "name", "tf-test-rules-renamed"),
					resource.TestCheckNoResourceAttr(resourceName, "rules.#"),
					func(*terraform.State) error {
						fake.mu.Lock()
						defer fake.mu.Unlock()
						if !slices.Contains(fake.lastUpdateMask, "rules") {
							return fmt.Errorf("UpdatePolicy mask %v does not contain %q", fake.lastUpdateMask, "rules")
						}
						if got := len(fake.policies[policyID].GetRules()); got != 0 {
							return fmt.Errorf("fake still stores %d rules after clearing, want 0", got)
						}
						// Removing description/tags/etc. must clear them too —
						// full desired state under the mask.
						if fake.policies[policyID].Description != nil {
							return fmt.Errorf("description not cleared: %q", fake.policies[policyID].GetDescription())
						}
						if got := len(fake.policies[policyID].GetTags()); got != 0 {
							return fmt.Errorf("tags not cleared: %v", fake.policies[policyID].GetTags())
						}
						return nil
					},
				),
			},
		},
	})
}

func TestPolicyResource_nameConflict(t *testing.T) {
	fake := setupPolicyTest(t)
	fake.seed("taken-name", policyv2.PolicyStatus_POLICY_STATUS_ACTIVE)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      policyConfigMinimal("taken-name"),
				ExpectError: regexp.MustCompile(`already exists`),
			},
		},
	})
}

func TestPolicyResource_inactiveAtCreateIsAPlanError(t *testing.T) {
	setupPolicyTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      policyConfigMinimalWithStatus("tf-test-inactive", "INACTIVE"),
				ExpectError: regexp.MustCompile(`Cannot create a policy as INACTIVE`),
			},
		},
	})
}

func TestPolicyResource_archivedOutOfBandRecreates(t *testing.T) {
	fake := setupPolicyTest(t)
	const resourceName = "barndoor_policy.test"
	var firstID string

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: policyConfigMinimal("tf-test-archived"),
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
				// Archive out-of-band: the refresh must drop the resource (an
				// archived policy is deleted in Terraform terms) and the apply
				// must create a replacement.
				PreConfig: func() { fake.archive(t, firstID) },
				Config:    policyConfigMinimal("tf-test-archived"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "status", "DRAFT"),
					func(s *terraform.State) error {
						rs, ok := s.RootModule().Resources[resourceName]
						if !ok {
							return fmt.Errorf("%s not in state", resourceName)
						}
						if rs.Primary.ID == firstID {
							return fmt.Errorf("resource still tracks the archived policy %s; want a re-created one", firstID)
						}
						if got := fake.get(t, firstID).GetStatus(); got != policyv2.PolicyStatus_POLICY_STATUS_ARCHIVED {
							return fmt.Errorf("original policy is %s, want it left ARCHIVED", got)
						}
						return nil
					},
				),
			},
		},
	})
}

// --- schema/metadata tests ----------------------------------------------------------

func TestPolicyResource_Metadata(t *testing.T) {
	var resp frameworkresource.MetadataResponse
	NewPolicyResource().Metadata(context.Background(), frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_policy"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestPolicyResource_Schema(t *testing.T) {
	var resp frameworkresource.SchemaResponse
	NewPolicyResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{"id", "name", "mcp_server_id", "description", "support_contact", "tags", "application_ids", "status", "rules"} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
	if !resp.Schema.Attributes["id"].IsComputed() {
		t.Error("id should be Computed")
	}
	for _, required := range []string{"name", "mcp_server_id"} {
		if !resp.Schema.Attributes[required].IsRequired() {
			t.Errorf("%s should be Required", required)
		}
	}

	rules, ok := resp.Schema.Attributes["rules"].(frameworkschema.ListNestedAttribute)
	if !ok {
		t.Fatalf("rules is %T, want schema.ListNestedAttribute", resp.Schema.Attributes["rules"])
	}
	for _, attr := range []string{"name", "description", "effect", "active", "actions", "roles", "condition"} {
		if _, ok := rules.NestedObject.Attributes[attr]; !ok {
			t.Errorf("rules missing nested attribute %q", attr)
		}
	}
	for _, required := range []string{"effect", "actions", "roles"} {
		if !rules.NestedObject.Attributes[required].IsRequired() {
			t.Errorf("rules.%s should be Required", required)
		}
	}
}

func TestPolicyStatusValidator(t *testing.T) {
	tests := map[string]struct {
		value       types.String
		wantError   bool
		wantContain string
	}{
		"null passes":     {value: types.StringNull()},
		"unknown passes":  {value: types.StringUnknown()},
		"DRAFT passes":    {value: types.StringValue("DRAFT")},
		"ACTIVE passes":   {value: types.StringValue("ACTIVE")},
		"INACTIVE passes": {value: types.StringValue("INACTIVE")},
		"ARCHIVED gets the destroy hint": {
			value:       types.StringValue("ARCHIVED"),
			wantError:   true,
			wantContain: "terraform destroy",
		},
		"garbage is rejected": {
			value:       types.StringValue("ENABLED"),
			wantError:   true,
			wantContain: "ENABLED",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var resp validator.StringResponse
			policyStatusValidator{}.ValidateString(context.Background(), validator.StringRequest{ConfigValue: tc.value}, &resp)
			if got := resp.Diagnostics.HasError(); got != tc.wantError {
				t.Fatalf("HasError() = %v, want %v (diags: %+v)", got, tc.wantError, resp.Diagnostics)
			}
			if tc.wantContain != "" {
				detail := resp.Diagnostics.Errors()[0].Detail()
				if !strings.Contains(detail, tc.wantContain) {
					t.Errorf("detail %q does not contain %q", detail, tc.wantContain)
				}
			}
		})
	}
}

// --- condition conversion tests --------------------------------------------------------

func TestConditionJSONRoundTrip(t *testing.T) {
	tests := map[string]string{
		"leaf expression":  `{"expr":{"expr":"request.tool == 'x'"}}`,
		"all of one":       `{"all":{"of":[{"expr":{"expr":"a"}}]}}`,
		"empty of":         `{"any":{"of":[]}}`,
		"nested operators": `{"none":{"of":[{"all":{"of":[{"expr":{"expr":"a"}},{"any":{"of":[{"expr":{"expr":"b"}}]}}]}}]}}`,
	}

	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			cond, err := conditionFromJSON([]byte(in))
			if err != nil {
				t.Fatalf("conditionFromJSON: %v", err)
			}
			out, err := conditionToJSON(cond)
			if err != nil {
				t.Fatalf("conditionToJSON: %v", err)
			}
			// The inputs are already canonical (compact, one key per object),
			// so the round trip must be byte-identical.
			if out != in {
				t.Errorf("round trip = %s, want %s", out, in)
			}
		})
	}
}

func TestConditionFromJSON_ProtoShape(t *testing.T) {
	cond, err := conditionFromJSON([]byte(`{"all":{"of":[{"expr":{"expr":"a"}},{"none":{"of":[{"expr":{"expr":"b"}}]}}]}}`))
	if err != nil {
		t.Fatalf("conditionFromJSON: %v", err)
	}
	all := cond.GetAll()
	if all == nil {
		t.Fatalf("want the all operator, got %v", cond)
	}
	if len(all.GetOf()) != 2 {
		t.Fatalf("all.of has %d children, want 2", len(all.GetOf()))
	}
	if got := all.GetOf()[0].GetExpr().GetExpr(); got != "a" {
		t.Errorf("first child expr = %q, want %q", got, "a")
	}
	none := all.GetOf()[1].GetNone()
	if none == nil || len(none.GetOf()) != 1 || none.GetOf()[0].GetExpr().GetExpr() != "b" {
		t.Errorf("second child mismapped: %v", all.GetOf()[1])
	}
}

func TestConditionFromJSON_Errors(t *testing.T) {
	tests := map[string]struct {
		in          string
		wantContain string
	}{
		"not an object":            {`["a"]`, "must be a JSON object"},
		"zero keys":                {`{}`, "exactly one of"},
		"multiple keys":            {`{"all":{"of":[]},"any":{"of":[]}}`, "exactly one of"},
		"unknown key":              {`{"some":{"of":[]}}`, `unknown condition key "some"`},
		"expr not an object":       {`{"expr":"raw string"}`, `"expr"`},
		"expr missing field":       {`{"expr":{}}`, `missing its "expr"`},
		"expr with unknown field":  {`{"expr":{"expr":"a","extra":1}}`, "unknown field"},
		"operator missing of":      {`{"all":{}}`, `missing its "of"`},
		"operator with extra keys": {`{"all":{"of":[],"extra":1}}`, "unknown field"},
		"of not an array":          {`{"all":{"of":{"expr":{"expr":"a"}}}}`, `"of"`},
		"nested error is located":  {`{"all":{"of":[{"bogus":{}}]}}`, "all.of[0]"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := conditionFromJSON([]byte(tc.in))
			if err == nil {
				t.Fatalf("conditionFromJSON(%s) succeeded, want an error", tc.in)
			}
			if !strings.Contains(err.Error(), tc.wantContain) {
				t.Errorf("error %q does not contain %q", err, tc.wantContain)
			}
		})
	}
}

// TestConditionToJSON_IsValidJSON guards the canonical rendering against
// accidental non-JSON output.
func TestConditionToJSON_IsValidJSON(t *testing.T) {
	cond := &policyv2.PolicyRuleCondition{
		Match: &policyv2.PolicyRuleCondition_Any{Any: &policyv2.OperatorAny{Of: []*policyv2.PolicyRuleCondition{
			{Match: &policyv2.PolicyRuleCondition_Expr{Expr: &policyv2.Expression{Expr: `say "hi"`}}},
		}}},
	}
	out, err := conditionToJSON(cond)
	if err != nil {
		t.Fatalf("conditionToJSON: %v", err)
	}
	if !json.Valid([]byte(out)) {
		t.Errorf("conditionToJSON produced invalid JSON: %s", out)
	}
}

// --- request/state conversion tests -----------------------------------------------------

func TestBuildUpdatePolicyRequest_FullMaskAndClearing(t *testing.T) {
	ctx := context.Background()
	plan := &policyResourceModel{
		ID:          types.StringValue("policy-1"),
		Name:        types.StringValue("renamed"),
		McpServerID: types.StringValue("mcp-server-1"),
		// Everything else null: the mask makes the empties meaningful clears.
		Description:    types.StringNull(),
		SupportContact: types.StringNull(),
		Tags:           types.SetNull(types.StringType),
		ApplicationIDs: types.SetNull(types.StringType),
		Status:         types.StringValue("ACTIVE"),
		Rules:          nil,
	}

	req, err := buildUpdatePolicyRequest(ctx, "policy-1", plan)
	if err != nil {
		t.Fatalf("buildUpdatePolicyRequest: %v", err)
	}

	if req.GetPolicyId() != "policy-1" {
		t.Errorf("policy_id = %q", req.GetPolicyId())
	}
	if req.Name == nil || *req.Name != "renamed" {
		t.Errorf("name = %v, want renamed", req.Name)
	}
	if req.Description != nil || req.SupportContact != nil {
		t.Errorf("null optionals must map to nil pointers: %+v", req)
	}
	if req.Status == nil || *req.Status != policyv2.PolicyStatus_POLICY_STATUS_ACTIVE {
		t.Errorf("status = %v, want ACTIVE", req.Status)
	}
	if len(req.GetRules()) != 0 || len(req.GetTags()) != 0 || len(req.GetApplicationIds()) != 0 {
		t.Errorf("cleared collections must be empty: %+v", req)
	}

	mask := req.GetUpdateMask()
	if mask == nil {
		t.Fatal("update_mask must always be set")
	}
	for _, want := range policyUpdateMaskPaths {
		if !slices.Contains(mask.GetPaths(), want) {
			t.Errorf("update_mask %v missing path %q", mask.GetPaths(), want)
		}
	}
	if len(mask.GetPaths()) != len(policyUpdateMaskPaths) {
		t.Errorf("update_mask has %d paths, want exactly %d", len(mask.GetPaths()), len(policyUpdateMaskPaths))
	}
}

func TestBuildCreatePolicyRequest_StatusAndActive(t *testing.T) {
	ctx := context.Background()
	activeFalse := types.BoolValue(false)
	plan := &policyResourceModel{
		Name:        types.StringValue("p"),
		McpServerID: types.StringValue("m"),
		Status:      types.StringNull(), // omitted → UNSPECIFIED → server default DRAFT
		Rules: []policyRuleModel{
			{
				Effect:    types.StringValue("DENY"),
				Active:    activeFalse,
				Actions:   mustStringList(t, "*"),
				Roles:     mustStringList(t, "*"),
				Condition: jsontypes.NewNormalizedNull(),
			},
			{
				Effect:    types.StringValue("ALLOW"),
				Active:    types.BoolUnknown(), // computed, not set → omit, server defaults
				Actions:   mustStringList(t, "read"),
				Roles:     mustStringList(t, "role:x"),
				Condition: jsontypes.NewNormalizedNull(),
			},
		},
	}

	req, err := buildCreatePolicyRequest(ctx, plan)
	if err != nil {
		t.Fatalf("buildCreatePolicyRequest: %v", err)
	}
	if req.GetStatus() != policyv2.PolicyStatus_POLICY_STATUS_UNSPECIFIED {
		t.Errorf("status = %v, want UNSPECIFIED so the server applies its default", req.GetStatus())
	}
	if req.GetRules()[0].Active == nil || *req.GetRules()[0].Active {
		t.Errorf("rules[0].active = %v, want explicit false", req.GetRules()[0].Active)
	}
	if req.GetRules()[1].Active != nil {
		t.Errorf("rules[1].active = %v, want nil (unset → server default)", req.GetRules()[1].Active)
	}
	if req.GetRules()[0].GetEffect() != policyv2.Effect_EFFECT_DENY {
		t.Errorf("rules[0].effect = %v, want DENY", req.GetRules()[0].GetEffect())
	}
}

func TestApplyPolicyDetail_NullVsEmpty(t *testing.T) {
	ctx := context.Background()
	emptyString := ""
	supportContact := "help@example.com"

	detail := &policyv2.PolicyDetail{
		Id:             "policy-1",
		Name:           "p",
		McpServerId:    "m",
		Status:         policyv2.PolicyStatus_POLICY_STATUS_DRAFT,
		Description:    &emptyString, // server echoes "" where config had nothing
		SupportContact: &supportContact,
		Tags:           nil,
		ApplicationIds: nil,
		Rules:          nil,
	}
	prior := &policyResourceModel{
		Description:    types.StringNull(),
		SupportContact: types.StringNull(),
		Tags:           types.SetNull(types.StringType),
		ApplicationIDs: types.SetNull(types.StringType),
		Rules:          nil,
	}

	state, err := applyPolicyDetail(ctx, detail, prior)
	if err != nil {
		t.Fatalf("applyPolicyDetail: %v", err)
	}

	if !state.Description.IsNull() {
		t.Errorf("description = %v, want null (empty server echo + null prior)", state.Description)
	}
	if state.SupportContact.ValueString() != "help@example.com" {
		t.Errorf("support_contact = %v", state.SupportContact)
	}
	if !state.Tags.IsNull() || !state.ApplicationIDs.IsNull() {
		t.Errorf("empty collections with null priors must stay null: tags=%v app_ids=%v", state.Tags, state.ApplicationIDs)
	}
	if state.Rules != nil {
		t.Errorf("rules = %v, want nil (null list)", state.Rules)
	}
	if state.Status.ValueString() != "DRAFT" {
		t.Errorf("status = %v", state.Status)
	}
}

func TestApplyPolicyDetail_PreservesExplicitEmptyRules(t *testing.T) {
	ctx := context.Background()
	detail := &policyv2.PolicyDetail{
		Id: "policy-1", Name: "p", McpServerId: "m",
		Status: policyv2.PolicyStatus_POLICY_STATUS_ACTIVE,
	}
	prior := &policyResourceModel{
		Tags:           types.SetNull(types.StringType),
		ApplicationIDs: types.SetNull(types.StringType),
		Rules:          []policyRuleModel{}, // explicit `rules = []` in config
	}

	state, err := applyPolicyDetail(ctx, detail, prior)
	if err != nil {
		t.Fatalf("applyPolicyDetail: %v", err)
	}
	if state.Rules == nil || len(state.Rules) != 0 {
		t.Errorf("rules = %#v, want a non-nil empty slice (known empty list)", state.Rules)
	}
}

func TestApplyPolicyDetail_MapsRules(t *testing.T) {
	ctx := context.Background()
	ruleName := "r1"
	inactive := false
	detail := &policyv2.PolicyDetail{
		Id: "policy-1", Name: "p", McpServerId: "m",
		Status:         policyv2.PolicyStatus_POLICY_STATUS_ACTIVE,
		SupportContact: nil, // absent optional → null
		Rules: []*policyv2.PolicyRule{
			{
				Name:    &ruleName,
				Effect:  policyv2.Effect_EFFECT_DENY,
				Actions: []string{"write"},
				Roles:   []string{"*"},
				Active:  &inactive,
				Condition: &policyv2.PolicyRuleCondition{
					Match: &policyv2.PolicyRuleCondition_Expr{Expr: &policyv2.Expression{Expr: "a"}},
				},
			},
			{
				Effect:  policyv2.Effect_EFFECT_ALLOW,
				Actions: []string{"read"},
				Roles:   []string{"role:x"},
				// Active nil: tolerate and settle on the server default.
			},
		},
	}
	prior := &policyResourceModel{
		Tags:           types.SetNull(types.StringType),
		ApplicationIDs: types.SetNull(types.StringType),
	}

	state, err := applyPolicyDetail(ctx, detail, prior)
	if err != nil {
		t.Fatalf("applyPolicyDetail: %v", err)
	}
	if state.SupportContact.IsNull() != true {
		t.Errorf("support_contact = %v, want null for an absent optional", state.SupportContact)
	}
	if len(state.Rules) != 2 {
		t.Fatalf("rules len = %d, want 2", len(state.Rules))
	}
	r0 := state.Rules[0]
	if r0.Name.ValueString() != "r1" || r0.Effect.ValueString() != "DENY" || r0.Active.ValueBool() {
		t.Errorf("rules[0] mismapped: %+v", r0)
	}
	if got := r0.Condition.ValueString(); got != `{"expr":{"expr":"a"}}` {
		t.Errorf("rules[0].condition = %s", got)
	}
	r1 := state.Rules[1]
	if !r1.Active.ValueBool() {
		t.Error("rules[1].active should settle to the server default true")
	}
	if !r1.Name.IsNull() || !r1.Condition.IsNull() {
		t.Errorf("rules[1] optionals should be null: %+v", r1)
	}
}
