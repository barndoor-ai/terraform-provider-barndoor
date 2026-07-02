// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

//go:build generate

package tools

import (
	_ "github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs"
)

// Copyright headers are applied by the copywrite binary (see the "generate"
// target in GNUmakefile and the setup-copywrite step in CI), not via `go run`.
// Building copywrite as a module dependency here collided with
// terraform-plugin-docs' transitive koanf v1, which broke `go generate`.

// Format Terraform code for use in documentation.
// If you do not have Terraform installed, you can remove the formatting command, but it is suggested
// to ensure the documentation is formatted properly.
//go:generate terraform fmt -recursive ../examples/

// Generate documentation.
//go:generate go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs generate --provider-dir .. -provider-name barndoor --rendered-provider-name Barndoor
