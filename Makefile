# OAN Network Adapter — build, test, lint and security-scan targets.
#
# Single source of truth for the ci.yml and ci-release.yml workflows: every CI
# step is a one-line `make <target>` call, so a red check reproduces locally by
# running the command the log shows. Anything left inline in a workflow is
# GitHub context (`${{ }}` expressions, $GITHUB_STEP_SUMMARY writes) that has
# no meaning outside a runner.
#
# No DB, no sqlc/migrate, no separate tools/ module — golangci-lint, gotestsum
# and trivy install straight into bin/ via `go install` / curl.

GO      ?= go
BIN_DIR := bin
IMAGE   ?= network-adapter:dev

# Where a tag push publishes to. Derived from GITHUB_REPOSITORY rather than
# written out, so a fork publishes to its own namespace and there is no repo
# name to update if this one is ever renamed; `tr` because ghcr.io rejects an
# uppercase path and GITHUB_REPOSITORY is mixed-case (OpenAgriNet/...).
REGISTRY   ?= ghcr.io
IMAGE_REPO ?= $(REGISTRY)/$(shell printf '%s' '$(GITHUB_REPOSITORY)' | tr '[:upper:]' '[:lower:]')

# The arch of the machine running make, so neither a local run nor the build
# matrix has to pass it: each arch is built on a runner of that arch, never
# under QEMU, because Dockerfile.adapter-with-plugins compiles every plugin
# with `go build -buildmode=plugin` and a plugin .so must match the adapter
# binary's GOARCH exactly.
ARCH ?= $(shell uname -m | sed -e 's/^x86_64$$/amd64/' -e 's/^aarch64$$/arm64/')

# CI thresholds/pins live here, not duplicated into workflow env blocks — one
# source of truth for both a local `make` run and the GitHub Actions runner.
MIN_COVERAGE          ?= 80
# development, not main: every branch in this repo is cut from development and
# PRs target it, so that is the base a local `make cover-diff` must compare to.
BASE_REF              ?= origin/development
SEVERITY              ?= CRITICAL,HIGH,MEDIUM,LOW
GOLANGCI_LINT_VERSION := v2.5.0
GOTESTSUM_VERSION     := v1.13.0
TRIVY_VERSION         := v0.74.0
ACTIONLINT_VERSION    := v1.7.12

GOLANGCI_LINT := $(BIN_DIR)/golangci-lint
GOTESTSUM     := $(BIN_DIR)/gotestsum
TRIVY         := $(BIN_DIR)/trivy
ACTIONLINT    := $(BIN_DIR)/actionlint

# From GOROOT, not PATH: `go` is always resolvable here (every other target
# needs it), and gofmt sits next to it, so this works even where only the
# toolchain's bin dir is on PATH. Expanded at recipe time, hence the `$$`.
GOFMT = $$($(GO) env GOROOT)/bin/gofmt

# pkg/plugin and benchmarks/e2e each build a real .so with a plain `go build
# -buildmode=plugin` subprocess, then load it with plugin.Open in the same test
# run. Two things make that .so unloadable:
#
#   -race     — the subprocess build carries no -race flag of its own, so a
#               race-instrumented test binary and a non-race .so mismatch.
#   ./...     — instrumenting the whole module's coverage in one build gives
#               shared packages (e.g. pkg/plugin/definition) a build identity
#               the subprocess build doesn't share, so plugin.Open rejects the
#               .so as "built with a different version of" that package.
#
# So they run as their own invocation, without -race and with their own
# coverage profile. Named once here and shared by test, cover and test-ci —
# the three must never disagree about which packages are carved out.
#
# Deliberately no -coverpkg anywhere, and it can't be added: benchmarks/e2e is
# the only test-only package in the module (`go list` confirms it holds no
# non-test files), so it is the only place -coverpkg would credit coverage of
# the packages it drives — and it is in this carve-out precisely because that
# whole-module instrumentation is what makes plugin.Open reject the .so. The
# code benchmarks/e2e exercises is therefore credited only by its own
# packages' tests, which is why cover-diff gates on the diff rather than on a
# module-wide total.
PLUGIN_PKGS := ./pkg/plugin ./benchmarks/e2e/...
MAIN_PKGS    = $$($(GO) list ./... | grep -vE '/pkg/plugin$$|/benchmarks/e2e$$')

