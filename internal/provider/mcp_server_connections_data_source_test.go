// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	frameworkdatasource "github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// --- fake admin roster endpoint ---------------------------------------------------

// fakeServerConnection is one row of the admin roster
// (`ServerConnectionSummary`). UserID and ApplicationID are mutually exclusive
// and both nil on a service-account row, mirroring the real model.
type fakeServerConnection struct {
	ConnectionID   string  `json:"connection_id"`
	UserID         *string `json:"user_id"`
	ApplicationID  *string `json:"application_id"`
	Status         string  `json:"status"`
	CreatedAt      string  `json:"created_at"`
	ConnectedAt    *string `json:"connected_at"`
	LastAccessedAt *string `json:"last_accessed_at"`
}

// ownerClass returns the row's owner class using the same mutually-exclusive
// taxonomy the platform applies.
func (c fakeServerConnection) ownerClass() string {
	switch {
	case c.UserID != nil:
		return "user"
	case c.ApplicationID != nil:
		return "agent"
	default:
		return "service_account"
	}
}

// listServerConnections serves GET /api/registry/v1/servers/{id}/connections:
// the org-admin roster, with the endpoint's status and owner filters and its
// paginated envelope. Callers hold f.mu (handleServers took it).
func (f *fakeRegistryServer) listServerConnections(w http.ResponseWriter, r *http.Request, serverID string) {
	f.rosterQueries = append(f.rosterQueries, r.URL.RawQuery)

	if f.rosterForbidden {
		writeJSONError(w, http.StatusForbidden, "Not authorized to list connections for this server")
		return
	}
	if s, ok := f.servers[serverID]; !ok || s == nil {
		// Soft-deleted servers stay readable through this route, matching the
		// platform: the roster outlives the deletion of the server row.
		writeJSONError(w, http.StatusNotFound, "MCP server not found")
		return
	}

	statuses := splitFilter(r.URL.Query().Get("status"))
	owners := splitFilter(r.URL.Query().Get("owner"))

	// The endpoint rejects a status it can never have stored rather than
	// returning an empty roster that reads as "nobody is connected".
	for want := range statuses {
		if want == "available" {
			writeJSONError(w, http.StatusBadRequest,
				"Invalid status filter: available. Valid values: connected, error, pending")
			return
		}
	}

	rows := []any{}
	for _, conn := range f.roster[serverID] {
		if len(statuses) > 0 && !statuses[conn.Status] {
			continue
		}
		if len(owners) > 0 && !owners[conn.ownerClass()] {
			continue
		}
		rows = append(rows, conn)
	}

	page, limit := registryPageParams(r)
	writeRegistryPage(w, rows, page, limit)
}

// splitFilter parses a comma-separated filter value into a set; an empty value
// means the filter was omitted.
func splitFilter(raw string) map[string]bool {
	if raw == "" {
		return nil
	}
	out := map[string]bool{}
	for _, v := range strings.Split(raw, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out[v] = true
		}
	}
	return out
}

func strPtr(s string) *string { return &s }

// seedRoster registers the roster fixture server and its rows on the fake.
func seedRoster(f *fakeRegistryServer, rows ...fakeServerConnection) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.servers[rosterServerID] = &fakeMcpServer{ID: rosterServerID, Name: "Roster Server"}
	f.roster[rosterServerID] = rows
}

// rosterQueries returns the recorded roster request query strings.
func rosterQueries(f *fakeRegistryServer) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.rosterQueries...)
}

// assertRosterQueryParam fails unless every recorded roster request carried
// key=want. This is what keeps a filter test honest: without it the test would
// still pass if the provider dropped the filter, because the fake's unfiltered
// roster can coincide with the filtered one.
func assertRosterQueryParam(t *testing.T, f *fakeRegistryServer, key, want string) {
	t.Helper()
	queries := rosterQueries(f)
	if len(queries) == 0 {
		t.Fatalf("the roster endpoint was never called, so %s=%s was never sent", key, want)
	}
	for _, q := range queries {
		if !strings.Contains(q, key+"="+want) {
			t.Errorf("roster request %q did not carry %s=%s", q, key, want)
		}
	}
}

// --- schema tests ------------------------------------------------------------------

func TestMcpServerConnectionsDataSource_Metadata(t *testing.T) {
	var resp frameworkdatasource.MetadataResponse
	NewMcpServerConnectionsDataSource().Metadata(context.Background(),
		frameworkdatasource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_mcp_server_connections"; got != want {
		t.Fatalf("TypeName = %q, want %q", got, want)
	}
}

