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

.PHONY: ci check check-module tidy-check vet test lint lint-workflows test-scripts verify-assets proxy-smoke browser-install browser-test capacity

# ci checks every module, the workflows, the scripts and the vendored assets.
# The proxy and browser tests need Docker and a browser; run them separately.
ci: check lint-workflows test-scripts verify-assets

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
	shellcheck .github/scripts/*.sh examples/taskboard/deploy/*.sh examples/taskboard/vendor.sh
	$(ACTIONLINT)

test-scripts:
	.github/scripts/release_test.sh
	examples/taskboard/deploy/smoke_test.sh

# verify-assets checks the task board's vendored browser files against their
# recorded digests and the pinned tavern-js release.
verify-assets:
	examples/taskboard/vendor.sh upstream

# proxy-smoke runs the task board behind a pinned nginx and checks streaming
# through it. Needs Docker and Linux host networking.
proxy-smoke:
	examples/taskboard/deploy/smoke.sh

# browser-install installs the locked test dependencies and the Chromium build
# they expect. CI adds PLAYWRIGHT_INSTALL_FLAGS=--with-deps for system packages.
BROWSER_DIR := examples/taskboard/browser
PLAYWRIGHT_INSTALL_FLAGS ?=

browser-install:
	cd $(BROWSER_DIR) && npm ci --no-audit --no-fund
	cd $(BROWSER_DIR) && npx playwright install $(PLAYWRIGHT_INSTALL_FLAGS) chromium

# browser-test runs the task board and showcase in a real browser.
browser-test:
	cd $(BROWSER_DIR) && npx playwright test

# capacity runs every load preset; see examples/capacity/README.md.
capacity:
	go run ./examples/capacity -preset all
