schema_version = 1

project {
  license          = "MIT"
  copyright_holder = "Barndoor AI, Inc."
  copyright_year   = 2026

  header_ignore = [
    # examples used within documentation (prose)
    "examples/**",

    # GitHub issue template configuration
    ".github/ISSUE_TEMPLATE/*.yml",

    # golangci-lint tooling configuration
    ".golangci.yml",

    # GoReleaser tooling configuration
    ".goreleaser.yml",
  ]
}