# Only used inside a workflow; a local run gets a placeholder rather than a
# broken link.
RUN_URL ?= $(if $(GITHUB_RUN_ID),$(GITHUB_SERVER_URL)/$(GITHUB_REPOSITORY)/actions/runs/$(GITHUB_RUN_ID),local run)

.DEFAULT_GOAL := help

## help: list the available targets
help:
	@grep -hE '^## [a-z]' $(MAKEFILE_LIST) | sed 's/^## /  /' | sort

## build: compile the adapter binary
# Scoped to cmd/adapter, not ./... — pkg/plugin/implementation/*/cmd holds
# `package main` sources meant only for `go build -buildmode=plugin`
# (install/build-plugins.sh), with no func main() for an ordinary build.
build:
	$(GO) build -trimpath -o $(BIN_DIR)/ ./cmd/adapter/...

## test: run the unit and integration suites (plugin packages without -race)
test:
	$(GO) test -race $(MAIN_PKGS)
	$(GO) test $(PLUGIN_PKGS)

## cover: run the suites and write a merged coverage profile to coverage.out
cover:
	$(GO) test -race -covermode=atomic -coverprofile=coverage.out $(MAIN_PKGS)
	$(GO) test -covermode=atomic -coverprofile=coverage-plugin.out $(PLUGIN_PKGS)
	@$(MAKE) --no-print-directory merge-coverage

## test-ci: cover, through gotestsum — one line per package. What ci.yml calls.
test-ci: $(GOTESTSUM)
	$(GOTESTSUM) --format pkgname --format-hide-empty-pkg -- \
		-race -coverprofile=coverage.out -covermode=atomic $(MAIN_PKGS)
	$(GOTESTSUM) --format pkgname --format-hide-empty-pkg -- \
		-coverprofile=coverage-plugin.out -covermode=atomic $(PLUGIN_PKGS)
	@$(MAKE) --no-print-directory merge-coverage

# `;` not `&&`, and always removes the intermediate: a failed tail must not
# leave coverage-plugin.out behind for someone to pick up by hand and misread.
merge-coverage:
	@tail -n +2 coverage-plugin.out >> coverage.out; rm -f coverage-plugin.out

# cover-diff needs a profile but must not re-run the suites in CI, where
# test-ci already wrote one. A file rule gives it both: present (CI) and make
# skips this; absent (clean local checkout) and it runs the suites once.
coverage.out:
	@$(MAKE) --no-print-directory cover

# The marker is written into the report itself, not added by the workflow:
# find-comment matches on this exact string to update its comment in place
# rather than posting a new one on every run.
COVER_MARKER := <!-- coverage-report -->
SEC_MARKER   := <!-- sec-scan -->

# Named once and shared by trivy-report and trivy-gate, so the report the PR
# shows and the report the gate reads can never be a different set of files.
SARIF_REPORTS := trivy-deps.sarif trivy-image.sarif