func TestMcpServerConnectionsDataSource_Schema(t *testing.T) {
	var resp frameworkdatasource.SchemaResponse
	NewMcpServerConnectionsDataSource().Schema(context.Background(),
		frameworkdatasource.SchemaRequest{}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("Schema returned diagnostics: %v", resp.Diagnostics)
	}
	if _, ok := resp.Schema.Attributes["server_id"]; !ok {
		t.Fatal("schema is missing the required server_id attribute")
	}
	for _, name := range []string{"status", "owner", "connections"} {
		if _, ok := resp.Schema.Attributes[name]; !ok {
			t.Fatalf("schema is missing the %s attribute", name)
		}
	}
}

// TestMcpServerConnectionsDataSource_statusExcludesAvailable pins that
// `available` is not an accepted status. It is computed per-caller from
// credential state and never stored, so accepting it would filter to nothing —
// indistinguishable from "nobody is connected".
func TestMcpServerConnectionsDataSource_statusExcludesAvailable(t *testing.T) {
	for _, s := range serverConnectionStatuses {
		if s == "available" {
			t.Fatal("serverConnectionStatuses must not offer `available`: the platform never stores it")
		}
	}
	if len(serverConnectionStatuses) != 3 {
		t.Fatalf("expected the three stored statuses, got %v", serverConnectionStatuses)
	}
}

// --- lifecycle (real plan against the fake) ---------------------------------------

const rosterServerID = "srv-roster-1"

func rosterConfig(extra string) string {
	return fmt.Sprintf(`
data "barndoor_mcp_server_connections" "roster" {
  server_id = %q
%s}
`, rosterServerID, extra)
}

// TestMcpServerConnectionsDataSource_readsEveryOwnerClass is the regression
// guard for the defect the platform caught in its own review: a default that
// filtered to `user_id IS NOT NULL` silently dropped agent-owned rows, handing
// an admin a migration list that looked complete while omitting every AI agent
// still bound to the server. With no `owner` set, all three classes must appear.
func TestMcpServerConnectionsDataSource_readsEveryOwnerClass(t *testing.T) {
	fake := setupRegistryTest(t)
	seedRoster(fake,
		fakeServerConnection{
			ConnectionID: "conn-user", UserID: strPtr("kc-sub-alice"), Status: "connected",
			CreatedAt: "2026-08-01T00:00:00Z", ConnectedAt: strPtr("2026-08-01T00:05:00Z"),
			LastAccessedAt: strPtr("2026-08-20T12:00:00Z"),
		},
		fakeServerConnection{
			ConnectionID: "conn-agent", ApplicationID: strPtr("app-7"), Status: "connected",
			CreatedAt: "2026-08-02T00:00:00Z", ConnectedAt: strPtr("2026-08-02T00:05:00Z"),
		},
		fakeServerConnection{
			ConnectionID: "conn-sa", Status: "error",
			CreatedAt: "2026-08-03T00:00:00Z",
		},
	)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: rosterConfig(""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server_connections.roster", "connections.#", "3"),
					// Person-owned row: user_id set, application_id null.
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server_connections.roster", "connections.0.connection_id", "conn-user"),
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server_connections.roster", "connections.0.user_id", "kc-sub-alice"),
					resource.TestCheckNoResourceAttr(
						"data.barndoor_mcp_server_connections.roster", "connections.0.application_id"),
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server_connections.roster", "connections.0.last_accessed_at",
						"2026-08-20T12:00:00Z"),
					// Agent-owned row: present by default, application_id set.
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server_connections.roster", "connections.1.connection_id", "conn-agent"),
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server_connections.roster", "connections.1.application_id", "app-7"),
					resource.TestCheckNoResourceAttr(
						"data.barndoor_mcp_server_connections.roster", "connections.1.user_id"),
					// A never-used connection reports a null last_accessed_at,
					// which is not a liveness signal.
					resource.TestCheckNoResourceAttr(
						"data.barndoor_mcp_server_connections.roster", "connections.1.last_accessed_at"),
					// Service-account row: neither owner field set.
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server_connections.roster", "connections.2.connection_id", "conn-sa"),
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server_connections.roster", "connections.2.status", "error"),
					resource.TestCheckNoResourceAttr(
						"data.barndoor_mcp_server_connections.roster", "connections.2.user_id"),
					resource.TestCheckNoResourceAttr(
						"data.barndoor_mcp_server_connections.roster", "connections.2.application_id"),
					resource.TestCheckNoResourceAttr(
						"data.barndoor_mcp_server_connections.roster", "connections.2.connected_at"),
				),
			},
		},
	})

	// No filter set means no filter sent — the endpoint's own default is every
	// class, and the provider must not narrow it on the client side.
	for _, q := range rosterQueries(fake) {
		if strings.Contains(q, "owner=") {
			t.Errorf("roster request %q sent an owner filter that the configuration did not set", q)
		}
	}
}

