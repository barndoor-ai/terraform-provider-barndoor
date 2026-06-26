// Copyright (c) Barndoor AI, Inc.
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAPIErrorError(t *testing.T) {
	tests := map[string]struct {
		body        string
		wantContain string // substring the message must contain
		wantOmit    string // substring the message must NOT contain (empty = no check)
	}{
		"empty body omits the body segment": {
			body:        "",
			wantContain: "GET /x: unexpected status 500",
			wantOmit:    "status 500:", // no trailing "<status>: <body>" separator
		},
		"short plain-text body is included verbatim": {
			body:        "export not found",
			wantContain: "unexpected status 404: export not found",
		},
		`json {"message"} is extracted, braces dropped`: {
			body:        `{"message":"export not found","trace_id":"abc-123"}`,
			wantContain: "unexpected status 404: export not found",
			wantOmit:    "trace_id",
		},
		`json {"error"} is extracted`: {
			body:        `{"error":"destination unreachable"}`,
			wantContain: ": destination unreachable",
			wantOmit:    "{",
		},
		"non-object json falls back to the raw body": {
			body:        `["a","b"]`,
			wantContain: `: ["a","b"]`,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			status := http.StatusNotFound
			if tc.body == "" {
				status = http.StatusInternalServerError
			}
			e := &apiError{method: http.MethodGet, path: "/x", status: status, body: tc.body}
			got := e.Error()
			if !strings.Contains(got, tc.wantContain) {
				t.Errorf("Error() = %q, want it to contain %q", got, tc.wantContain)
			}
			if tc.wantOmit != "" && strings.Contains(got, tc.wantOmit) {
				t.Errorf("Error() = %q, want it to omit %q", got, tc.wantOmit)
			}
		})
	}
}

func TestAPIErrorError_BoundsLongBody(t *testing.T) {
	long := strings.Repeat("a", 5000)
	e := &apiError{method: http.MethodGet, path: "/x", status: http.StatusInternalServerError, body: long}

	got := e.Error()
	if !strings.HasSuffix(got, "…") {
		t.Errorf("Error() = %q, want it to end with an ellipsis", got)
	}
	// The rendered message is the prefix plus a bounded body; nowhere near 5000.
	if len(got) > 200+maxErrorBodyLen+len("…") {
		t.Errorf("Error() length = %d, want it bounded near maxErrorBodyLen (%d)", len(got), maxErrorBodyLen)
	}
	// The full body stays on the struct for debug logging.
	if e.body != long {
		t.Error("apiError.body should retain the full response body")
	}
}

func TestJSONErrorMessage(t *testing.T) {
	tests := map[string]struct {
		body string
		want string
	}{
		"message field":                {`{"message":"boom"}`, "boom"},
		"error field":                  {`{"error":"boom"}`, "boom"},
		"detail field":                 {`{"detail":"boom"}`, "boom"},
		"message wins over error":      {`{"error":"second","message":"first"}`, "first"},
		"blank message falls to error": {`{"message":"   ","error":"real"}`, "real"},
		"message is trimmed":           {`{"message":"  boom  "}`, "boom"},
		"object without known keys":    {`{"code":42}`, ""},
		"non-string error value":       {`{"error":{"code":42}}`, ""},
		"json array is not an object":  {`["a","b"]`, ""},
		"plain text is not json":       {`export not found`, ""},
		"empty string":                 {``, ""},
		"malformed json":               {`{"message":`, ""},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := jsonErrorMessage(tc.body); got != tc.want {
				t.Errorf("jsonErrorMessage(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	t.Run("shorter than limit is unchanged", func(t *testing.T) {
		if got := truncate("hello", 10); got != "hello" {
			t.Errorf("truncate = %q, want %q", got, "hello")
		}
	})

	t.Run("exactly at limit is unchanged", func(t *testing.T) {
		if got := truncate("hello", 5); got != "hello" {
			t.Errorf("truncate = %q, want %q", got, "hello")
		}
	})

	t.Run("over limit is cut and gets an ellipsis", func(t *testing.T) {
		got := truncate("hello world", 5)
		if !strings.HasSuffix(got, "…") {
			t.Errorf("truncate = %q, want an ellipsis suffix", got)
		}
		if !strings.HasPrefix(got, "hello") {
			t.Errorf("truncate = %q, want it to keep the first 5 bytes", got)
		}
	})

	t.Run("does not split a multi-byte rune", func(t *testing.T) {
		// "é" is two bytes; a byte-boundary cut at an odd limit would split one.
		got := truncate(strings.Repeat("é", 100), 5)
		if !utf8.ValidString(got) {
			t.Errorf("truncate produced invalid UTF-8: %q", got)
		}
		if !strings.HasSuffix(got, "…") {
			t.Errorf("truncate = %q, want an ellipsis suffix", got)
		}
	})
}