## cover-diff: coverage of the files changed vs BASE_REF, gated on MIN_COVERAGE
# A PR review needs the diff's number, not the whole repo's. On failure, names
# the changed files dragging it down, worst first. Always writes
# coverage-report.md — the workflow reads that file unconditionally, so every
# exit path here has to produce it.
cover-diff: coverage.out
	@if ! git rev-parse --verify --quiet "$(BASE_REF)^{commit}" >/dev/null; then \
		echo "::error::BASE_REF '$(BASE_REF)' does not resolve to a commit — cannot compute the changed-file set"; \
		echo "📊 **Test Coverage: ❌ Failed** — BASE_REF \`$(BASE_REF)\` does not resolve to a commit" > coverage-report.md; \
		exit 1; \
	fi; \
	if ! DIFF=$$(git diff --name-only --diff-filter=ACMR "$(BASE_REF)...HEAD" -- '*.go'); then \
		echo "::error::git diff against '$(BASE_REF)' failed — the changed-file set is unknown, not empty"; \
		echo "📊 **Test Coverage: ❌ Failed** — \`git diff\` against \`$(BASE_REF)\` failed" > coverage-report.md; \
		exit 1; \
	fi; \
	CHANGED=$$(printf '%s\n' "$$DIFF" | grep -v '_test\.go$$'); \
	if [ -z "$$CHANGED" ]; then \
		printf '%s\n' "$(COVER_MARKER)" "📊 **Test Coverage: ✅ Passed** — not applicable, no changed Go files vs $(BASE_REF)" | tee coverage-report.md; \
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
		printf '%s\n' "$(COVER_MARKER)" "📊 **Test Coverage: ✅ Passed** — not applicable, changed files carry no coverable statements" | tee coverage-report.md; \
		exit 0; \
	fi; \
	PCT=$$(echo "$$RESULT" | awk -F'\t' '$$1=="TOTAL"{print $$2}'); \
	{ \
		echo "$(COVER_MARKER)"; \
		if [ "$$PCT" -lt "$(MIN_COVERAGE)" ]; then \
			BELOW=$$(echo "$$RESULT" | awk -F'\t' '$$1=="FILE"{printf "%s\t%s\n",$$2,$$3}' | sort -n); \
			TOTAL_BELOW=$$(echo "$$BELOW" | wc -l); \
			echo "📊 **Test Coverage: ❌ Failed** — $${PCT}% of changed lines covered, min $(MIN_COVERAGE)%"; \
			echo; \
			echo "| File | Coverage |"; \
			echo "|---|---|"; \
			echo "$$BELOW" | head -15 | awk -F'\t' '{printf "| `%s` | %s%% |\n", $$2, $$1}'; \
			[ "$$TOTAL_BELOW" -gt 15 ] && echo "| … | $$((TOTAL_BELOW - 15)) more file(s) below $(MIN_COVERAGE)% |"; \
		else \
			echo "📊 **Test Coverage: ✅ Passed** — $${PCT}% of changed lines covered, min $(MIN_COVERAGE)%"; \
		fi; \
	} > coverage-report.md; \
	cat coverage-report.md; \
	if [ "$$PCT" -lt "$(MIN_COVERAGE)" ]; then \
		echo "::error::changed-file coverage is $${PCT}%, below the $(MIN_COVERAGE)% minimum"; \
		exit 1; \
	fi

## trivy-deps: scan the dependency graph, SARIF report to trivy-deps.sarif
# Catches what the image scan structurally cannot — a vulnerable module only
# the test suite imports, so it is never linked into the binary or a layer.
# --skip-dirs bin: the workflow restores the cached trivy binary into bin/
# before this runs, and a scanner reporting on its own binary is noise.
trivy-deps: $(TRIVY)
	$(TRIVY) fs . --skip-dirs $(BIN_DIR) --severity $(SEVERITY) --exit-code 0 \
		--format sarif --output trivy-deps.sarif

## trivy-image: scan IMAGE, SARIF report to trivy-image.sarif
# Reads the base layers plus the Go build info embedded in the binary —
# including `stdlib`, so a Go toolchain CVE shows up here and nowhere else.
trivy-image: $(TRIVY)
	$(TRIVY) image $(IMAGE) --severity $(SEVERITY) --exit-code 0 \
		--format sarif --output trivy-image.sarif

## trivy-release-gate: fail the release if the digest just pushed has a finding
# The PR-time Security Scan is not this gate. It scans an image built from the
# PR's tree on the day the PR ran; a tag cut weeks later rebuilds from a freshly
# pulled `wolfi-base` and a freshly resolved module graph, so the artifact that
# ships is not the artifact anything looked at. Without this, image-build
# publishes a digest no scan has ever seen.
#
# Scans the digest, not a local tag: image-build pushes by digest, so the only
# reference to the layers it just built is the one in digest-$(ARCH).txt. That
# digest is unreachable by name until image-publish binds a tag to it, and this
# runs first — so a finding here means the version tag is never created.
#
# --exit-code 1 and a table, not SARIF at --exit-code 0: there is no PR to
# comment on, so the findings belong in the log the red check points at, and
# the scan itself is the gate rather than a report something else grades.
trivy-release-gate: $(TRIVY) require-image-repo
	@test -s digest-$(ARCH).txt || \
		{ echo "::error::digest-$(ARCH).txt is missing or empty — run image-build first"; exit 1; }
	$(TRIVY) image $(IMAGE_REPO)@$$(cat digest-$(ARCH).txt) \
		--severity $(SEVERITY) --exit-code 1 --format table

