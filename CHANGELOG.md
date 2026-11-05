# Changelog

All notable changes to this project are documented in this file.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- MIT `LICENSE`.
- Dependabot configuration for GitHub Actions and Go modules.
- This changelog.

### Changed

- Minimum Go version is now 1.26; CI tests Go 1.26 and 1.27.
- YAML parsing uses `go.yaml.in/yaml/v3` v3.0.6 instead of the archived
  `gopkg.in/yaml.v3`.
- CI uses `actions/checkout@v7` and `actions/setup-go@v7`.

## [0.1.0] - 2026-07-26

### Added

- Terraform plan JSON parser that keeps only resource addresses, types and
  actions, with risk ordering and replacement detection.
- `.tf-ledger.yml` policy: protected resources gated behind a written
  acknowledgement from someone other than the author.
- Risk-ordered sticky pull request comment (Markdown or JSON).
- Append-only, hash-chained ledger with `verify`, and CSV/JSON export with
  date, repo, action and protection filters.
- CLI commands `summarize`, `gate`, `record`, `export` and `verify`.

[Unreleased]: https://github.com/moveeeax/tf-change-ledger/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/moveeeax/tf-change-ledger/releases/tag/v0.1.0
