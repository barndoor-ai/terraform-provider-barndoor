// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"sync"
	"testing"
	"time"

	frameworkdatasource "github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// --- fake policy-revisions endpoint -------------------------------------------------

const testRevisionsServerID = "11111111-2222-3333-4444-555555555555"

// fakePolicyRevisions serves GET /api/policy/v2/policy-revisions from a
// per-test responder and records the query of every request, so tests can
// assert what the provider actually sent (cursor, since, limit) and how many
// round trips it made.
type fakePolicyRevisions struct {
	mu       sync.Mutex
	requests []url.Values
	respond  func(q url.Values) (status int, body string)
}

// maxFakeRevisionRequests bounds the fake so a provider that loops forever
// fails the test with an error instead of hanging it.
const maxFakeRevisionRequests = 20

func setupPolicyRevisionsTest(t *testing.T, respond func(q url.Values) (int, string)) *fakePolicyRevisions {
	t.Helper()

	fake := &fakePolicyRevisions{respond: respond}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			writeToken(w)
		case "/api/policy/v2/policy-revisions":
			fake.mu.Lock()
			fake.requests = append(fake.requests, r.URL.Query())
			n := len(fake.requests)
			fake.mu.Unlock()
			if r.Method != http.MethodGet {
				http.NotFound(w, r)
				return
			}
			if n > maxFakeRevisionRequests {
				writeJSONError(w, http.StatusInternalServerError, "fake: provider did not stop paging")
				return
			}
			status, body := fake.respond(r.URL.Query())
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	t.Setenv("BARNDOOR_BASE_URL", srv.URL)
	t.Setenv("BARNDOOR_TOKEN_URL", srv.URL+"/token")
	t.Setenv("BARNDOOR_CLIENT_ID", "test-client")
	t.Setenv("BARNDOOR_CLIENT_SECRET", "test-secret")
	t.Setenv("BARNDOOR_ORGANIZATION_ID", "org-123")
	return fake
}

func (f *fakePolicyRevisions) queries() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]url.Values(nil), f.requests...)
}

// revisionRow renders a PolicyRevisionSummary wire object with every field
// populated; callers override fields through the mutate hook.
func revisionRow(id, revisedAt string, mutate func(map[string]any)) map[string]any {
	row := map[string]any{
		"id":                id,
		"policy_id":         "aaaaaaaa-0000-0000-0000-000000000001",
		"changes":           map[string]any{"status": map[string]any{"from": "DRAFT", "to": "ACTIVE"}},
		"revised_at":        revisedAt,
		"revised_by":        "user-1",
		"revised_by_name":   "Ada Admin",
		"triggered_by":      "system",
		"triggered_by_name": "System",
		"categories":        []string{"status"},
		"changes_summary":   []string{"Status changed from DRAFT to ACTIVE"},
		"resolved_refs":     map[string]any{"ignored": "yes"},
		"version_number":    1,
	}
	if mutate != nil {
		mutate(row)
	}
	return row
}

func pageBody(t *testing.T, rows []map[string]any, next any) string {
	t.Helper()
	if rows == nil {
		rows = []map[string]any{}
	}
	b, err := json.Marshal(map[string]any{"data": rows, "next_cursor": next})
	if err != nil {
		t.Fatalf("marshal page: %v", err)
	}
	return string(b)
}

func revisionsConfig(extra string) string {
	return fmt.Sprintf(`
data "barndoor_policy_revisions" "test" {
  mcp_server_id = %q
%s}
`, testRevisionsServerID, extra)
}

const revisionsDS = "data.barndoor_policy_revisions.test"

// --- schema tests -----------------------------------------------------------------

