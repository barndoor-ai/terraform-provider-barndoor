# Contributing

This provider is **not published** to the Terraform Registry yet. The supported
way to develop and test it is against a **local Barndoor platform** brought up
with `make tilt`, using Terraform's local-provider mechanism
([`dev_overrides`](https://developer.hashicorp.com/terraform/cli/config/config-file#development-overrides-for-provider-developers))
— no registry, no `terraform init`, no release.

This guide is the end-to-end loop. It assumes you have a checkout of
`barndoor-ai/bdai-platform` next to this repo and that `make tilt` works there.

- [Prerequisites](#prerequisites)
- [TL;DR](#tldr)
- [1. Bring up the local platform](#1-bring-up-the-local-platform)
- [2. Trust the local CA (read this first)](#2-trust-the-local-ca-read-this-first)
- [3. Get a local IaC credential](#3-get-a-local-iac-credential)
- [4. Build the provider and wire `dev_overrides`](#4-build-the-provider-and-wire-dev_overrides)
- [5. Write a config, apply, and verify](#5-write-a-config-apply-and-verify)
- [Fallback: skip TLS with port-forwards](#fallback-skip-tls-with-port-forwards)
- [Gotchas and known limitations](#gotchas-and-known-limitations)

---

## Prerequisites

| Tool | Version | Notes |
|------|---------|-------|
| Go | ≥ 1.24 (`go.mod` pins 1.25.x) | builds the provider |
| Terraform | ≥ 1.0 | `dev_overrides` needs nothing newer |
| `bdai-platform` checkout | — | provides `make tilt` and `make trust-certs` |
| macOS | — | the `make trust-certs` helper is macOS-only; see [Fallback](#fallback-skip-tls-with-port-forwards) for Linux / dev-container |

You do **not** need Tailscale for local testing (unlike the shared `dev`
environment — everything is on `localhost` / `*.barndoorlocal.com`).

## TL;DR

```bash
# In your bdai-platform checkout: bring up the platform and trust the local CA.
make tilt           # local k3d cluster + charts/barndoor umbrella chart
make trust-certs    # one-time: trust the *.barndoorlocal.com CA (sudo)

# In this repo: build the provider and tell Terraform to use the local binary.
make dev-install    # go install + prints the ~/.terraformrc dev_overrides block
#   …paste that block into ~/.terraformrc…

# Provide the credential (see section 3) and run the loop from examples/local/.
export BARNDOOR_CLIENT_ID=barndoor-log-export-bootstrap
export BARNDOOR_CLIENT_SECRET=...           # from Keycloak admin console
export BARNDOOR_ORGANIZATION_ID=...         # decode the token; see section 3
cd examples/local
terraform plan      # NO `terraform init` — dev_overrides handles resolution
terraform apply
```

## 1. Bring up the local platform

From your `bdai-platform` checkout:

```bash
make tilt
```

This creates the local k3d cluster, installs the `charts/barndoor` umbrella
chart, and seeds the setup organization. The provider talks to two of those
services through the local Envoy edge (`platform-lb`), which terminates TLS on
`:443` for `*.barndoorlocal.com` using the local CA.

**Endpoints the provider uses** (all resolve to `127.0.0.1` via `/etc/hosts`,
which `make tilt` manages):

| Purpose | Local URL |
|---------|-----------|
| OIDC token endpoint (`token_url`) | `https://auth.barndoorlocal.com/realms/barndoor/protocol/openid-connect/token` |
| SMS public export API (`base_url`) | `https://mcp.barndoorlocal.com/api/system-management/public/v1` |
| Platform UI (API Tokens page, Keycloak admin) | `https://app.barndoorlocal.com` / `https://auth.barndoorlocal.com/admin/` |

The edge route `/api/system-management/public/v1` is rewritten to `/public/v1`
and proxied straight to `system-management-service` (not via the portal BFF).
The provider's `barndoor_log_export` resource appends
`exports/{organization_id}/{export_type}/…` to `base_url`, so the full path of,
e.g., a destination write is
`…/public/v1/exports/<org>/datadog-json/destination`.

**Services that must be `Ready` in the Tilt UI:**

- `keycloak` (+ `keycloak-db`) — issues the `client_credentials` token
- `system-management-service` (+ `system-management-service-db`) — serves the
  export API and **auto-provisions** a `datadog-json` export (disabled) for the
  setup org via its organization poller, so the export row already exists
- `identity-service` — source of org/credential records (and the dynamic
  credential API in section 3, Option B)
- `platform-lb` — the Envoy edge that terminates TLS and routes the two hosts
- `audit-consumer` — the export delivery pipeline (only needed if you actually
  enable streaming to a real bucket; not needed for the `enabled = false` loop)

## 2. Trust the local CA (read this first)

This is the **#1 thing that bites you**. The provider's HTTP client is the
stock Go client — it validates TLS against the OS trust store and has **no
`insecure_skip_verify` and no custom-CA option** (see
[Gotchas](#gotchas-and-known-limitations)). The `*.barndoorlocal.com` certs are
signed by a local self-signed CA (`CN=Barndoor Local CA`), so until that CA is
trusted by the OS, every call fails with `x509: certificate signed by unknown
authority`.

`make tilt` creates and mounts the certs but does **not** trust the CA (that
needs `sudo`). Run the dedicated target once:

```bash
# In your bdai-platform checkout:
make trust-certs
```

It adds `tls-certs/local-ca.crt` to the macOS **System** keychain as a trusted
root. Go's TLS verifier on macOS consults the system keychain, so once this is
done the provider trusts `https://auth.barndoorlocal.com` and
`https://mcp.barndoorlocal.com` with **no code change**. (Your browser will also
stop warning on `https://app.barndoorlocal.com`.)

Not on macOS, or you'd rather not modify your trust store? Use the
[port-forward fallback](#fallback-skip-tls-with-port-forwards).

## 3. Get a local IaC credential

The provider authenticates with a Keycloak `client_credentials` service account
scoped to one organization. You need three values: `client_id`,
`client_secret`, and the `organization_id` the credential is bound to. Two ways
to get one locally.

### Option A — static bootstrap client (fastest)

A fixed bootstrap client (`barndoor-log-export-bootstrap`, `admin` role, bound
to the setup org) is gated behind a Helm value, off by default. Enable it for
local dev in your `bdai-platform` `values.override.yaml`:

```yaml
keycloak:
  logExportBootstrapClient:
    enabled: true
```

Re-run `make tilt`. The Keycloak bootstrap job creates the client. Its secret is
Keycloak-generated and not stored in any Helm value, so read it from the admin
console:

1. Open `https://auth.barndoorlocal.com/admin/` and sign in with the local
   superadmin (`test.superadmin@barndoorlocal.com` / `test` by default — these
   are the chart's `global.auth.keycloak.adminUsername`/`adminPassword`).
2. Realm **`barndoor`** → **Clients** → **barndoor-log-export-bootstrap** →
   **Credentials** → copy the **Client secret**.

```bash
export BARNDOOR_CLIENT_ID=barndoor-log-export-bootstrap
export BARNDOOR_CLIENT_SECRET=<paste from Credentials tab>
```

### Option B — dynamic provisioning (production-like, self-service)

The platform exposes self-service IaC credentials, the same path a real
customer uses:

- **UI:** sign in to `https://app.barndoorlocal.com` as an org admin and go to
  **Settings → API Tokens** (`/settings/tokens`). Create/enable the Terraform
  credential and **copy the secret from the reveal dialog — it is shown once.**
  (The section is gated by the `terraform-provider` PostHog flag; enable it for
  your org if you don't see it.)
- **API:** `POST /organizations/{organization_id}/iac-credentials` on
  `identity-service` returns `{ "credential": { "client_id": … }, "client_secret": … }`
  with the secret present **only** on create/rotate. `…/rotate` mints a new one
  if you lose it.

### Get the `organization_id` (authoritative method)

`organization_id` must equal the org claim in the credential's token — a
mismatch yields 403s. Rather than guessing the UUID, **mint a token and read the
claim**:

```bash
TOKEN=$(curl -sS \
  -d grant_type=client_credentials \
  -d client_id="$BARNDOOR_CLIENT_ID" \
  -d client_secret="$BARNDOOR_CLIENT_SECRET" \
  https://auth.barndoorlocal.com/realms/barndoor/protocol/openid-connect/token \
  | jq -r .access_token)

# Print the JWT payload; the organization UUID is your organization_id.
python3 -c 'import sys,json,base64; p=sys.argv[1].split(".")[1]; print(json.dumps(json.loads(base64.urlsafe_b64decode(p+"=="*((4-len(p)%4)%4))),indent=2))' "$TOKEN"

export BARNDOOR_ORGANIZATION_ID=<the organization UUID from the claim>
```

For the static bootstrap client this is the seeded setup org (documented as
`fcdc562c-546c-4cca-8fee-e557a642dc9d` in `values.override.template.yaml`), but
**trust the token claim** over any hardcoded value — the seed UUID can drift.

## 4. Build the provider and wire `dev_overrides`

```bash
make dev-install
```

`dev-install` runs `go install` (binary lands in `go env GOBIN`, or
`$(go env GOPATH)/bin`) and prints the exact block to add to
`~/.terraformrc`:

```hcl
provider_installation {
  dev_overrides {
    "registry.terraform.io/barndoor-ai/barndoor" = "/Users/you/go/bin"
  }
  direct {}
}
```

With that in place, `terraform plan`/`apply` resolve `barndoor-ai/barndoor` to
your local binary.

**`dev_overrides` caveats:**

- **Do not run `terraform init`.** With dev overrides set, init errors (there's
  no published provider to install). Go straight to `plan`/`apply`.
- Every `plan`/`apply` prints a warning: *"Provider development overrides are in
  effect."* That's expected — it's how you know you're on the local build.
- Rebuild after each code change (`make dev-install`); Terraform always loads
  whatever binary is at that path.
- **Remove the `dev_overrides` block when you're done.** While it's present,
  Terraform ignores normal provider resolution for `barndoor-ai/barndoor` in
  *every* workspace on your machine. To keep it scoped, write the block to a
  separate file and point Terraform at it per-invocation instead of editing
  `~/.terraformrc`:
  ```bash
  TF_CLI_CONFIG_FILE=$PWD/examples/local/dev.tfrc terraform plan
  ```

## 5. Write a config, apply, and verify

A ready-to-run config lives in [`examples/local/`](examples/local/). It pins the
local `base_url`/`token_url` and configures the auto-provisioned `datadog-json`
export with a **dummy destination and `enabled = false`** — so the API stores
the config but runs **no** S3 connectivity probe (the probe only runs on
*start*). That makes it a safe smoke test with no real bucket.

```bash
cd examples/local

# Optional but recommended: prove the platform + credential + TLS independently
# of Terraform. A 200 with JSON means token_url, base_url, auth, routing and the
# CA trust are all good. (A 403 still proves connectivity — auth worked but the
# org/feature gate rejected you.)
curl -sS -H "Authorization: Bearer $TOKEN" \
  "https://mcp.barndoorlocal.com/api/system-management/public/v1/exports/$BARNDOOR_ORGANIZATION_ID/datadog-json" | jq .

terraform plan     # shows the destination it will configure; no init needed
terraform apply
```

**Verify the result landed** (any one of):

- `terraform state show barndoor_log_export.audit` — `has_credentials = true`
  and your destination fields are populated from the server read-back.
- The `curl` above, re-run — `destination.endpoint`/`bucket` now reflect what
  you applied.
- The platform UI / DB — the export's destination row for the org now exists in
  `system-management-service`'s database.

**Clean up:**

```bash
terraform destroy   # pauses the export and deletes the destination
```

(The export *row* itself stays — it's platform-provisioned, not owned by
Terraform — but its destination config is removed.)

## Fallback: skip TLS with port-forwards

If you're on Linux / in a dev container, or you don't want to trust the CA,
bypass Envoy (and TLS) entirely by port-forwarding the two services and using
`http://localhost` URLs. Note the SMS path prefix is **`/public/v1`** here (the
`/api/system-management` prefix is an edge-only route that Envoy rewrites away):

```bash
# In your bdai-platform checkout (namespace defaults to `barndoor`):
KC_PORT=$(kubectl -n barndoor get svc keycloak -o jsonpath='{.spec.ports[0].port}')
kubectl -n barndoor port-forward svc/keycloak "8081:${KC_PORT}" &
kubectl -n barndoor port-forward svc/system-management-service 8090:8090 &

export BARNDOOR_TOKEN_URL=http://localhost:8081/realms/barndoor/protocol/openid-connect/token
export BARNDOOR_BASE_URL=http://localhost:8090/public/v1
# client_id / client_secret / organization_id as in section 3
```

The token's `iss` claim still reflects Keycloak's configured hostname, so SMS
accepts it regardless of the URL you fetched it from.

## Gotchas and known limitations

- **TLS trust is mandatory on the HTTPS path, and the provider has no escape
  hatch.** `internal/client/client.go` always uses a stock `http.Client`; the
  schema exposes no `insecure_skip_verify` and no CA-bundle option. For local
  dev this is fine — `make trust-certs` (macOS) or the
  [port-forward fallback](#fallback-skip-tls-with-port-forwards) both work
  without touching the provider. **If we want friction-free local/self-signed
  testing** (e.g. a `BARNDOOR_CA_BUNDLE` env var, or an `insecure` provider
  attribute usable only against non-public hosts), that's a small, separable
  provider enhancement worth its own ticket — it is *not* required to test
  locally today.
- **DNS:** the `*.barndoorlocal.com` names are added to `/etc/hosts` (→
  `127.0.0.1`) by the Tilt flow. If a name doesn't resolve, confirm the entry
  exists and that the cluster is up.
- **Keycloak admin secret:** the bootstrap client's secret is regenerated each
  time the bootstrap client is recreated — re-copy it from the **Credentials**
  tab after any `make tilt` that re-runs the Keycloak job.
- **One-time secrets:** both the bootstrap (on recreate) and the dynamic API (on
  create/rotate) surface the secret once. Rotate to get a fresh one.
- **`organization_id` mismatch → 403.** Always derive it from the token claim
  (section 3), not from a hardcoded UUID.
- **No `terraform init`** while `dev_overrides` is active — see section 4.
