# OAN Network Adapter — build, test and toolchain targets.
#
# Mirrors discovery-service's Makefile shape (same target names, same
# run-tests/security/build-and-push workflows call these, not raw commands),
# adapted to this module: no DB, no sqlc/migrate, no separate tools/ module —
# golangci-lint, gotestsum and trivy install straight into bin/ via `go
# install`/curl, same as discovery-service does for gotestsum and trivy.

GO      ?= go
BIN_DIR := bin
IMAGE   ?= network-adapter:dev

# CI thresholds/pins live here, not duplicated into workflow env blocks — one
# source of truth for both a local `make` run and the GitHub Actions runner.
MIN_COVERAGE          ?= 80
BASE_REF              ?= origin/main
SEVERITY              ?= HIGH,CRITICAL
GOLANGCI_LINT_VERSION := v2.5.0
GOTESTSUM_VERSION     := v1.13.0
TRIVY_VERSION         := v0.74.0

GOLANGCI_LINT := $(BIN_DIR)/golangci-lint
GOTESTSUM     := $(BIN_DIR)/gotestsum
TRIVY         := $(BIN_DIR)/trivy

.DEFAULT_GOAL := help

## help: list the available targets
help:
	@grep -hE '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /' | sort

## build: compile the adapter binary
# Scoped to cmd/adapter, not ./... — pkg/plugin/implementation/*/cmd holds
# `package main` sources meant only for `go build -buildmode=plugin`
# (install/build-plugins.sh), with no func main() for an ordinary build.
build:
	$(GO) build -trimpath -o $(BIN_DIR)/ ./cmd/adapter/...

## test: run the unit and integration suites
test:
	$(GO) test -race ./...

## cover: run the suites and write a coverage profile
cover:
	$(GO) test -race -covermode=atomic -coverprofile=coverage.out ./...

## test-ci: run the suites through gotestsum — one line per package, coverage
##          profile written alongside. What run-tests.yml calls; `make test`
##          stays the plain everyday entrypoint.
#
# pkg/plugin and benchmarks/e2e each build a real .so with a plain `go build
# -buildmode=plugin` subprocess, then load it with plugin.Open in the same
# test run. Instrumenting the *whole* module's coverage in one `./...` build
# gives shared packages (e.g. pkg/plugin/definition) a build identity that
# subprocess build doesn't share, so plugin.Open rejects the .so as built
# with a different version of that package. Splitting them into their own
# `go test` invocation keeps their build graph small enough to match.
test-ci: $(GOTESTSUM)
	$(GOTESTSUM) --format pkgname --format-hide-empty-pkg -- \
		-race -coverprofile=coverage.out -covermode=atomic \
		$$(go list ./... | grep -vE '/pkg/plugin$$|/benchmarks/e2e$$')
	# No -race here: these two packages build a plugin .so in a subprocess
	# `go build` with no -race flag of its own, so a race-instrumented test
	# binary and a non-race .so mismatch and plugin.Open refuses to load it.
	$(GOTESTSUM) --format pkgname --format-hide-empty-pkg -- \
		-coverprofile=coverage-plugin.out -covermode=atomic \
		./pkg/plugin ./benchmarks/e2e/...
	@tail -n +2 coverage-plugin.out >> coverage.out && rm -f coverage-plugin.out

## cover-diff: coverage restricted to files changed vs BASE_REF — a PR review
##             needs the diff's number, not the whole repo's. On failure,
##             names the changed files dragging the number down (worst first).
cover-diff: coverage.out
	@CHANGED=$$(git diff --name-only --diff-filter=ACMR "$(BASE_REF)...HEAD" -- '*.go' | grep -v '_test\.go$$' || true); \
	if [ -z "$$CHANGED" ]; then \
		echo "📊 **Test Coverage: ✅ Passed** — not applicable, no changed Go files vs $(BASE_REF)" | tee coverage-report.md; \
		exit 0; \
	fi; \
	MODULE=$$($(GO) list -m); \
	RESULT=$$(echo "$$CHANGED" | awk -v mod="$$MODULE/" -v min="$(MIN_COVERAGE)" ' \
		NR==FNR { want[mod $$0] = 1; next } \
		{ f = $$1; sub(/:.*/, "", f); if (!(f in want)) next; \
		  tot[f] += $$(NF-1); if ($$NF > 0) cov[f] += $$(NF-1) } \
		END { \
			T = 0; C = 0; \
			for (f in tot) { \
				T += tot[f]; C += cov[f]; \
				p = int(cov[f] * 100 / tot[f]); \
				disp = f; sub("^" mod, "", disp); \
				if (p < min) print "FILE\t" p "\t" disp; \
			} \
			if (T == 0) { print "EMPTY"; exit } \
			print "TOTAL\t" int(C * 100 / T) \
		}' - coverage.out); \
	if echo "$$RESULT" | grep -q '^EMPTY$$'; then \
		echo "📊 **Test Coverage: ✅ Passed** — not applicable, changed files carry no coverable statements" | tee coverage-report.md; \
		exit 0; \
	fi; \
	PCT=$$(echo "$$RESULT" | awk -F'\t' '$$1=="TOTAL"{print $$2}'); \
	if [ "$$PCT" -lt "$(MIN_COVERAGE)" ]; then \
		BELOW=$$(echo "$$RESULT" | awk -F'\t' '$$1=="FILE"{printf "%s\t%s\n",$$2,$$3}' | sort -n); \
		TOTAL_BELOW=$$(echo "$$BELOW" | wc -l); \
		{ \
			echo "📊 **Test Coverage: ❌ Failed** — $${PCT}% of changed lines covered, min $(MIN_COVERAGE)%"; \
			echo; \
			echo "| File | Coverage |"; \
			echo "|---|---|"; \
			echo "$$BELOW" | head -15 | awk -F'\t' '{printf "| `%s` | %s%% |\n", $$2, $$1}'; \
			[ "$$TOTAL_BELOW" -gt 15 ] && echo "| … | $$((TOTAL_BELOW - 15)) more file(s) below $(MIN_COVERAGE)% |"; \
		} > coverage-report.md; \
	else \
		echo "📊 **Test Coverage: ✅ Passed** — $${PCT}% of changed lines covered, min $(MIN_COVERAGE)%" > coverage-report.md; \
	fi; \
	cat coverage-report.md; \
	[ "$$PCT" -ge "$(MIN_COVERAGE)" ]

