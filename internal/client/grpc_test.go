// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestGRPCTarget(t *testing.T) {
	tests := map[string]struct {
		cfg     Config
		want    string
		wantErr bool
	}{
		"https host root defaults to :443": {
			cfg:  Config{BaseURL: "https://platform.barndoor.ai"},
			want: "platform.barndoor.ai:443",
		},
		"explicit port wins over the default": {
			cfg:  Config{BaseURL: "https://platform.example.com:8443"},
			want: "platform.example.com:8443",
		},
		"http URL with port keeps host:port": {
			cfg:  Config{BaseURL: "http://localhost:9000"},
			want: "localhost:9000",
		},
		"GRPCTarget override wins over BaseURL": {
			cfg:  Config{BaseURL: "https://platform.barndoor.ai", GRPCTarget: "passthrough:///bufnet"},
			want: "passthrough:///bufnet",
		},
		"unparsable base URL is an error": {
			cfg:     Config{BaseURL: "://nope"},
			wantErr: true,
		},
		"base URL without a host is an error": {
			cfg:     Config{BaseURL: "platform.barndoor.ai"},
			wantErr: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := New(tc.cfg).grpcTarget()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("grpcTarget() = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("grpcTarget: %v", err)
			}
			if got != tc.want {
				t.Errorf("grpcTarget() = %q, want %q", got, tc.want)
			}
		})
	}
}

// newTokenTestClient builds a client whose TokenURL points at an httptest
// token endpoint returning a fixed access token.
func newTokenTestClient(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "grpc-test-token", "expires_in": 3600})
	}))
	t.Cleanup(srv.Close)
	return New(Config{
		BaseURL:      srv.URL,
		TokenURL:     srv.URL + "/token",
		ClientID:     "id",
		ClientSecret: "secret",
	})
}

func TestTokenCredentials_GetRequestMetadata(t *testing.T) {
	c := newTokenTestClient(t)

	md, err := (&tokenCredentials{c: c}).GetRequestMetadata(context.Background())
	if err != nil {
		t.Fatalf("GetRequestMetadata: %v", err)
	}
	if got, want := md["authorization"], "Bearer grpc-test-token"; got != want {
		t.Errorf("authorization = %q, want %q", got, want)
	}
}

func TestTokenCredentials_RequireTransportSecurity(t *testing.T) {
	if !(&tokenCredentials{}).RequireTransportSecurity() {
		t.Error("production credentials must require transport security")
	}
	if (&tokenCredentials{allowInsecure: true}).RequireTransportSecurity() {
		t.Error("test-path credentials must not require transport security")
	}
}

func TestGRPCConn_CachesTheChannel(t *testing.T) {
	// grpc.NewClient does not dial, so no server is needed; the injected
	// insecure credentials exercise the test-only dial-option path.
	c := New(Config{
		GRPCTarget:      "passthrough:///bufnet",
		GRPCDialOptions: []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())},
	})

	conn1, err := c.GRPCConn(context.Background())
	if err != nil {
		t.Fatalf("GRPCConn: %v", err)
	}
	t.Cleanup(func() { _ = conn1.Close() })

	conn2, err := c.GRPCConn(context.Background())
	if err != nil {
		t.Fatalf("GRPCConn (second call): %v", err)
	}
	if conn1 != conn2 {
		t.Error("GRPCConn must cache and return the same channel")
	}
}