## trivy-report: render both SARIF reports as one PR comment, trivy-report.md
# One comment covering both scans, not one comment each: the two scans run in
# the same job now, and two bot comments per PR was the noise this is meant to
# cut. A missing report is written into the comment as missing rather than
# skipped — trivy-gate fails on it, and the comment has to agree with the gate.
#
# The jq program lives in tools/trivy-comment.jq rather than inline: as a file
# it is lintable (`jq -n -f`), diffable, and free of Makefile `$$`/backslash
# escaping.
trivy-report:
	@{ \
		echo "$(SEC_MARKER)"; \
		echo "## 🛡️ Trivy security scan ($(SEVERITY))"; \
		echo "[View full run]($(RUN_URL))"; \
		for report in $(SARIF_REPORTS); do \
			case "$$report" in \
				trivy-deps.sarif)  title="Go dependencies";; \
				trivy-image.sarif) title="Container image";; \
				*)                 title="$$report";; \
			esac; \
			echo; echo "### $$title"; echo; \
			if [ -s "$$report" ]; then \
				jq -r --arg severity "$(SEVERITY)" -f tools/trivy-comment.jq "$$report"; \
			else \
				echo "⚠️ No report — the scan did not produce $$report."; \
			fi; \
		done; \
	} > trivy-report.md

## trivy-gate: fail if either SARIF report carries a finding, or is missing
# Reads the reports the two scans already produced rather than scanning a third
# and fourth time. A missing or unparsable report is a failure, not a pass: a
# scan that silently wrote nothing must not turn the gate into a green no-op.
trivy-gate:
	@fail=0; \
	for report in $(SARIF_REPORTS); do \
		if [ ! -s "$$report" ]; then \
			echo "$$report: MISSING — no scan produced it"; \
			fail=1; continue; \
		fi; \
		count=$$(jq '[.runs[].results[]?] | length' "$$report" 2>/dev/null); \
		if [ -z "$$count" ]; then \
			echo "$$report: UNREADABLE — not valid SARIF"; \
			fail=1; continue; \
		fi; \
		echo "$$report: $$count $(SEVERITY)"; \
		if [ "$$count" -gt 0 ]; then \
			jq -r '.runs[].results[]? | "\(.ruleId) \(.message.text)"' "$$report"; \
			fail=1; \
		fi; \
	done; \
	[ "$$fail" -eq 0 ] || echo "::error::Trivy findings at $(SEVERITY), or a missing report — see the log above"; \
	exit $$fail

## lint: vet, format check and static analysis
lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run ./...
	$(GOLANGCI_LINT) fmt --diff ./...

## fmt: apply the formatters that lint checks for
fmt: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) fmt ./...

## lint-actions: validate the workflows and composite actions
# Not wired into ci.yml on purpose — the pre-commit hook is the gate. Run
# whole-repo rather than per-file: actionlint resolves `needs:` across a
# workflow's jobs and checks `uses: ./.github/actions/...` against the action
# on disk, so a single file in isolation is not enough to judge either.
lint-actions: $(ACTIONLINT)
	$(ACTIONLINT)

