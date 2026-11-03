# Checks run identically locally and in CI (.github/workflows/validate.yml).
# Every module is checked against the core in this checkout.

MODULES := . tavernprom tavernotel

# Pinned tools. Refresh them in a reviewed PR (see CONTRIBUTING.md).
GOLANGCI_LINT_VERSION := v2.15.0
ACTIONLINT_VERSION := v1.7.12

# Use the installed Go, which must satisfy the go directive, instead of
# switching toolchains behind the build.
export GOTOOLCHAIN := local

GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
ACTIONLINT := go run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)

.PHONY: ci check check-module tidy-check vet test lint lint-workflows test-scripts

# ci checks every module, the workflows and the release script.
ci: check lint-workflows test-scripts

check:
	@for m in $(MODULES); do $(MAKE) --no-print-directory check-module MODULE=$$m || exit 1; done

# check-module runs all checks for one module: make check-module MODULE=tavernprom
check-module: tidy-check vet test lint

MODULE ?= .

tidy-check:
	cd $(MODULE) && go mod tidy -diff

vet:
	cd $(MODULE) && go vet ./...

test:
	cd $(MODULE) && go test -race -count=1 ./...

lint:
	cd $(MODULE) && $(GOLANGCI_LINT) run --timeout=5m ./...

# actionlint also checks workflow shell steps; both need shellcheck on PATH.
lint-workflows:
	shellcheck .github/scripts/*.sh
	$(ACTIONLINT)

test-scripts:
	.github/scripts/release_test.sh
