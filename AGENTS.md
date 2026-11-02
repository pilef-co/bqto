# How to work on bqto

This file is for coding agents and contributors who change bqto. The repository
says how bqto works now: code, `README.md` and `docs/`. [CONTEXT.md](CONTEXT.md)
defines the terms.

## Branch, commit, pull request

- Conventional Commits for every commit and for the PR title: `feat`, `fix`, `docs`,
  `ci`, `chore`, `refactor`, `test`, with an optional scope such as `fix(valkey):`. The
  PR is squashed into one commit on `main` from its title and body, so a change that
  breaks something takes `!` in the PR title (`feat!:`) and a final `BREAKING CHANGE:`
  paragraph in the PR body.
- CI checks that the PR title is a Conventional Commit, because the title becomes
  the subject of the squash commit on `main`.
- The PR body follows [.github/pull_request_template.md](.github/pull_request_template.md)
  and says which docs changed, or `Docs: no change needed`.
- Keep the PR to one change. Propose a new finding separately instead of widening
  the PR.

## Code

This repository builds one binary: `cmd/bqto` calls `app.Main`, which runs
`internal/cli`. bqto Pro is a separate product that builds on the `ext` package.

- The core (`internal/cli`, `internal/bastion`, `internal/tunnel`, `internal/auth`) and the
  free services (`cloudsql`, `alloydb`, `redis`, `valkey`) know nothing of the paid ones.
  A service registers itself with `ext.RegisterService`; paid flags, commands, wizard
  questions and targets plug in with `ext.RegisterConnect` and `ext.RegisterCommand`.
- `bqto -h` aligns the `Usage` lines of a registered command, written `COMMAND  description`
  with no double space inside COMMAND, in one column. A `bqto` line replaces the wizard's,
  a `bqto connect ...` line joins the connect group, and the rest join the group of profiles
  and stacks.
- `ext` is the extension point bqto Pro builds on. A change to it can break bqto Pro.
- `ext/pro.go` lists the bqto Pro services, variants, commands and flags. This binary
  refuses them with a `Fix:` line and exit code 2, and lists the flags last in its help,
  marked `(bqto Pro)`. Paid code never goes in this repository.
- `bqto version` and `bqto update` live in `internal/cli/version.go` and
  `internal/selfupdate`: the free binary asks GitHub Releases, and bqto Pro
  plugs in its own source and download with `ext.SetUpdater`. The core always
  checks `checksums.txt` and replaces the binary itself. Nothing looks for
  updates unless one of these two commands runs.
- A Google API read goes through `gcperr.Retry` (or `gcperr.RetryDo`), which retries a brief
  outage. A write (create or delete a VM) never does: a retried insert could act twice.
- A `Fix:` line in the core offers a bqto Pro flag only when `ext.FlagRegistered` says
  the binary has it, so the free binary never asks for a flag it refuses. Hooks only
  define flags on the `fs` and `*Config` they get: `ext.FlagRegistered` runs them on a
  scratch flag set.
- The Services table and the bqto Pro section of `README.md` say which plan has
  each feature. `ext/promark_test.go` fails when the README or the agent skill
  disagree with `ext/pro.go`.

- No comments in Go code. Only toolchain directives: `//go:build`, `//go:generate`,
  `//nolint:<linter>`. Say it in the name, the structure or the test. Prose goes in the PR body,
  or in the docs when users need it.
- Every error the user sees names its cause and a `Fix:` line.
- Code that calls a Google API is tested against `internal/gcpfake`, as
  [CONTRIBUTING.md](CONTRIBUTING.md) explains.

## Keep the docs in sync

Before the commit, update in the same PR whatever the change affects:

- `README.md`: supported services and their plan, requirements, build and install,
  examples.
- [docs/usage.md](docs/usage.md): flags, environment variables, exit codes, cleanup
  and updates.
- [docs/troubleshooting.md](docs/troubleshooting.md): a tunnel error that bqto
  recognizes.
- [docs/design.md](docs/design.md): why bqto needs a VM, its scope and its
  measurements.
- [docs/permissions.md](docs/permissions.md): every new API call, with its permission and
  role.
- `scripts/testenv.sh`: a target for a new service or kind of instance.
- [skills/bqto/SKILL.md](skills/bqto/SKILL.md): when a flag, an environment variable or
  the JSON output that it describes changes.

## Tests

Every PR runs CI ([.github/workflows/ci.yml](.github/workflows/ci.yml)): `go vet` and
`go test -race -shuffle=on ./...`, `go test ./...` on the minimum Go of `go.mod`, a
build for every release platform, `go mod tidy -diff`, gofmt, golangci-lint,
shellcheck, actionlint, zizmor, gitleaks and govulncheck, all at the versions in
`mise.toml`. `go test -race -shuffle=on ./...` on macOS
([.github/workflows/test-macos.yml](.github/workflows/test-macos.yml)) runs when the
PR gets the `test:macos` label, by hand and after each release. Add the label when
a change can behave differently on macOS: ports, signals, file paths, the shell.
Before opening the PR, run the checks of [CONTRIBUTING.md](CONTRIBUTING.md), which
also has the setup. A lint finding is fixed, or silenced with
`//nolint:<linter> // reason`.

Unit tests never reach Google Cloud. Say in one line of the PR body what the unit
tests do not cover.

To check a change by hand against real services, `scripts/testenv.sh` creates a
target and connects to it with `go run ./cmd/bqto`. That is the free binary, so
the targets of bqto Pro services and variants need bqto Pro.