// TestMcpServerConnectionsDataSource_pagesToExhaustion seeds more rows than one
// page holds. The endpoint bounds `limit` at 100, so a client that reads only
// the first page returns a short roster — the exact silent-truncation failure
// this data source exists to remove.
func TestMcpServerConnectionsDataSource_pagesToExhaustion(t *testing.T) {
	fake := setupRegistryTest(t)

	const total = 250
	rows := make([]fakeServerConnection, 0, total)
	for i := 0; i < total; i++ {
		rows = append(rows, fakeServerConnection{
			ConnectionID: fmt.Sprintf("conn-%03d", i),
			UserID:       strPtr(fmt.Sprintf("kc-sub-%03d", i)),
			Status:       "connected",
			CreatedAt:    "2026-08-01T00:00:00Z",
		})
	}
	seedRoster(fake, rows...)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: rosterConfig(""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server_connections.roster", "connections.#", "250"),
					// First and last rows both present, in API order.
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server_connections.roster", "connections.0.connection_id", "conn-000"),
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server_connections.roster", "connections.249.connection_id", "conn-249"),
				),
			},
		},
	})

	// Three pages at the endpoint's maximum page size, not one.
	if got := len(rosterQueries(fake)); got < 3 {
		t.Errorf("expected at least 3 roster requests to page 250 rows, got %d", got)
	}
	assertRosterQueryParam(t, fake, "limit", "100")
}

// TestMcpServerConnectionsDataSource_filtersReachTheAPI checks that both
// filters are sent, and that their values are joined as the endpoint expects.
func TestMcpServerConnectionsDataSource_filtersReachTheAPI(t *testing.T) {
	fake := setupRegistryTest(t)
	seedRoster(fake,
		fakeServerConnection{
			ConnectionID: "conn-user", UserID: strPtr("kc-sub-alice"), Status: "connected",
			CreatedAt: "2026-08-01T00:00:00Z",
		},
		fakeServerConnection{
			ConnectionID: "conn-agent", ApplicationID: strPtr("app-7"), Status: "connected",
			CreatedAt: "2026-08-02T00:00:00Z",
		},
		fakeServerConnection{
			ConnectionID: "conn-user-error", UserID: strPtr("kc-sub-bob"), Status: "error",
			CreatedAt: "2026-08-03T00:00:00Z",
		},
	)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: rosterConfig(`  status = ["connected"]
  owner  = ["user"]
`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server_connections.roster", "connections.#", "1"),
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server_connections.roster", "connections.0.connection_id", "conn-user"),
				),
			},
		},
	})

	assertRosterQueryParam(t, fake, "status", "connected")
	assertRosterQueryParam(t, fake, "owner", "user")
}

// TestMcpServerConnectionsDataSource_multiValueFilter pins the comma-joined
// wire format for a multi-value filter. The values are sorted so the request
// is independent of framework set iteration order; this assertion checks the
// joined encoding (`agent%2Cuser`), not that the sort itself is what produced
// that order — the framework already hands this pair as agent-then-user.
func TestMcpServerConnectionsDataSource_multiValueFilter(t *testing.T) {
	fake := setupRegistryTest(t)
	seedRoster(fake,
		fakeServerConnection{
			ConnectionID: "conn-user", UserID: strPtr("kc-sub-alice"), Status: "connected",
			CreatedAt: "2026-08-01T00:00:00Z",
		},
		fakeServerConnection{
			ConnectionID: "conn-agent", ApplicationID: strPtr("app-7"), Status: "pending",
			CreatedAt: "2026-08-02T00:00:00Z",
		},
	)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: rosterConfig(`  owner = ["user", "agent"]
`),
				Check: resource.TestCheckResourceAttr(
					"data.barndoor_mcp_server_connections.roster", "connections.#", "2"),
			},
		},
	})

	assertRosterQueryParam(t, fake, "owner", "agent%2Cuser")
}

