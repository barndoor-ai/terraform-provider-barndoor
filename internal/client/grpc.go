// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package client

import (
	"context"
	"fmt"
	"net/url"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Compile-time check that tokenCredentials can be attached to a channel.
var _ credentials.PerRPCCredentials = (*tokenCredentials)(nil)

// GRPCConn lazily creates and caches a single gRPC channel to the platform
// host. The channel is shared by every gRPC-backed resource: grpc.NewClient
// does not dial until the first RPC, so creating it is cheap and never blocks
// on the network.
//
// Production path: TLS against the system roots, with per-RPC bearer-token
// credentials that require transport security. Injected-dial-options path
// (cfg.GRPCDialOptions non-empty, tests only): the default TLS transport
// credentials are skipped and the per-RPC credentials tolerate an insecure
// channel, so a bufconn listener can carry authenticated RPCs.
//
// The context is accepted for signature stability (token minting happens per
// RPC, not here) and is currently unused.
func (c *Client) GRPCConn(_ context.Context) (*grpc.ClientConn, error) {
	c.grpcMu.Lock()
	defer c.grpcMu.Unlock()

	if c.grpcConn != nil {
		return c.grpcConn, nil
	}

	target, err := c.grpcTarget()
	if err != nil {
		return nil, err
	}

	var opts []grpc.DialOption
	if len(c.cfg.GRPCDialOptions) > 0 {
		// Test-only path: the caller supplies the transport (e.g. a bufconn
		// dialer with insecure credentials), so the per-RPC credentials must
		// not insist on TLS or gRPC would refuse to attach them.
		opts = append(opts, grpc.WithPerRPCCredentials(&tokenCredentials{c: c, allowInsecure: true}))
		opts = append(opts, c.cfg.GRPCDialOptions...)
	} else {
		// Production path: TLS with the system root CAs, bearer token on every
		// RPC, and transport security required.
		opts = append(opts,
			grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(nil, "")),
			grpc.WithPerRPCCredentials(&tokenCredentials{c: c}),
		)
	}

	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, fmt.Errorf("create gRPC channel to %s: %w", target, err)
	}
	c.grpcConn = conn
	return conn, nil
}

// grpcTarget resolves the gRPC dial target: the explicit GRPCTarget override
// when set, otherwise BaseURL's host with its explicit port, defaulting to
// :443 (the platform terminates gRPC on the same TLS edge as HTTPS).
func (c *Client) grpcTarget() (string, error) {
	if c.cfg.GRPCTarget != "" {
		return c.cfg.GRPCTarget, nil
	}

	u, err := url.Parse(c.cfg.BaseURL)
	if err != nil {
		return "", fmt.Errorf("derive gRPC target: parse base_url %q: %w", c.cfg.BaseURL, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("derive gRPC target: base_url %q has no host", c.cfg.BaseURL)
	}
	if u.Port() != "" {
		return u.Host, nil
	}
	return u.Host + ":443", nil
}

// tokenCredentials attaches the client's OAuth2 bearer token (minted and
// cached by accessToken) to every outgoing RPC as an `authorization` header.
type tokenCredentials struct {
	c *Client

	// allowInsecure is set only on the injected-dial-options (test) path so
	// the credentials can ride a bufconn/insecure channel; production
	// credentials always require transport security.
	allowInsecure bool
}

// GetRequestMetadata mints (or reuses) the access token and returns it as the
// standard bearer authorization header.
func (t *tokenCredentials) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	token, err := t.c.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]string{"authorization": "Bearer " + token}, nil
}

// RequireTransportSecurity reports whether the bearer token may only be sent
// over a secure channel. True in production; false only for the test path.
func (t *tokenCredentials) RequireTransportSecurity() bool {
	return !t.allowInsecure
}
