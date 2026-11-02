# Contributing

To work on bqto, install the pre-commit hook once:

```sh
pre-commit install
```

The hook runs on `git commit`, not on push. It runs gitleaks, which scans the
change for secrets.

CI runs the same secret scan over the whole history, so `SKIP=gitleaks` only
postpones the failure. CI also checks that the pull request title is a
Conventional Commit. The squash merge makes the title the commit subject, and
release-please builds the version and the changelog from it.

`mise.toml` pins every tool CI uses. With [mise](https://mise.jdx.dev)
installed, `mise install` puts them on `PATH` for this checkout. To update a
tool, change `mise.toml` only. The checks CI runs:

```sh
test -z "$(gofmt -l . | tee /dev/stderr)"
go vet ./...
go test -race -shuffle=on ./...
golangci-lint run ./...
shellcheck scripts/*.sh install.sh
actionlint
zizmor .github/workflows
govulncheck ./...
```

`.golangci.yml` lists the Go linters and the findings left out on purpose, with
the reason. A finding in the code is fixed, or silenced on its line with
`//nolint:<linter> // reason`. The pre-commit hook pins its own gitleaks
version (`.pre-commit-config.yaml`); keep it equal to `mise.toml`.

Code that calls a Google API is tested against a fake server, not a live project.
Each package that builds a client passes a `clientOptions` variable to it; a test
points it at `internal/gcpfake`, queues the answer to each request and fails on
any request it was not told about and on any answer never asked for. The variable
is shared by the whole package, so these tests cannot use `t.Parallel`; the fake
stops a second one that tries.

To check a change by hand against real services, `scripts/testenv.sh create TARGET` creates
a test instance, and `scripts/testenv.sh connect TARGET` connects to it with
`go run ./cmd/bqto`, the free binary. The targets of bqto Pro services and
variants need bqto Pro.

## Docs

When a pull request adds or changes a feature, it updates the docs in the same
pull request. [AGENTS.md](AGENTS.md#keep-the-docs-in-sync) lists them.

## Build

`go build -o bqto ./cmd/bqto` builds the free binary. It calls `app.Main`, which
runs `internal/cli`. A release build sets the version with
`-ldflags "-X github.com/pilef-co/bqto/internal/cli.version=v1.2.3"`. Without it,
`bqto version` prints the version the Go toolchain recorded (module or VCS), or
`dev`.

## Releases

release-please keeps a release pull request open with the next version and the
changelog. Merging it tags the version. The `binaries` job of the
release-please workflow then runs GoReleaser (`.goreleaser.yaml`). It builds
`cmd/bqto` for darwin and linux, amd64 and arm64, with `-trimpath`, the commit's
timestamps and root as the archive owner, so two builds of one tag give the same
`checksums.txt`. It builds from the tag, even when main has moved on. To check a
change to the release build locally, run `goreleaser release --snapshot --clean`.