// TestMcpServerConnectionsDataSource_rejectsAvailableStatus checks the
// plan-time rejection, so the practitioner never reaches an API 400.
func TestMcpServerConnectionsDataSource_rejectsAvailableStatus(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: rosterConfig(`  status = ["available"]
`),
				ExpectError: regexp.MustCompile(`(?s)Invalid Attribute Value Match`),
			},
		},
	})
}

func TestMcpServerConnectionsDataSource_rejectsUnknownOwner(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: rosterConfig(`  owner = ["everyone"]
`),
				ExpectError: regexp.MustCompile(`(?s)Invalid Attribute Value Match`),
			},
		},
	})
}

// TestMcpServerConnectionsDataSource_rejectsEmptyFilterSet pins SizeAtLeast(1)
// on both filters. An empty set is not null, so without the validator csvFilter
// would return "" and the request would go out unfiltered — every status/owner
// class, which is the silent-widening (and privacy-exposing) direction.
func TestMcpServerConnectionsDataSource_rejectsEmptyFilterSet(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: rosterConfig(`  status = []
`),
				ExpectError: regexp.MustCompile(`(?s)Invalid Attribute Value.*set must contain at least 1 elements`),
			},
			{
				Config: rosterConfig(`  owner = []
`),
				ExpectError: regexp.MustCompile(`(?s)Invalid Attribute Value.*set must contain at least 1 elements`),
			},
		},
	})
}

// TestMcpServerConnectionsDataSource_unknownServer pins that an unknown or
// cross-organization server id surfaces as "not found", not as a permission
// error — the endpoint answers 404 for another org's server deliberately, so it
// is not an existence oracle.
func TestMcpServerConnectionsDataSource_unknownServer(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: rosterConfig(""),
				ExpectError: regexp.MustCompile(
					`(?s)MCP server not found.*another organization is also reported as not\s+found`),
			},
		},
	})
}

// TestMcpServerConnectionsDataSource_forbidden pins that a 403 is explained as
// the admin-credential requirement, including the older-release case. The raw
// API error alone leaves a practitioner with no idea which of the two it is.
func TestMcpServerConnectionsDataSource_forbidden(t *testing.T) {
	fake := setupRegistryTest(t)
	seedRoster(fake, fakeServerConnection{
		ConnectionID: "conn-user", UserID: strPtr("kc-sub-alice"), Status: "connected",
		CreatedAt: "2026-08-01T00:00:00Z",
	})
	fake.mu.Lock()
	fake.rosterForbidden = true
	fake.mu.Unlock()

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: rosterConfig(""),
				ExpectError: regexp.MustCompile(
					`(?s)Not permitted to read the connection roster.*organization-admin.*v2\.30\.0`),
			},
		},
	})
}

// TestMcpServerConnectionsDataSource_softDeletedServerStillReadable documents
// the fake's fidelity choice: a soft-deleted server stays on the roster route,
// matching the platform (the roster outlives the server row). The fake never
// consults `deleted` — this does not observe that platform behaviour, it pins
// that the fake's 404 path is "missing", not "deleted".
func TestMcpServerConnectionsDataSource_softDeletedServerStillReadable(t *testing.T) {
	fake := setupRegistryTest(t)
	seedRoster(fake, fakeServerConnection{
		ConnectionID: "conn-user", UserID: strPtr("kc-sub-alice"), Status: "connected",
		CreatedAt: "2026-08-01T00:00:00Z",
	})
	fake.mu.Lock()
	fake.servers[rosterServerID].deleted = true
	fake.mu.Unlock()

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: rosterConfig(""),
				Check: resource.TestCheckResourceAttr(
					"data.barndoor_mcp_server_connections.roster", "connections.#", "1"),
			},
		},
	})
}

// TestMcpServerConnectionsDataSource_emptyRoster pins that a server nobody is
// connected to yields an empty list rather than an error or a null.
func TestMcpServerConnectionsDataSource_emptyRoster(t *testing.T) {
	fake := setupRegistryTest(t)
	seedRoster(fake)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: rosterConfig(""),
				Check: resource.TestCheckResourceAttr(
					"data.barndoor_mcp_server_connections.roster", "connections.#", "0"),
			},
		},
	})
}
