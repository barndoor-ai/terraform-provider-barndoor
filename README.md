# Terraform Provider for Barndoor

Manage [Barndoor AI](https://barndoor.ai) platform resources as code.

> **Status: early development.** The provider is being built one resource at a
> time. The first resource — audit **log export** — is in progress; this initial
> release establishes the provider, authentication, and release pipeline.

Built on the [Terraform Plugin Framework](https://github.com/hashicorp/terraform-plugin-framework).

## Using the provider

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
`BARNDOOR_CLIENT_SECRET`, `BARNDOOR_ORGANIZATION_ID`.

The provider authenticates with the OAuth2 **`client_credentials`** grant
against `token_url`, using a Barndoor service-account credential scoped to your
organization.

## Requirements

- [Terraform](https://developer.hashicorp.com/terraform/downloads) >= 1.0
- [Go](https://golang.org/doc/install) >= 1.24 (to build from source)

## Building

```shell
go install
```

## Developing

- `make generate` — regenerate documentation (`tfplugindocs`) and license headers.
- `make testacc` — run the acceptance suite (`TF_ACC`); requires a reachable
  Barndoor environment and a service-account credential.
  **Acceptance tests create real resources.**

## License

[MPL-2.0](LICENSE)