func TestPolicyRevisionsDataSource_Metadata(t *testing.T) {
	var resp frameworkdatasource.MetadataResponse
	NewPolicyRevisionsDataSource().Metadata(context.Background(),
		frameworkdatasource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_policy_revisions"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestPolicyRevisionsDataSource_Schema(t *testing.T) {
	var resp frameworkdatasource.SchemaResponse
	NewPolicyRevisionsDataSource().Schema(context.Background(), frameworkdatasource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}
	for _, attr := range []string{"id", "mcp_server_id", "since", "revisions"} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
	if !resp.Schema.Attributes["mcp_server_id"].IsRequired() {
		t.Error("mcp_server_id must be required")
	}
	if !resp.Schema.Attributes["since"].IsOptional() {
		t.Error("since must be optional")
	}
}

// --- timestamp parsing --------------------------------------------------------------

// setLocalZoneForTest sets time.Local to UTC-7 for the test and restores it.
// Callers must not run in parallel.
func setLocalZoneForTest(t *testing.T) {
	t.Helper()
	orig := time.Local
	time.Local = time.FixedZone("UTC-7", -7*3600)
	t.Cleanup(func() { time.Local = orig })
}

func TestParseRevisedAt(t *testing.T) {
	// Not parallel: time.Local is process-global. A non-UTC local zone makes a
	// naive timestamp parsed as local time (instead of UTC) fail the test.
	setLocalZoneForTest(t)
	cases := []struct {
		in   string
		want string
	}{
		{"2026-09-28T18:10:24.830776", "2026-09-28T18:10:24.830776Z"},  // naive, microseconds
		{"2026-09-28T18:10:24", "2026-09-28T18:10:24Z"},                // naive, no fraction
		{"2026-09-28T18:10:24.830776Z", "2026-09-28T18:10:24.830776Z"}, // explicit Z
		{"2026-09-28T11:10:24.5-07:00", "2026-09-28T18:10:24.5Z"},      // offset converted to UTC
		{"2026-09-28T18:10:24+00:00", "2026-09-28T18:10:24Z"},          // zero offset
		{"2026-09-28T23:59:59.999999", "2026-09-28T23:59:59.999999Z"},  // naive near midnight stays same day
	}
	for _, tc := range cases {
		got, err := parseRevisedAt(tc.in)
		if err != nil {
			t.Errorf("parseRevisedAt(%q) error: %v", tc.in, err)
			continue
		}
		if s := got.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"); s != tc.want {
			t.Errorf("parseRevisedAt(%q) = %q, want %q", tc.in, s, tc.want)
		}
	}
	for _, bad := range []string{"", "yesterday", "2026-09-28"} {
		if _, err := parseRevisedAt(bad); err == nil {
			t.Errorf("parseRevisedAt(%q) succeeded, want error", bad)
		}
	}
}

// --- lifecycle (real plan/apply against the fake) ----------------------------------

func TestPolicyRevisionsDataSource_multiPageTraversal(t *testing.T) {
	// Three pages, newest first: p1 (r1,r2) -> "cur-2" -> p2 (r3,r4) -> "cur-3"
	// -> p3 (r5). Every row must appear, in this order, and each cursor must
	// be forwarded exactly once.
	pages := map[string]string{}
	pages[""] = pageBody(t, []map[string]any{
		revisionRow("r1", "2026-09-28T18:00:00", nil),
		revisionRow("r2", "2026-09-28T17:00:00", nil),
	}, "cur-2")
	pages["cur-2"] = pageBody(t, []map[string]any{
		revisionRow("r3", "2026-09-28T16:00:00", nil),
		revisionRow("r4", "2026-09-28T15:00:00", nil),
	}, "cur-3")
	pages["cur-3"] = pageBody(t, []map[string]any{
		revisionRow("r5", "2026-09-28T14:00:00", nil),
	}, nil)

	fake := setupPolicyRevisionsTest(t, func(q url.Values) (int, string) {
		body, ok := pages[q.Get("cursor")]
		if !ok {
			return http.StatusBadRequest, `{"detail":"invalid_cursor"}`
		}
		return http.StatusOK, body
	})

	checks := []resource.TestCheckFunc{
		resource.TestCheckResourceAttr(revisionsDS, "id", testRevisionsServerID),
		resource.TestCheckResourceAttr(revisionsDS, "revisions.#", "5"),
	}
	for i, id := range []string{"r1", "r2", "r3", "r4", "r5"} {
		checks = append(checks,
			resource.TestCheckResourceAttr(revisionsDS, fmt.Sprintf("revisions.%d.id", i), id))
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: revisionsConfig(""),
			Check:  resource.ComposeAggregateTestCheckFunc(checks...),
		}},
	})

	// Terraform may read the data source more than once; every read must walk
	// the same cursor chain, so compare the first read's sequence.
	qs := fake.queries()
	if len(qs) < 3 {
		t.Fatalf("expected at least 3 page requests, got %d", len(qs))
	}
	var gotCursors []string
	for _, q := range qs[:3] {
		gotCursors = append(gotCursors, q.Get("cursor"))
		if got := q.Get("mcp_server_id"); got != testRevisionsServerID {
			t.Errorf("mcp_server_id = %q, want %q", got, testRevisionsServerID)
		}
		if got := q.Get("limit"); got != "100" {
			t.Errorf("limit = %q, want 100", got)
		}
	}
	if want := []string{"", "cur-2", "cur-3"}; !reflect.DeepEqual(gotCursors, want) {
		t.Errorf("cursor sequence = %v, want %v", gotCursors, want)
	}
}

