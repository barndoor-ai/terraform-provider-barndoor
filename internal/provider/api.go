// Copyright (c) Barndoor AI, Inc.
// SPDX-License-Identifier: MPL-2.0

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

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

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
		return &apiError{method: method, path: path, status: resp.StatusCode, body: strings.TrimSpace(string(data))}
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

// apiError is a non-2xx response from the Barndoor API.
type apiError struct {
	method string
	path   string
	status int
	body   string
}

func (e *apiError) Error() string {
	if e.body == "" {
		return fmt.Sprintf("%s %s: unexpected status %d", e.method, e.path, e.status)
	}
	return fmt.Sprintf("%s %s: unexpected status %d: %s", e.method, e.path, e.status, e.body)
}

func (e *apiError) NotFound() bool { return e.status == http.StatusNotFound }
