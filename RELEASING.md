# Releasing terraform-provider-barndoor

This is the runbook for publishing a versioned, GPG-signed release of the
provider to the [Terraform Registry](https://registry.terraform.io).

The release rails already exist in the repo:

- [`.github/workflows/release.yml`](.github/workflows/release.yml) — runs on a
  pushed `v*` tag: imports the GPG key and runs GoReleaser.
- [`.goreleaser.yml`](.goreleaser.yml) — cross-compiles the provider, builds the
  release archives, and GPG-signs the `SHA256SUMS`.
- [`terraform-registry-manifest.json`](terraform-registry-manifest.json) —
  declares Terraform protocol `6.0`.

> [!IMPORTANT]
> The one-time setup steps below — **adding repo secrets, flipping the repo
> public, registering the GPG key, and cutting the first tag** — are owner
> actions with lasting/outward-facing effects. They are documented here so they
> are turnkey, but **a human owner must perform them.** Do not automate them.

---

## One-time setup (owner)

### 1. Decide the Registry namespace

A provider's Registry address is `registry.terraform.io/<namespace>/<name>`,
where:

- `<namespace>` is the **GitHub org or user that owns the repo** (the Registry
  derives it from the repo owner when you publish — you do not get to type an
  arbitrary value).
- `<name>` is taken from the repo name `terraform-provider-<name>`, so this repo
  yields `<name> = barndoor`.

That leaves a real decision about the namespace:

| Option | Registry source | What it requires |
| --- | --- | --- |
| **A — `barndoor-ai/barndoor`** (recommended) | `barndoor-ai/barndoor` | Nothing extra. The repo already lives under the `barndoor-ai` org, and the README/examples already use this source. Publish as-is. |
| **B — `barndoor/barndoor`** | `barndoor/barndoor` | A GitHub **organization named `barndoor`** that owns the repo. You would have to create/secure that org and transfer (or fork-and-republish) the repo into it. Cleaner vanity address, more identity to manage. |

**Recommendation: ship `barndoor-ai/barndoor` (Option A).** It matches the org
the code already lives in and the source string already published in the README
and examples, and it unblocks the first release with zero extra identity work. A
move to `barndoor/barndoor` later is possible but means a new Registry namespace
(a different `source`, so a breaking change for early adopters) — decide before
the first release if the vanity name matters, otherwise take Option A and move
on.

If you choose B, do the org creation + repo transfer **before** any of the steps
below, so every later step targets the final owner.

### 2. Create a GPG signing key

The Registry verifies provider archives against a GPG public key you register.
GoReleaser signs the `SHA256SUMS` file with the matching private key.

If you do not already have a release signing key, generate one (RSA 4096, no
expiry is fine for a CI signing key; **use a passphrase**):

```shell
gpg --full-generate-key   # choose RSA and RSA, 4096 bits, real name "Barndoor AI", an ops email

# Find the key id (the long hex after rsa4096/) and its fingerprint:
gpg --list-secret-keys --keyid-format=long
```

Export both halves:

```shell
# Public key — you paste this into the Terraform Registry (step 4).
gpg --armor --export "<KEY_ID>" > barndoor-provider-gpg.pub.asc

# Private key — this becomes the GPG_PRIVATE_KEY repo secret (step 3).
gpg --armor --export-secret-keys "<KEY_ID>" > barndoor-provider-gpg.private.asc
```

Store the private key and passphrase in the team password manager. Never commit
either file; delete the exported files once the secrets are set.

### 3. Add the repo secrets

`release.yml` reads exactly two secrets (the workflow derives the GPG
*fingerprint* automatically from the imported key, so you do **not** set a
fingerprint secret):

| Secret | Value |
| --- | --- |
| `GPG_PRIVATE_KEY` | Contents of `barndoor-provider-gpg.private.asc` (the full ASCII-armored block). |
| `PASSPHRASE` | The passphrase protecting that key. |

```shell
gh secret set GPG_PRIVATE_KEY < barndoor-provider-gpg.private.asc
gh secret set PASSPHRASE      # paste when prompted
```

### 4. Make the repo public and register it with the Registry

The Terraform Registry only publishes **public** GitHub repositories.

1. Flip the repo to public (GitHub → Settings → General → Danger Zone → Change
   visibility). Confirm no secrets are in the git history first.
2. Sign in to <https://registry.terraform.io> with the GitHub account/org from
   step 1 and authorize the Terraform Registry GitHub App.
3. **Publish → Provider**, then select `terraform-provider-barndoor`.
4. Add the **GPG public key** from step 2 (paste the contents of
   `barndoor-provider-gpg.pub.asc`) under the namespace's signing keys.

After this, the Registry watches the repo for new `v*` release tags.

---

## Cutting a release

Once the one-time setup is done, each release is just a tag.

### Pre-flight (safe to run locally; no side effects)

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

### Tag and push

1. Update [`CHANGELOG.md`](CHANGELOG.md): rename the `Unreleased` heading to the
   version and date, and confirm the entries are accurate. Commit on `main`.
2. Choose a semantic version with a leading `v` (the workflow triggers on `v*`).
   The first release is `v0.1.0`.
3. Tag a commit on `main` and push the tag:

   ```shell
   git checkout main && git pull
   git tag -a v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```

Pushing the tag starts [`release.yml`](.github/workflows/release.yml):
GoReleaser cross-compiles for the configured OS/arch matrix, builds the zip
archives + `SHA256SUMS`, GPG-signs the checksums, and creates a **GitHub
Release** with all artifacts plus the registry manifest. The Terraform Registry
detects the new release tag and publishes it (usually within a few minutes).

### Verify

- The GitHub Release has `*_SHA256SUMS`, `*_SHA256SUMS.sig`, the per-platform
  `*.zip` archives, and `*_manifest.json`.
- The version appears at
  `https://registry.terraform.io/providers/<namespace>/barndoor/latest`.
- A smoke test in a scratch dir:

  ```hcl
  terraform {
    required_providers {
      barndoor = {
        source  = "<namespace>/barndoor"
        version = "0.1.0"
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
