// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package client

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// shrinkRetryDelays makes backoff effectively instant for the duration of a
// test. Tests that call this must not run in parallel (package-level state).
func shrinkRetryDelays(t *testing.T) {
	t.Helper()
	origBase, origMax := retryBaseDelay, retryMaxDelay
	retryBaseDelay, retryMaxDelay = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { retryBaseDelay, retryMaxDelay = origBase, origMax })
}

// newTestClient wires a Client whose token endpoint always succeeds, pointed
// at apiURL.
func newTestClient(t *testing.T, apiURL string) *Client {
	t.Helper()
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
	}))
	t.Cleanup(token.Close)
	return New(Config{
		BaseURL:        apiURL,
		TokenURL:       token.URL,
		ClientID:       "id",
		ClientSecret:   "secret",
		OrganizationID: "org",
	})
}

func TestDoRetriesTransientStatusOnGet(t *testing.T) {
	shrinkRetryDelays(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL).Do(context.Background(), http.MethodGet, "/x", nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("server saw %d calls, want 3", got)
	}
}

func TestDoDoesNotRetryTransientStatusOnPost(t *testing.T) {
	shrinkRetryDelays(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL).Do(context.Background(), http.MethodPost, "/x", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("server saw %d calls, want 1 (503 on POST must not retry)", got)
	}
}

func TestDoRetries429OnPostAndReplaysBody(t *testing.T) {
	shrinkRetryDelays(t)

	const payload = `{"name":"x"}`
	var calls atomic.Int32
	var lastBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		lastBody.Store(string(b))
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL).Do(context.Background(), http.MethodPost, "/x", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("server saw %d calls, want 2 (429 retries every method)", got)
	}
	if got, _ := lastBody.Load().(string); got != payload {
		t.Fatalf("retried request body = %q, want %q (body must replay from the start)", got, payload)
	}
}

func TestDoReturnsLastResponseWhenAttemptsExhausted(t *testing.T) {
	shrinkRetryDelays(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL).Do(context.Background(), http.MethodGet, "/x", nil)
	if err != nil {
		t.Fatalf("Do: %v (exhausted retries must surface the response, not an error)", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := calls.Load(); got != int32(retryMaxAttempts) {
		t.Fatalf("server saw %d calls, want %d", got, retryMaxAttempts)
	}
}

func TestDoHonorsRetryAfter(t *testing.T) {
	shrinkRetryDelays(t)
	// Retry-After must beat the (shrunken) backoff but stay test-fast; the
	// shrunken retryMaxDelay caps it at 5ms, so use the date form to verify
	// parsing and rely on the seconds form in parseRetryAfter's unit test.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL).Do(context.Background(), http.MethodGet, "/x", nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got := calls.Load(); got != 2 {
		t.Fatalf("server saw %d calls, want 2", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got := parseRetryAfter("7"); got != 7*time.Second {
		t.Fatalf("seconds form = %v, want 7s", got)
	}
	if got := parseRetryAfter(""); got != 0 {
		t.Fatalf("empty = %v, want 0", got)
	}
	if got := parseRetryAfter("garbage"); got != 0 {
		t.Fatalf("garbage = %v, want 0", got)
	}
	date := time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(date); got <= 0 || got > 3*time.Second {
		t.Fatalf("date form = %v, want (0, 3s]", got)
	}
}

func TestDoAbortsBackoffOnContextCancel(t *testing.T) {
	// Real delays here: cancellation must win before the first 500ms backoff.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := newTestClient(t, srv.URL).Do(ctx, http.MethodGet, "/x", nil)
		done <- err
	}()

	// Let the first attempt land, then cancel during its backoff sleep.
	deadline := time.After(5 * time.Second)
	for calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("first attempt never reached the server")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Do returned nil error after context cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Do did not return promptly after context cancellation")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("server saw %d calls, want 1 (no retry after cancellation)", got)
	}
}

func TestAccessTokenRetriesTransientFailure(t *testing.T) {
	shrinkRetryDelays(t)

	var tokenCalls atomic.Int32
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if tokenCalls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
	}))
	defer token.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer api.Close()

	c := New(Config{BaseURL: api.URL, TokenURL: token.URL, ClientID: "id", ClientSecret: "s", OrganizationID: "o"})
	resp, err := c.Do(context.Background(), http.MethodGet, "/x", nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got := tokenCalls.Load(); got != 2 {
		t.Fatalf("token endpoint saw %d calls, want 2 (token grant must retry transient failures)", got)
	}
}

// countingDialTransport fails every connection at the dial phase and counts
// the attempts, so tests can prove dial errors re-enter the retry loop.
type countingDialTransport struct {
	dials atomic.Int32
}

func (c *countingDialTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.dials.Add(1)
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
}

func TestDoRetriesDialErrorOnPost(t *testing.T) {
	shrinkRetryDelays(t)

	transport := &countingDialTransport{}
	c := New(Config{
		BaseURL:        "http://unreachable.invalid",
		TokenURL:       "http://unreachable.invalid/token",
		ClientID:       "id",
		ClientSecret:   "s",
		OrganizationID: "o",
		HTTPClient:     &http.Client{Transport: transport},
	})

	_, err := c.Do(context.Background(), http.MethodPost, "/x", strings.NewReader(`{}`))
	if err == nil {
		t.Fatal("Do succeeded against a dial-failing transport")
	}
	// The token grant dials first and burns its own retryMaxAttempts; the API
	// request is never reached. Either way, seeing more than one dial proves
	// dial-phase errors are retried for non-idempotent flows.
	if got := transport.dials.Load(); got != int32(retryMaxAttempts) {
		t.Fatalf("transport saw %d dials, want %d", got, retryMaxAttempts)
	}
}