func TestPolicyRevisionsDataSource_emptyStringCursorEndsPaging(t *testing.T) {
	// An empty-string next_cursor means "no further page", same as null.
	fake := setupPolicyRevisionsTest(t, func(q url.Values) (int, string) {
		if q.Get("cursor") == "" {
			return http.StatusOK, pageBody(t, []map[string]any{
				revisionRow("r1", "2026-09-28T18:00:00", nil),
			}, "cur-2")
		}
		return http.StatusOK, pageBody(t, []map[string]any{
			revisionRow("r2", "2026-09-28T17:00:00", nil),
		}, "")
	})

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: revisionsConfig(""),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(revisionsDS, "revisions.#", "2"),
				resource.TestCheckResourceAttr(revisionsDS, "revisions.0.id", "r1"),
				resource.TestCheckResourceAttr(revisionsDS, "revisions.1.id", "r2"),
			),
		}},
	})

	// Every read must be exactly two requests; a third would mean the empty
	// cursor was followed.
	qs := fake.queries()
	if len(qs) == 0 || len(qs)%2 != 0 {
		t.Fatalf("expected a whole number of two-request reads, got %d requests", len(qs))
	}
	for i, q := range qs {
		want := ""
		if i%2 == 1 {
			want = "cur-2"
		}
		if got := q.Get("cursor"); got != want {
			t.Errorf("request %d cursor = %q, want %q", i, got, want)
		}
	}
}

func TestPolicyRevisionsDataSource_empty(t *testing.T) {
	setupPolicyRevisionsTest(t, func(url.Values) (int, string) {
		return http.StatusOK, pageBody(t, nil, nil)
	})

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: revisionsConfig(""),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(revisionsDS, "id", testRevisionsServerID),
				// An empty history is an empty list, not an absent attribute.
				resource.TestCheckResourceAttr(revisionsDS, "revisions.#", "0"),
			),
		}},
	})
}

func TestPolicyRevisionsDataSource_sinceForwarded(t *testing.T) {
	fake := setupPolicyRevisionsTest(t, func(url.Values) (int, string) {
		return http.StatusOK, pageBody(t, nil, nil)
	})

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			// A non-UTC offset must be normalized to UTC before it is sent.
			Config: revisionsConfig("  since = \"2026-09-01T09:00:00-07:00\"\n"),
			Check:  resource.TestCheckResourceAttr(revisionsDS, "revisions.#", "0"),
		}},
	})

	qs := fake.queries()
	if len(qs) == 0 {
		t.Fatal("no requests reached the fake")
	}
	for _, q := range qs {
		if got, want := q.Get("since"), "2026-09-01T16:00:00Z"; got != want {
			t.Errorf("since = %q, want %q (normalized to UTC)", got, want)
		}
	}
}

func TestPolicyRevisionsDataSource_unknownSince(t *testing.T) {
	fake := setupPolicyRevisionsTest(t, func(url.Values) (int, string) {
		return http.StatusOK, pageBody(t, nil, nil)
	})

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			// terraform_data.t.output is unknown at plan time, so validation
			// must skip `since` instead of parsing the empty placeholder.
			Config: `
resource "terraform_data" "t" {
  input = "2026-09-01T09:00:00-07:00"
}

data "barndoor_policy_revisions" "test" {
  mcp_server_id = "` + testRevisionsServerID + `"
  since         = terraform_data.t.output
}
`,
			Check: resource.TestCheckResourceAttr(revisionsDS, "revisions.#", "0"),
		}},
	})

	qs := fake.queries()
	if len(qs) == 0 {
		t.Fatal("no requests reached the fake")
	}
	for _, q := range qs {
		if got, want := q.Get("since"), "2026-09-01T16:00:00Z"; got != want {
			t.Errorf("since = %q, want %q (normalized to UTC)", got, want)
		}
	}
}

