# Terraform Provider for Barndoor

[![Terraform Registry](https://img.shields.io/badge/Terraform%20Registry-barndoor--ai%2Fbarndoor-7B42BC?logo=terraform)](https://registry.terraform.io/providers/barndoor-ai/barndoor/latest)
[![Tests](https://github.com/barndoor-ai/terraform-provider-barndoor/actions/workflows/test.yml/badge.svg)](https://github.com/barndoor-ai/terraform-provider-barndoor/actions/workflows/test.yml)
[![Release](https://img.shields.io/github/v/release/barndoor-ai/terraform-provider-barndoor?sort=semver)](https://github.com/barndoor-ai/terraform-provider-barndoor/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Manage [Barndoor AI](https://barndoor.ai) platform resources as code, built on
the [Terraform Plugin Framework](https://github.com/hashicorp/terraform-plugin-framework).

> **Status: early development.** The provider is being built one resource at a
> time. The first resource — audit **log export** — is in progress; this initial
> release establishes the provider, authentication, and release pipeline.

## Documentation

Full, generated provider documentation lives on the Terraform Registry:
**[registry.terraform.io/providers/barndoor-ai/barndoor/latest/docs](https://registry.terraform.io/providers/barndoor-ai/barndoor/latest/docs)**.
The source for those pages is in [`docs/`](docs/), with runnable
[`examples/`](examples/).

## Quick start

```hcl
terraform {
  required_providers {
    barndoor = {
      source = "barndoor-ai/barndoor"
    }
  }
}

provider "barndoor" {
  base_url        = "https://platform.barndoor.ai/api/system-management/public/v1"
  token_url       = "https://auth.barndoor.ai/realms/barndoor/protocol/openid-connect/token"
  client_id       = var.barndoor_client_id
  client_secret   = var.barndoor_client_secret # prefer BARNDOOR_CLIENT_SECRET
  organization_id = var.barndoor_organization_id
}
```

Every setting can also be supplied via environment variable:
`BARNDOOR_BASE_URL`, `BARNDOOR_TOKEN_URL`, `BARNDOOR_CLIENT_ID`,
`BARNDOOR_CLIENT_SECRET`, `BARNDOOR_ORGANIZATION_ID`. Prefer the environment
variable for `client_secret` rather than committing it to configuration.

The provider authenticates with the OAuth2 **`client_credentials`** grant
against `token_url`, using a Barndoor service-account credential scoped to your
organization. Create one in the Barndoor app under **Settings → API Tokens**.

## Requirements

- [Terraform](https://developer.hashicorp.com/terraform/downloads) >= 1.0
- [Go](https://go.dev/dl/) matching [`go.mod`](go.mod) (currently 1.25.x) — only to build from source

## Developing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the full loop. In short:

- `make build` / `go install` — build the provider.
- `make test` — unit tests (no network, no credentials).
- `make dev-install` — build, install, and print the `dev_overrides` block for
  `~/.terraformrc` so `terraform plan`/`apply` use your local build (no registry,
  no `terraform init`).
- `make generate` — regenerate docs (`tfplugindocs`) and license headers.
- `make testacc` — acceptance tests (`TF_ACC`) against a real Barndoor
  environment and service-account credential. They **skip cleanly** when the
  `BARNDOOR_*` connection variables are unset, and the create/update/destroy
  tests run only against an explicitly named **disposable** org. See the safety
  notes in [`internal/provider/acceptance_test.go`](internal/provider/acceptance_test.go).

## Releasing

See [RELEASING.md](RELEASING.md) for the signed-release + Terraform Registry
publishing runbook.

## Support

- **Bugs and feature requests:** open a
  [GitHub issue](https://github.com/barndoor-ai/terraform-provider-barndoor/issues).
- **Security vulnerabilities:** please follow the process in
  [SECURITY.md](SECURITY.md) — do not open a public issue.
- **Barndoor product/account questions:** contact Barndoor support.

This provider is maintained by Barndoor AI on a best-effort basis.

## License

[MIT](LICENSE)