## lint-staged: the pre-commit lints, against the staged files only. What the hook runs.
# Staged-only, and only these two checks, because both can pass today:
#
#   workflows   actionlint reports nothing on the current tree, so it blocks
#               from day one.
#   formatting  22 files in the repo are not gofmt-clean. Scoped to what you
#               staged, that history is someone else's problem until you touch
#               one of those files — at which point `make fmt` fixes it.
#
# Deliberately NOT `golangci-lint run`: with no .golangci.yml it uses tool
# defaults against a codebase that has never been linted, so it would reject
# every commit. Nor the test suite — a pre-commit hook has to stay in seconds,
# and CI is where `make test-ci` belongs.
#
# Reads the working tree, not the staged blob. A file staged clean but dirty in
# the working copy is reported here; that is the conservative direction, and
# avoids checking out the index to a temp dir on every commit.
lint-staged:
	@STAGED=$$(git diff --cached --name-only --diff-filter=ACMR); \
	if [ -z "$$STAGED" ]; then \
		echo "lint-staged: nothing staged"; \
		exit 0; \
	fi; \
	fail=0; \
	if printf '%s\n' "$$STAGED" | grep -qE '^\.github/(workflows/.*\.ya?ml|actions/.*/action\.ya?ml)$$'; then \
		echo "==> lint-actions (staged workflow or action change)"; \
		$(MAKE) --no-print-directory lint-actions || fail=1; \
	fi; \
	GOFILES=$$(printf '%s\n' "$$STAGED" | grep '\.go$$' || true); \
	if [ -n "$$GOFILES" ]; then \
		echo "==> gofmt (staged Go files)"; \
		UNFMT=$$(printf '%s\n' "$$GOFILES" | xargs $(GOFMT) -l); \
		if [ -n "$$UNFMT" ]; then \
			echo "not gofmt-clean:"; \
			printf '  %s\n' $$UNFMT; \
			echo "run \`make fmt\` (or gofmt -w on the files above), then stage the result"; \
			fail=1; \
		fi; \
	fi; \
	if [ "$$fail" -ne 0 ]; then \
		echo; \
		echo "pre-commit checks failed — commit aborted"; \
		exit 1; \
	fi; \
	echo "lint-staged: ok"

## hooks: point git at the repo's versioned hooks (run once per clone)
# core.hooksPath rather than copying into .git/hooks: the hook stays in the
# repo, under review, and a change to it reaches everyone on their next pull
# instead of only the people who re-copy it.
hooks:
	git config core.hooksPath .githooks
	@echo "core.hooksPath -> .githooks, running: $$(ls .githooks | tr '\n' ' ')"

## docker: build the shipped adapter image — the Dockerfile and build args CI scans
# Dockerfile.adapter-with-plugins, not Dockerfile.adapter: the plugins image is
# what the Security Scan job scans and what a tag publishes, so a local
# `make docker && make trivy-image` scans the same thing CI gates on. The vars
# come from the script rather than being named again here, so ONIX_VERSION and
# friends are spelled out in exactly one place.
docker:
	. install/scripts/version-vars.sh && \
	docker build -f Dockerfile.adapter-with-plugins \
		--build-arg ONIX_VERSION="$$ONIX_VERSION" \
		--build-arg GIT_COMMIT="$$GIT_COMMIT" \
		--build-arg GIT_TREE_STATE="$$GIT_TREE_STATE" \
		--build-arg BUILD_DATE="$$BUILD_DATE" \
		-t $(IMAGE) .

## image-build: push this arch's image to IMAGE_REPO by digest (ARCH, no tag)
# Pushed untagged, by digest only. Nothing binds a version tag to it until
# image-publish has both arches, so a release that builds amd64 and then fails
# on arm64 never leaves behind a tag `docker pull` resolves on one platform and
# 404s on the other.
#
# --provenance=false: with provenance on, buildx wraps even a single-platform
# push in an OCI index to carry the attestation, and `imagetools create` would
# then compose indexes of indexes. A plain manifest per arch is what makes the
# two-platform index image-publish builds a clean one.
#
# Same Dockerfile and same version build args as `docker` above, so what a tag
# publishes is what CI scanned on the PR.
image-build: require-image-repo
	. install/scripts/version-vars.sh && \
	docker buildx build -f Dockerfile.adapter-with-plugins \
		--build-arg ONIX_VERSION="$$ONIX_VERSION" \
		--build-arg GIT_COMMIT="$$GIT_COMMIT" \
		--build-arg GIT_TREE_STATE="$$GIT_TREE_STATE" \
		--build-arg BUILD_DATE="$$BUILD_DATE" \
		--platform linux/$(ARCH) \
		--provenance=false \
		--output type=image,name=$(IMAGE_REPO),push-by-digest=true,name-canonical=true,push=true \
		--metadata-file image-metadata-$(ARCH).json .
	@jq -r '."containerimage.digest"' image-metadata-$(ARCH).json > digest-$(ARCH).txt
	@echo "pushed $(IMAGE_REPO)@$$(cat digest-$(ARCH).txt) (linux/$(ARCH))"

