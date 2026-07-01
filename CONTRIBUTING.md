# Contributing

Thanks for your interest in improving the Barndoor Terraform provider! This
guide covers building the provider, running its tests, and the conventions we
follow for pull requests.

- [Prerequisites](#prerequisites)
- [Build](#build)
- [Run unit tests](#run-unit-tests)
- [Test a local build against your Barndoor account](#test-a-local-build-against-your-barndoor-account)
- [Acceptance tests](#acceptance-tests)
- [Generate docs and license headers](#generate-docs-and-license-headers)
- [Lint and format](#lint-and-format)
- [Pull requests](#pull-requests)

## Prerequisites

| Tool | Version |
|------|---------|
| [Go](https://go.dev/dl/) | matches `go.mod` (currently 1.25.x) |
| [Terraform](https://developer.hashicorp.com/terraform/downloads) | ≥ 1.0 (doc generation tracks the latest release) |

Anything that talks to the API needs a Barndoor account and a service-account
(`client_credentials`) credential scoped to an organization. Create one in the
Barndoor app under **Settings → API Tokens**; see the
[provider documentation](https://registry.terraform.io/providers/barndoor-ai/barndoor/latest/docs)
for the connection settings.

## Build

```bash
go build ./...      # compile
go install          # install the provider binary into $GOBIN
# or simply:
make build
```

## Run unit tests

Unit tests need no network and no credentials:

```bash
make test           # go test -v -cover -timeout=120s -parallel=10 ./...
```

## Test a local build against your Barndoor account

To exercise a locally-built provider with real `terraform plan`/`apply` without
publishing it, use Terraform's
[`dev_overrides`](https://developer.hashicorp.com/terraform/cli/config/config-file#development-overrides-for-provider-developers):

```bash
make dev-install    # go install + prints the ~/.terraformrc block to paste
```

Paste the printed block into `~/.terraformrc` (or a file referenced by
`TF_CLI_CONFIG_FILE`), then point the provider at your Barndoor environment and
credential and run Terraform **without `terraform init`** — dev overrides resolve
the provider from your local build:

```bash
export BARNDOOR_BASE_URL=https://platform.barndoor.ai/api/system-management/public/v1
export BARNDOOR_TOKEN_URL=https://auth.barndoor.ai/realms/barndoor/protocol/openid-connect/token
export BARNDOOR_CLIENT_ID=...
export BARNDOOR_CLIENT_SECRET=...
export BARNDOOR_ORGANIZATION_ID=...

terraform plan      # no init; the "dev overrides are in effect" warning is expected
```

Rebuild (`make dev-install`) after each change. Remove the `dev_overrides` block
when you're done — while it is present Terraform ignores normal resolution for
this provider in every workspace on your machine.

## Acceptance tests

Acceptance tests run real Terraform against a real Barndoor environment. They
are gated on `TF_ACC` **and** the `BARNDOOR_*` connection variables, so they
**skip cleanly** when those are unset (as do `make test` and CI):

```bash
export TF_ACC=1
export BARNDOOR_BASE_URL=...        # as above
export BARNDOOR_TOKEN_URL=...
export BARNDOOR_CLIENT_ID=...
export BARNDOOR_CLIENT_SECRET=...
export BARNDOOR_ORGANIZATION_ID=...
make testacc
```

> [!WARNING]
> Acceptance tests create and modify real resources and may incur costs.
> `TestAccConnectivity` is read-only and safe against any org. The
> create/update/destroy tests run **only** when you set
> `BARNDOOR_ACC_TEST_ORGANIZATION_ID` to a **disposable** org whose export
> configuration may be freely changed; they skip otherwise. As a safety guard,
> set `BARNDOOR_ACC_PROTECTED_ORGANIZATION_ID` to an org that must never be
> touched (e.g. production) and the destructive tests refuse to run against it.
> See the header of
> [`internal/provider/acceptance_test.go`](internal/provider/acceptance_test.go).

## Generate docs and license headers

The resource/data-source docs under [`docs/`](docs/) and the SPDX license
headers are generated. After changing schema, descriptions, or examples,
regenerate and commit the result — CI runs `make generate` and fails if it
produces a diff:

```bash
make generate
```

This runs [`tfplugindocs`](https://github.com/hashicorp/terraform-plugin-docs)
(which needs `terraform` on your `PATH`) and
[`copywrite`](https://github.com/hashicorp/copywrite).

## Lint and format

```bash
make fmt            # gofmt -s -w
golangci-lint run   # or: make lint
```

## Pull requests

- Keep changes focused; open an issue first for anything non-trivial.
- Use [Conventional Commits](https://www.conventionalcommits.org) for commit and
  PR titles (e.g. `feat:`, `fix:`, `docs:`, `chore:`).
- Add or update tests for your change, and run `make test`,
  `golangci-lint run`, and `make generate` before pushing.
- Add a bullet to the `Unreleased` section of [CHANGELOG.md](CHANGELOG.md) for
  any user-facing change.

By contributing, you agree that your contributions will be licensed under the
[MIT License](LICENSE).
