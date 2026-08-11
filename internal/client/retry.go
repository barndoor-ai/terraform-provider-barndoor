// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package client

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"
)

// Retry tuning. Package variables rather than constants so tests can shrink
// the delays; production code never mutates them.
var (
	// retryMaxAttempts is the total number of tries (first attempt + retries).
	retryMaxAttempts = 4
	// retryBaseDelay seeds the exponential backoff (doubled per attempt).
	retryBaseDelay = 500 * time.Millisecond
	// retryMaxDelay caps a single backoff sleep, including Retry-After values.
	retryMaxDelay = 8 * time.Second
)

// isIdempotent reports whether a method can be safely re-sent even when the
// previous attempt may have reached the server. PUTs on this API are full
// replacements and DELETEs tolerate repetition, so both qualify alongside the
// read-only methods. POST is the lone non-idempotent method in use.
func isIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete:
		return true
	default:
		return false
	}
}

// retryableStatus reports whether a response status warrants another attempt.
// 429 means the server refused the request without processing it, so it is
// safe to retry for every method. The transient gateway statuses (502/503/504)
// give no such guarantee — the request may have been processed before the
// failure — so they are retried only for idempotent methods.
func retryableStatus(method string, status int) bool {
	switch status {
	case http.StatusTooManyRequests:
		return true
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return isIdempotent(method)
	default:
		return false
	}
}

// retryableError reports whether a transport-level error warrants another
// attempt. Context cancellation/expiry is always terminal. A dial-phase error
// means no bytes reached the server, so it is safe to retry for every method;
// any later transport failure (reset mid-request, unexpected EOF) is retried
// only for idempotent methods because the server may have processed the
// request before the connection died.
func retryableError(method string, err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	return isIdempotent(method)
}

// retryDelay returns how long to sleep before the attempt after attempt
// (0-based): the server's Retry-After when it sent one, otherwise full-jitter
// exponential backoff. Both are capped at retryMaxDelay.
func retryDelay(attempt int, resp *http.Response) time.Duration {
	if resp != nil {
		if ra := parseRetryAfter(resp.Header.Get("Retry-After")); ra > 0 {
			return min(ra, retryMaxDelay)
		}
	}
	ceiling := min(retryBaseDelay<<attempt, retryMaxDelay)
	return time.Duration(rand.Int64N(int64(ceiling)) + 1)
}

// parseRetryAfter understands both Retry-After forms (delta-seconds and
// HTTP-date) and returns 0 for anything absent or unparseable.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return time.Until(t)
	}
	return 0
}

// doWithRetry issues a request built by newReq, retrying transient failures
// with backoff. newReq must return a fresh request (with a fresh body) per
// call. method only classifies retryability; forceIdempotent marks a POST
// whose semantics are known to be safe to re-send (the OAuth token grant).
//
// On success or a terminal (non-retryable) status the response is returned
// for the caller to interpret — retries are invisible except for latency.
// When every attempt fails with a retryable status, the LAST response is
// returned rather than an error, so callers surface the same status-based
// diagnostics as an unretried failure.
func doWithRetry(ctx context.Context, hc *http.Client, method string, forceIdempotent bool, newReq func() (*http.Request, error)) (*http.Response, error) {
	classifyMethod := method
	if forceIdempotent {
		classifyMethod = http.MethodGet
	}

	var lastErr error
	for attempt := 0; ; attempt++ {
		req, err := newReq()
		if err != nil {
			return nil, err
		}

		resp, err := hc.Do(req)
		switch {
		case err == nil && !retryableStatus(classifyMethod, resp.StatusCode):
			return resp, nil
		case err != nil && !retryableError(classifyMethod, err):
			return nil, err
		}
		lastErr = err

		if attempt == retryMaxAttempts-1 {
			// Out of attempts: surface the final outcome as if unretried.
			if err != nil {
				return nil, err
			}
			return resp, nil
		}

		delay := retryDelay(attempt, resp)
		fields := map[string]any{
			"method":  method,
			"attempt": attempt + 1,
			"delay":   delay.String(),
		}
		if resp != nil {
			fields["status"] = resp.StatusCode
			fields["url"] = req.URL.Redacted()
			// Drain so the keep-alive connection is reusable, then close.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
		} else if lastErr != nil {
			fields["error"] = lastErr.Error()
		}
		tflog.Debug(ctx, "Retrying Barndoor API request after a transient failure", fields)

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
