# Contributing to MemoryStore

Thank you for your interest in contributing to **MemoryStore**! We welcome bug reports, feature requests, documentation improvements, performance optimizations, and pull requests from the community.

## Code of Conduct

This project and everyone participating in it is governed by our [Code of Conduct](CODE_OF_CONDUCT.md). By participating, you are expected to uphold this code.

## Prerequisites

- **Go 1.27+** (matching `go.mod`)
- **golangci-lint v2** (`v2.15.0+`) for local linting
- **Git** and **Make**
- Optional: **`benchstat`** (`go install golang.org/x/perf/cmd/benchstat@latest`) for comparing benchmarks

## Getting Started

1. **Fork** the repository on GitHub and clone your fork locally:

   ```bash
   git clone https://github.com/<your-username>/MemoryStore.git
   cd MemoryStore
   git remote add upstream https://github.com/BryceWayne/MemoryStore.git
   ```

2. **Create a feature branch** from the default branch (`root`):

   ```bash
   git checkout root
   git pull upstream root
   git checkout -b feat/your-feature-name
   ```

   > **Note:** This repository's default branch is **`root`** (not `main`). Always branch from and open pull requests against `root`.

3. **Verify your environment:**

   ```bash
   make all
   ```

## Development Workflow & Standards

### Code Style & Design Guidelines
- Follow standard Go idioms ([Effective Go](https://go.dev/doc/effective_go) and [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments)).
- Format all code with `gofmt` (`make fmt`).
- Ensure `go vet ./...` and `golangci-lint run ./...` pass with zero warnings (`make vet lint`).
- **Keep dependencies minimal:** Prefer the Go standard library (such as `encoding/json/v2`, `hash/maphash`, `uuid`, `sync/atomic`) over third-party libraries for core cache functionality.
- **Maintain backward compatibility:** Do not break exported function signatures or documented behavior without prior discussion in an issue and explicit versioning in `CHANGELOG.md`.
- **Document exported symbols:** Every exported type, function, method, and constant must have a clear Go doc comment. Include runnable `Example` tests in `memorystore/example_test.go` for new user-facing APIs.

### Testing Requirements
- All changes must include unit tests covering both normal paths and edge cases (prefer table-driven tests).
- All tests must pass under the Go race detector:

  ```bash
  go test -count=1 -race -cover ./...
  ```

- Avoid leaking goroutines; verify lifecycle cleanup on `Stop()` and `Close()`.

### Benchmarking Guidelines
For performance-related PRs, run before/after benchmarks with `-benchmem` and `-count=6` and include `benchstat` output in your PR description:

```bash
go test -run='^$' -bench=. -benchmem -count=6 ./memorystore > old.txt
# apply changes
go test -run='^$' -bench=. -benchmem -count=6 ./memorystore > new.txt
benchstat old.txt new.txt
```

### Commit Messages
Write clear, concise commit messages using the imperative mood:

- `Add TTL jitter option to SetMulti`
- `Fix race condition during PubSub unsubscribe`
- `Optimize shard lookup allocation in GetMulti`

### Changelog Entries
For user-facing changes, bug fixes, or performance improvements, add a concise entry under the `## [Unreleased]` section of [CHANGELOG.md](CHANGELOG.md).

## Pull Request Process

1. Rebase onto the latest `upstream/root` if needed.
2. Run the full verification suite locally:
   ```bash
   make all
   ```
3. Push your branch to your fork and open a Pull Request targeting **`root`**.
4. Fill out the [Pull Request template](.github/PULL_REQUEST_TEMPLATE.md) clearly, linking any related issues and including benchmark results when relevant.
5. Ensure all GitHub Actions CI checks pass and respond to review feedback.

## Reporting Bugs & Requesting Features

- **Bugs:** Open an issue using the [Bug Report template](.github/ISSUE_TEMPLATE/bug_report.md) with a minimal reproducible example and your Go/OS version.
- **Features / Proposals:** For significant API or architectural changes, open a [Feature Request](.github/ISSUE_TEMPLATE/feature_request.md) first so we can align on design before implementation.
- **Security Issues:** Do **not** open a public issue. See [SECURITY.md](SECURITY.md) for private disclosure instructions.

## Release Process (Maintainers)

MemoryStore follows [Semantic Versioning](https://semver.org/):

1. Ensure `root` is green on CI and all release changes are documented in [CHANGELOG.md](CHANGELOG.md).
2. Move items from `[Unreleased]` into a versioned heading (e.g., `## [1.0.0] - YYYY-MM-DD`) and update comparison links at the bottom of `CHANGELOG.md`.
3. Create and push an annotated Git tag matching the release version:
   ```bash
   git tag -a v1.0.0 -m "Release v1.0.0"
   git push origin v1.0.0
   ```
4. The automated [Release workflow](.github/workflows/release.yml) validates the build/tests and publishes the GitHub Release.
