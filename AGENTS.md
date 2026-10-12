# AGENTS.md

Guidance for AI agents working in this repository.

## Architecture

`aim` follows a layered architecture:

| Layer | Location | Responsibility |
|-------|----------|----------------|
| Presentation | `cmd/aim-server/` | CLI commands, server orchestration, user-facing entry point |
| Application (use case) | `internal/` | Orchestrates domain components into application flows (opinionated, not reusable across projects) |
| Domain | `pkg/` | Domain models, entities, value objects, and interfaces (shared, reusable, framework-agnostic) |

Domain models live in `pkg/`. Application layer (use cases) lives in `internal/`. Infrastructure concerns (persistence, external integrations) also live in `internal/`. Presentation layer is `cmd/aim-server/`.

## Platform support

Supported targets: **Linux**, **macOS**, and **Windows** — all native.

- Pure Go, no `cgo`, no OS-specific syscalls. Cross-compile freely: `GOOS=windows GOARCH=amd64 go build ./...`
- After changing code, cross-compile for all three OSes before considering the change done.

## Conventions

- Standard `gofmt` formatting; keep code idiomatic and match the style of the surrounding file.
- **Never abbreviate function parameters.** Write each parameter with its own type: `func fn(a string, b string) string`, not `func fn(a, b string) string`.
- **Always run `gofmt -w .` on changed files.** After every file update, run `gofmt -w .` to enforce consistent formatting on all .go files before running tests.
- **No comments. Code must be self-describing.** Do not write `//` or `/* */` comments anywhere — no doc comments, inline notes, section dividers, `TODO`s, or commented-out code. Instead make the code explain itself: descriptive names, named constants for magic values, and well-named helpers in place of a comment describing a step. If something still needs a comment to be understood, rename or restructure rather than add the comment. Exception: compiler directives (`//go:build`, `//go:generate`, `//go:embed`, `//nolint`) are not comments — keep them.
- Keep edits minimal and focused; prefer reusing existing helpers over adding new dependencies.
- **Always write tests for what you implement.** When adding a feature or fixing a bug, add or update the matching `*_test.go` table-driven test in the same package, covering the new behaviour and its edge cases (a bug fix should include a test that fails before the fix), then run `make test`.
- **One `_test.go` per source `*.go`.** A test file mirrors exactly one source file: tests for code in `foo.go` live in `foo_test.go`. Never introduce a `_test.go` with no matching `*.go` (e.g. no `log_test.go` without `log.go`) — put the tests in the test file named after the source that holds the code under test.
- Always run `make test` before considering a change done.

## Special Directories

- None currently

## Documentation

- README.md - start here for an overview of the project, features, and setup instructions