func TestPolicyRevisionsDataSource_sinceOmittedIsNotSent(t *testing.T) {
	fake := setupPolicyRevisionsTest(t, func(url.Values) (int, string) {
		return http.StatusOK, pageBody(t, nil, nil)
	})

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps:                    []resource.TestStep{{Config: revisionsConfig("")}},
	})

	for _, q := range fake.queries() {
		if q.Has("since") {
			t.Errorf("since was sent (%q) although not configured", q.Get("since"))
		}
	}
}

func TestPolicyRevisionsDataSource_invalidSince(t *testing.T) {
	fake := setupPolicyRevisionsTest(t, func(url.Values) (int, string) {
		return http.StatusOK, pageBody(t, nil, nil)
	})

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config:      revisionsConfig("  since = \"last tuesday\"\n"),
			ExpectError: regexp.MustCompile(`Invalid .since. timestamp`),
		}},
	})

	if n := len(fake.queries()); n != 0 {
		t.Errorf("an invalid since must be rejected before any API call, got %d requests", n)
	}
}

func TestPolicyRevisionsDataSource_timestampNormalization(t *testing.T) {
	// Not parallel: time.Local is process-global (see setLocalZoneForTest).
	setLocalZoneForTest(t)
	setupPolicyRevisionsTest(t, func(url.Values) (int, string) {
		return http.StatusOK, pageBody(t, []map[string]any{
			revisionRow("naive", "2026-09-28T18:10:24.830776", nil),
			revisionRow("zulu", "2026-09-28T18:10:24.830776Z", nil),
			revisionRow("offset", "2026-09-28T11:10:24-07:00", nil),
		}, nil)
	})

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: revisionsConfig(""),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(revisionsDS, "revisions.0.revised_at", "2026-09-28T18:10:24.830776Z"),
				resource.TestCheckResourceAttr(revisionsDS, "revisions.1.revised_at", "2026-09-28T18:10:24.830776Z"),
				resource.TestCheckResourceAttr(revisionsDS, "revisions.2.revised_at", "2026-09-28T18:10:24Z"),
			),
		}},
	})
}

func TestPolicyRevisionsDataSource_fieldMappingAndNulls(t *testing.T) {
	setupPolicyRevisionsTest(t, func(url.Values) (int, string) {
		return http.StatusOK, pageBody(t, []map[string]any{
			revisionRow("full", "2026-09-28T18:00:00", func(r map[string]any) {
				r["version_number"] = 7
				r["categories"] = []string{"status", "rules"}
				r["changes_summary"] = []string{"one", "two"}
			}),
			revisionRow("sparse", "2026-09-28T17:00:00", func(r map[string]any) {
				r["revised_by_name"] = nil
				r["triggered_by"] = nil
				r["triggered_by_name"] = nil
				r["categories"] = []string{}
				r["changes_summary"] = []string{}
			}),
		}, nil)
	})

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: revisionsConfig(""),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(revisionsDS, "revisions.0.policy_id", "aaaaaaaa-0000-0000-0000-000000000001"),
				resource.TestCheckResourceAttr(revisionsDS, "revisions.0.version_number", "7"),
				resource.TestCheckResourceAttr(revisionsDS, "revisions.0.revised_by", "user-1"),
				resource.TestCheckResourceAttr(revisionsDS, "revisions.0.revised_by_name", "Ada Admin"),
				resource.TestCheckResourceAttr(revisionsDS, "revisions.0.triggered_by", "system"),
				resource.TestCheckResourceAttr(revisionsDS, "revisions.0.triggered_by_name", "System"),
				resource.TestCheckResourceAttr(revisionsDS, "revisions.0.categories.#", "2"),
				resource.TestCheckResourceAttr(revisionsDS, "revisions.0.categories.0", "status"),
				resource.TestCheckResourceAttr(revisionsDS, "revisions.0.categories.1", "rules"),
				resource.TestCheckResourceAttr(revisionsDS, "revisions.0.changes_summary.#", "2"),
				resource.TestCheckResourceAttr(revisionsDS, "revisions.0.changes_summary.1", "two"),

				// Nullable strings map to null (attribute absent from state), never "".
				resource.TestCheckResourceAttr(revisionsDS, "revisions.1.revised_by", "user-1"),
				resource.TestCheckNoResourceAttr(revisionsDS, "revisions.1.revised_by_name"),
				resource.TestCheckNoResourceAttr(revisionsDS, "revisions.1.triggered_by"),
				resource.TestCheckNoResourceAttr(revisionsDS, "revisions.1.triggered_by_name"),
				// Empty lists stay empty lists.
				resource.TestCheckResourceAttr(revisionsDS, "revisions.1.categories.#", "0"),
				resource.TestCheckResourceAttr(revisionsDS, "revisions.1.changes_summary.#", "0"),
			),
		}},
	})
}

