# Contributing

## Checks

`make ci` runs everything CI runs:

- for each module (`.`, `tavernprom`, `tavernotel`): `go mod tidy -diff`,
  `go vet`, `go test -race` and golangci-lint;
- actionlint and shellcheck on the workflows and scripts, the release
  script's dry-run tests and the proxy smoke script's failure-path tests.

`make check-module MODULE=tavernprom` checks one module. The examples under
`examples/` belong to the root module, so these checks cover them.
`make proxy-smoke` runs the task board behind nginx (Docker required; CI
runs it on every pull request), and `make capacity` runs the load presets. Lint and actionlint
run through `go run` at the versions pinned in the Makefile, so no separate
install is needed; shellcheck must be on `PATH`.

The Makefile sets `GOTOOLCHAIN=local`: the installed Go must satisfy the
`go` directive rather than a toolchain being downloaded behind the build.

## Go baseline

Every module declares `go 1.27.3`, the minimum the core requires. Raising it
is a deliberate change in a reviewed pull request; CI never rewrites it.

## Pinned tools

- Go: the version in the root `go.mod` (`actions/setup-go` reads it).
- golangci-lint and actionlint: `GOLANGCI_LINT_VERSION` and
  `ACTIONLINT_VERSION` in the Makefile.
- GitHub Actions: pinned to commit SHAs with the release tag in a comment.

Dependabot proposes Go module and GitHub Actions updates as weekly pull
requests (`.github/dependabot.yml`). They run the same checks as any change
and are merged by a maintainer; nothing merges automatically. Refresh the
Makefile tool pins by hand in a pull request.

## Releases

Each push to `main` runs the Pipeline workflow. It validates the pushed
commit and, only if every module passes, tags exactly that commit with the
next patch version and publishes a GitHub release
(`.github/scripts/release.sh`).

- Releases are serialized. A rerun for a commit that already has a release
  tag reuses that tag; a commit older than the latest release is not
  released.
- The weekly scheduled run only validates `main`; it never releases.
- A manual run on `main` validates and releases the same way; on any other
  branch it only validates.
- Minor and major versions are tagged by hand.

## Adapter modules

`tavernprom` and `tavernotel` are separate modules. In this repository they
build against the local core through `replace github.com/catgoose/tavern =>
../`, so CI tests them with your changes. For users that replace is
ignored and the `require` line applies: it names a published core release,
so raise it in a pull request when an adapter needs a newer core.

Release tags (`vX.Y.Z`) version the core module only. The adapters are not
tagged, so `go get github.com/catgoose/tavern/tavernprom@latest` resolves to
a pseudo-version of the latest commit.