## trivy-deps: dependency graph scan (T4), SARIF report. Catches what the
##             image scan structurally cannot — a vulnerable module only the
##             test suite imports, so it's never linked into the binary.
trivy-deps: $(TRIVY)
	$(TRIVY) fs . --severity $(SEVERITY) --exit-code 0 \
		--format sarif --output trivy-deps.sarif

TRIVY_IMAGE_SCAN = $(TRIVY) image $(IMAGE) --severity $(SEVERITY)

## trivy-image: shipped image scan (T4), SARIF report. IMAGE names the ref.
trivy-image: $(TRIVY)
	$(TRIVY_IMAGE_SCAN) --exit-code 0 --format sarif --output trivy-image.sarif

## trivy-release-gate: same image scan as trivy-image, but exit 1 on a
##                     finding instead of writing a report — the pre-push
##                     release gate build-and-push.yml runs once per
##                     arch-tagged local image, before anything is pushed.
trivy-release-gate: $(TRIVY)
	$(TRIVY_IMAGE_SCAN) --exit-code 1 --format table

## trivy-gate: fail if either SARIF report already produced by a scan step
##             carries a finding. Reads the reports rather than rescanning.
trivy-gate:
	@fail=0; \
	for report in trivy-deps.sarif trivy-image.sarif; do \
		count=$$(jq '[.runs[].results[]?] | length' "$$report"); \
		echo "$${report}: $${count} $(SEVERITY)"; \
		if [ "$$count" -gt 0 ]; then \
			jq -r '.runs[].results[]? | "\(.ruleId) \(.message.text)"' "$$report"; \
			fail=1; \
		fi; \
	done; \
	exit $$fail

## lint: vet, format check and static analysis
lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run ./...
	$(GOLANGCI_LINT) fmt --diff ./...

## fmt: apply the formatters lint checks for
fmt: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) fmt ./...

## docker: build the adapter image
docker:
	docker build -f Dockerfile.adapter -t $(IMAGE) .

## tools: build the pinned toolchain into bin/
tools: $(GOLANGCI_LINT) $(GOTESTSUM) $(TRIVY)

## clean: remove build output and coverage/scan artifacts
clean:
	rm -rf $(BIN_DIR) coverage.out coverage-report.md trivy-deps.sarif trivy-image.sarif

$(GOLANGCI_LINT):
	@mkdir -p $(BIN_DIR)
	GOBIN=$(abspath $(BIN_DIR)) $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

# gotestsum is CI-only (see run-tests.yml), so it doesn't belong in the
# adapter's or the linter's dependency graph either one.
$(GOTESTSUM):
	@mkdir -p $(BIN_DIR)
	GOBIN=$(abspath $(BIN_DIR)) $(GO) install gotest.tools/gotestsum@$(GOTESTSUM_VERSION)

# The prebuilt release binary, not `go install`: trivy's rpm-db parser needs
# cgo, and its module graph is comparable in size to golangci-lint's for a
# tool nothing here imports — the official install script is what
# aquasecurity itself recommends over building from source for exactly this.
$(TRIVY):
	@mkdir -p $(BIN_DIR)
	curl -sfL https://raw.githubusercontent.com/aquasecurity/trivy/main/contrib/install.sh | \
		sh -s -- -b $(abspath $(BIN_DIR)) $(TRIVY_VERSION)

.PHONY: help build test cover test-ci cover-diff lint fmt trivy-deps \
	trivy-image trivy-release-gate trivy-gate docker tools clean