func TestPolicyRevisionsDataSource_changesJSON(t *testing.T) {
	// The server pretty-prints `changes` with whitespace; the data source must
	// hand back compact JSON with the content intact.
	body := `{"data":[{
	  "id": "r1", "policy_id": "p1", "revised_at": "2026-09-28T18:00:00",
	  "revised_by": "u", "version_number": 3,
	  "categories": [], "changes_summary": [],
	  "changes": {
	    "status": {"from": "DRAFT", "to": "ACTIVE"},
	    "rules": [ {"id": 1, "note": "a b"} ]
	  }
	}], "next_cursor": null}`
	setupPolicyRevisionsTest(t, func(url.Values) (int, string) { return http.StatusOK, body })

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: revisionsConfig(""),
			Check: resource.TestCheckResourceAttr(revisionsDS, "revisions.0.changes_json",
				`{"status":{"from":"DRAFT","to":"ACTIVE"},"rules":[{"id":1,"note":"a b"}]}`),
		}},
	})
}

func TestPolicyRevisionsDataSource_invalidCursorSurfaces(t *testing.T) {
	setupPolicyRevisionsTest(t, func(q url.Values) (int, string) {
		if q.Get("cursor") == "" {
			return http.StatusOK, pageBody(t, []map[string]any{revisionRow("r1", "2026-09-28T18:00:00", nil)}, "bogus")
		}
		return http.StatusBadRequest, `{"detail":"invalid_cursor"}`
	})

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config:      revisionsConfig(""),
			ExpectError: regexp.MustCompile(`(?s)Failed to list policy revisions.*unexpected status 400.*invalid_cursor`),
		}},
	})
}

func TestPolicyRevisionsDataSource_repeatedCursorGuard(t *testing.T) {
	cases := map[string]func(cursor string) string{
		// The server keeps returning the cursor it was just given.
		"immediate repeat": func(cursor string) string { return "same" },
		// c1 -> c2 -> c1: a cycle longer than one hop.
		"cycle": func(cursor string) string {
			if cursor == "c1" {
				return "c2"
			}
			return "c1"
		},
	}
	for name, next := range cases {
		t.Run(name, func(t *testing.T) {
			fake := setupPolicyRevisionsTest(t, func(q url.Values) (int, string) {
				cur := q.Get("cursor")
				return http.StatusOK, pageBody(t, []map[string]any{
					revisionRow("r-"+cur, "2026-09-28T18:00:00", nil),
				}, next(cur))
			})
			if name == "cycle" {
				// Enter the cycle from the first page.
				orig := fake.respond
				fake.respond = func(q url.Values) (int, string) {
					if q.Get("cursor") == "" {
						return http.StatusOK, pageBody(t, nil, "c1")
					}
					return orig(q)
				}
			}

			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{{
					Config:      revisionsConfig(""),
					ExpectError: regexp.MustCompile(`(?s)pagination cursor.*more than once`),
				}},
			})

			if n := len(fake.queries()); n >= maxFakeRevisionRequests {
				t.Errorf("provider made %d requests; the guard should stop it within a handful", n)
			}
		})
	}
}

func TestPolicyRevisionsDataSource_pageCap(t *testing.T) {
	// Not parallel: shrinks a package var.
	orig := policyRevisionsMaxPages
	policyRevisionsMaxPages = 3
	t.Cleanup(func() { policyRevisionsMaxPages = orig })

	// Fresh cursor every page, so only the page cap can stop the walk.
	fake := setupPolicyRevisionsTest(t, func(q url.Values) (int, string) {
		return http.StatusOK, pageBody(t, []map[string]any{
			revisionRow("r-"+q.Get("cursor"), "2026-09-28T18:00:00", nil),
		}, q.Get("cursor")+"x")
	})

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config:      revisionsConfig(""),
			ExpectError: regexp.MustCompile(`(?s)pagination did not terminate after 3\s+pages`),
		}},
	})

	if n := len(fake.queries()); n < 3 || n >= maxFakeRevisionRequests {
		t.Errorf("provider made %d requests; want the walk to stop at the cap of 3 per read", n)
	}
}