## image-publish: tag the digests image-build pushed as one multi-arch release
# The only step that creates a user-visible tag. Reads whatever digest-*.txt
# files are present rather than a fixed arch list, so adding an arch to the
# build matrix needs no change here.
#
# `&&` between every step, not `;`: a recipe is one shell invocation with no
# `set -e`, so with `;` the exit status would be `imagetools inspect`'s alone.
# A failed `create` on a tag that already exists would then leave inspect
# reporting the *previous* index and this job green — the published tag would
# point at the wrong digests with nothing red to say so.
#
# The tag comes from version-vars.sh, the same place the binary's -ldflags
# version comes from, so the image tag and `adapter --version` can't disagree.
# `latest` moves only for a plain vX.Y.Z: git describe renders a pre-release as
# v2.0.1-rc1 and an untagged commit as v1.8.2-3-gabc1234, and neither should be
# what `docker pull` gives someone who asked for no tag at all.
image-publish: require-image-repo
	@ls digest-*.txt >/dev/null 2>&1 || \
		{ echo "::error::no digest-*.txt — run image-build on each arch first"; exit 1; }
	. install/scripts/version-vars.sh && \
	tags="-t $(IMAGE_REPO):$$ONIX_VERSION" && \
	case "$$ONIX_VERSION" in \
		*-*) echo "$$ONIX_VERSION is not a plain release — not moving :latest";; \
		*)   tags="$$tags -t $(IMAGE_REPO):latest";; \
	esac && \
	docker buildx imagetools create $$tags \
		$$(for d in digest-*.txt; do echo "$(IMAGE_REPO)@$$(cat $$d)"; done) && \
	docker buildx imagetools inspect $(IMAGE_REPO):$$ONIX_VERSION

# Split out so both image targets fail the same way, naming the thing to set,
# instead of pushing to a repo path that is just the registry and a slash.
require-image-repo:
	@test "$(IMAGE_REPO)" != "$(REGISTRY)/" || \
		{ echo "::error::IMAGE_REPO is empty — set GITHUB_REPOSITORY=owner/repo, or IMAGE_REPO directly"; exit 1; }

## clean: remove build output and coverage/scan artifacts
clean:
	rm -rf $(BIN_DIR) coverage.out coverage-plugin.out coverage-report.md \
		$(SARIF_REPORTS) trivy-report.md \
		image-metadata-*.json digest-*.txt

$(GOLANGCI_LINT):
	@mkdir -p $(BIN_DIR)
	GOBIN=$(abspath $(BIN_DIR)) $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

# gotestsum is CI-only (see ci.yml), so it doesn't belong in the adapter's or
# the linter's dependency graph either one.
$(GOTESTSUM):
	@mkdir -p $(BIN_DIR)
	GOBIN=$(abspath $(BIN_DIR)) $(GO) install gotest.tools/gotestsum@$(GOTESTSUM_VERSION)

# Pinned like the others, and go-installable, so no curl | sh for this one.
$(ACTIONLINT):
	@mkdir -p $(BIN_DIR)
	GOBIN=$(abspath $(BIN_DIR)) $(GO) install github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)

# The prebuilt release binary, not `go install`: trivy's rpm-db parser needs
# cgo, and its module graph is comparable in size to golangci-lint's for a
# tool nothing here imports — the official install script is what
# aquasecurity itself recommends over building from source for exactly this.
#
# The script is fetched at $(TRIVY_VERSION), not at main: this pipes a remote
# script into sh in a job that holds the runner's GITHUB_TOKEN, so what runs
# has to be the reviewed script for the pinned release rather than whatever is
# on the default branch at the time. Every other tool here is pinned too.
$(TRIVY):
	@mkdir -p $(BIN_DIR)
	curl -sfL https://raw.githubusercontent.com/aquasecurity/trivy/$(TRIVY_VERSION)/contrib/install.sh | \
		sh -s -- -b $(abspath $(BIN_DIR)) $(TRIVY_VERSION)

.PHONY: help build test cover test-ci merge-coverage cover-diff lint fmt \
	lint-actions lint-staged hooks \
	trivy-deps trivy-image trivy-report trivy-gate trivy-release-gate \
	docker image-build image-publish require-image-repo clean
