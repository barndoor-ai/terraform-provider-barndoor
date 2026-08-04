#!/bin/sh
# Copyright Barndoor AI, Inc. 2026
# SPDX-License-Identifier: MIT

# Validate every Terraform configuration under examples/ against the locally
# built provider, so broken example HCL can't ship in the docs (a published
# example once used block syntax for a nested attribute — see the 0.2.0
# BUG FIXES entry in CHANGELOG.md).
#
# Offline and credential-free: `terraform validate` never configures the
# provider, but it does need the provider schema, so the provider is built
# into a local filesystem mirror that `terraform init` resolves from instead
# of the registry. Each example is validated in a temp copy with a generated
# required_providers stub (tfplugindocs example snippets intentionally omit
# one), keeping the working tree clean.
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_root"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

version="99.0.0"
platform="$(go env GOOS)_$(go env GOARCH)"
mirror="$work/mirror"
bindir="$mirror/registry.terraform.io/barndoor-ai/barndoor/$version/$platform"
mkdir -p "$bindir"
echo "==> building provider into local filesystem mirror ($platform)"
go build -o "$bindir/terraform-provider-barndoor_v$version" .

cat > "$work/terraform.rc" <<EOF
provider_installation {
  filesystem_mirror {
    path    = "$mirror"
    include = ["registry.terraform.io/barndoor-ai/barndoor"]
  }
  direct {
    exclude = ["registry.terraform.io/barndoor-ai/barndoor"]
  }
}
EOF
export TF_CLI_CONFIG_FILE="$work/terraform.rc"
export TF_PLUGIN_CACHE_DIR="$work/plugin-cache"
export TF_IN_AUTOMATION=1
mkdir -p "$TF_PLUGIN_CACHE_DIR"

status=0
for dir in $(find examples -name '*.tf' -exec dirname {} \; | sort -u); do
  cfg="$work/cfg"
  rm -rf "$cfg"
  mkdir -p "$cfg"
  cp "$dir"/*.tf "$cfg"/
  cat > "$cfg/zz_generated_stub.tf" <<EOF
terraform {
  required_providers {
    barndoor = {
      source = "barndoor-ai/barndoor"
    }
  }
}
EOF
  log="$work/log"
  if { terraform -chdir="$cfg" init -backend=false -input=false &&
    terraform -chdir="$cfg" validate -no-color; } >"$log" 2>&1; then
    echo "ok   $dir"
  else
    echo "FAIL $dir"
    cat "$log"
    status=1
  fi
done
exit $status
