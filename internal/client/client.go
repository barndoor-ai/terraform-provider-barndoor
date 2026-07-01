// Copyright (c) Barndoor AI, Inc.
// SPDX-License-Identifier: MIT

// Package client is a small authenticated HTTP client for the Barndoor public
// API. It obtains a Keycloak access token via the OAuth2 client_credentials
// grant, caches it until shortly before expiry, and attaches it as a Bearer
// token on every request.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// refreshBuffer re-mints the token this long before its real expiry so a
// long-running apply never presents a just-expired token.
const refreshBuffer = 30 * time.Second

// Config holds the provider-supplied connection and credential settings.
type Config struct {
	BaseURL        string
	TokenURL       string
	ClientID       string
	ClientSecret   string
	OrganizationID string

	// HTTPClient is optional; New installs a sane default when nil.
	HTTPClient *http.Client
}

// Client is an authenticated client for the Barndoor public API.
type Client struct {
	cfg        Config
	httpClient *http.Client

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// New builds a Client from cfg.
func New(cfg Config) *Client {
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{cfg: cfg, httpClient: hc}
}

// OrganizationID returns the org the credential is scoped to, used to build
// tenant-scoped resource paths.
func (c *Client) OrganizationID() string { return c.cfg.OrganizationID }

// accessToken returns a valid bearer token, minting a fresh one via the
// client_credentials grant when the cached token is missing or near expiry.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.token != "" && time.Now().Before(c.expiry.Add(-refreshBuffer)) {
		return c.token, nil
	}

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.cfg.ClientID},
		"client_secret": {c.cfg.ClientSecret},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request to %s: %w", c.cfg.TokenURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Surface up to 1KB of the error body so the caller sees what the
		// token endpoint objected to (invalid_client, unauthorized_client, …).
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("token request failed: %s from %s: %s", resp.Status, c.cfg.TokenURL, strings.TrimSpace(string(body)))
	}

	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("token response from %s contained no access_token", c.cfg.TokenURL)
	}

	c.token = tr.AccessToken
	c.expiry = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return c.token, nil
}

// Do issues an authenticated request to path (resolved against BaseURL) and
// returns the raw response. The caller owns and must close the response body.
func (c *Client) Do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}

	endpoint := strings.TrimSuffix(c.cfg.BaseURL, "/") + "/" + strings.TrimPrefix(path, "/")
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.httpClient.Do(req)
}
