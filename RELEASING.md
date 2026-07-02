# Releasing terraform-provider-barndoor

This is the runbook for publishing a versioned, GPG-signed release of the
provider to the [Terraform Registry](https://registry.terraform.io). The
provider is published at
[`barndoor-ai/barndoor`](https://registry.terraform.io/providers/barndoor-ai/barndoor/latest).

The release rails live in the repo:

- [`.github/workflows/release.yml`](.github/workflows/release.yml) — runs on a
  pushed `v*` tag: imports the GPG key and runs GoReleaser.
- [`.goreleaser.yml`](.goreleaser.yml) — cross-compiles the provider, builds the
  release archives, and GPG-signs the `SHA256SUMS`.
- [`terraform-registry-manifest.json`](terraform-registry-manifest.json) —
  declares Terraform protocol `6.0`.

The GPG signing key and the two repo secrets it needs (`GPG_PRIVATE_KEY` and
`PASSPHRASE`) are already configured, and the Registry is watching the repo for
new `v*` tags. Each release is just a tag.

---

## Pre-flight (safe to run locally; no side effects)

```shell
go build ./... && go vet ./... && go test ./...        # unit tests green
golangci-lint run                                       # lint clean
make generate && git diff --exit-code                   # docs/headers committed & idempotent
goreleaser check                                        # .goreleaser.yml is valid
goreleaser release --snapshot --clean --skip=sign       # full local build (no publish)
```

`goreleaser check` validates the config against the installed GoReleaser version.
`--snapshot --clean` runs the entire build/cross-compile/archive/checksum
pipeline into `dist/` (gitignored) without tagging or publishing — the best local
proof the release will succeed. `--skip=sign` is needed locally because signing
requires the GPG key + `GPG_FINGERPRINT`, which only exist in CI (the release
workflow derives the fingerprint from the imported `GPG_PRIVATE_KEY`); drop the
flag only if you have the key imported and `GPG_FINGERPRINT` exported. (Install
GoReleaser with `go install github.com/goreleaser/goreleaser/v2@latest` if
needed; the config is `version: 2`.)

> [!NOTE]
> The GoReleaser builds run a `go mod tidy` `before` hook (see
> [`.goreleaser.yml`](.goreleaser.yml)), so a local `--snapshot` run can modify
> `go.mod`/`go.sum` in your working tree. That is expected for the snapshot;
> review and **discard any resulting churn** (`git checkout -- go.mod go.sum`)
> rather than committing it as part of the release.

## Tag and push

1. Update [`CHANGELOG.md`](CHANGELOG.md): rename the `Unreleased` heading to the
   version and date, and confirm the entries are accurate. Commit on `main`.
2. Choose the next semantic version with a leading `v` (the workflow triggers on
   `v*`), following semver relative to the last release.
3. Tag a commit on `main` and push the tag:

   ```shell
   git checkout main && git pull
   git tag -a vX.Y.Z -m "vX.Y.Z"
   git push origin vX.Y.Z
   ```

Pushing the tag starts [`release.yml`](.github/workflows/release.yml):
GoReleaser cross-compiles for the configured OS/arch matrix, builds the zip
archives + `SHA256SUMS`, GPG-signs the checksums, and creates a **GitHub
Release** with all artifacts plus the registry manifest. The Terraform Registry
detects the new release tag and publishes it (usually within a few minutes).

## Verify

- The GitHub Release has `*_SHA256SUMS`, `*_SHA256SUMS.sig`, the per-platform
  `*.zip` archives, and `*_manifest.json`.
- The version appears at
  <https://registry.terraform.io/providers/barndoor-ai/barndoor/latest>.
- A smoke test in a scratch dir:

  ```hcl
  terraform {
    required_providers {
      barndoor = {
        source  = "barndoor-ai/barndoor"
        version = "X.Y.Z"
      }
    }
  }
  ```

  `terraform init` should download and verify the signed provider.

---

## Rollback

A published Registry version cannot be edited, only superseded. To withdraw a
bad release, delete the GitHub Release + tag and publish a higher patch version;
the Registry stops offering a deleted version. Because consumers pin versions,
prefer rolling **forward** with a fixed `vX.Y.(Z+1)` over deletion.
