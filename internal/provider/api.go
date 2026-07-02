// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// smsAPIPrefix is the system-management-service public API mount point under
// the platform host root. Since v0.2.0 `base_url` is the host root (e.g.
// https://platform.barndoor.ai), so every SMS request path carries this
// service prefix explicitly.
const smsAPIPrefix = "api/system-management/public/v1"

// doJSON issues an authenticated JSON request against the Barndoor public API.
// A 2xx response body is decoded into out (when non-nil); any non-2xx status is
// returned as an *apiError carrying up to 1MiB of the response body. It is the
// single HTTP entry point shared by the resource and data source.
func doJSON(ctx context.Context, c *client.Client, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request body: %w", err)
		}
		rdr = bytes.NewReader(b)
	}

	resp, err := c.Do(ctx, method, path, rdr)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody := strings.TrimSpace(string(data))
		// apiError.Error() bounds this body for user-facing diagnostics, so log
		// it in full here for troubleshooting a large or structured error blob.
		tflog.Debug(ctx, "Barndoor API request returned a non-success status", map[string]any{
			"method":        method,
			"path":          path,
			"status":        resp.StatusCode,
			"response_body": errBody,
		})
		return &apiError{method: method, path: path, status: resp.StatusCode, body: errBody}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode %s %s response: %w", method, path, err)
		}
	}
	return nil
}

// isNotFound reports whether err is an *apiError with a 404 status.
func isNotFound(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.NotFound()
}

// asAPIError unwraps err as an *apiError, so callers can branch on the HTTP
// status of a non-2xx response.
func asAPIError(err error) (*apiError, bool) {
	var apiErr *apiError
	ok := errors.As(err, &apiErr)
	return apiErr, ok
}

// maxErrorBodyLen bounds how many bytes of the response body apiError.Error()
// renders into a diagnostic. The SMS endpoints return short plain-text errors
// today, but a large or structured body would otherwise flood
// `terraform plan`/`apply` output. doJSON logs the full body at debug level.
const maxErrorBodyLen = 512

// apiError is a non-2xx response from the Barndoor API. body holds the full
// (trimmed) response body; Error renders a bounded, readable form of it.
type apiError struct {
	method string
	path   string
	status int
	body   string
}

func (e *apiError) Error() string {
	body := e.displayBody()
	if body == "" {
		return fmt.Sprintf("%s %s: unexpected status %d", e.method, e.path, e.status)
	}
	return fmt.Sprintf("%s %s: unexpected status %d: %s", e.method, e.path, e.status, body)
}

func (e *apiError) NotFound() bool { return e.status == http.StatusNotFound }

// displayBody renders the response body for a user-facing diagnostic: when the
// body is a JSON object it extracts a conventional message field rather than
// dumping the whole object, and the result is always bounded to maxErrorBodyLen.
func (e *apiError) displayBody() string {
	body := strings.TrimSpace(e.body)
	if body == "" {
		return ""
	}
	if msg := jsonErrorMessage(body); msg != "" {
		body = msg
	}
	return truncate(body, maxErrorBodyLen)
}

// jsonErrorMessage extracts a human-readable message from a JSON error body of
// the form {"message": "..."}, {"error": "..."}, or {"detail": "..."}. It
// returns "" when the body is not a JSON object or carries none of those keys
// as a non-empty string, leaving the caller to fall back to the raw body.
func jsonErrorMessage(body string) string {
	if body == "" || body[0] != '{' {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		return ""
	}
	for _, key := range []string{"message", "error", "detail"} {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
		}
	}
	return ""
}

// truncate bounds s to at most limit bytes, appending an ellipsis when it has
// to cut. It steps back to a UTF-8 rune boundary so a multi-byte character is
// never split across the cut.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	end := limit
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "…"
}
