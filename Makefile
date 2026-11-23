.PHONY: build run install deploy undeploy \
	test lint fmt fmt-check fix vuln secrets pin-check deadcode mod-check \
	docs-check hooks check coverage coverage-check tools pins-outdated \
	compile ci-parity ensure-tools vet script-check

# ── The same environment as CI ───────────────────────────────────────────────
# Every gate runs what CI runs, with what CI runs it with:
#  - Go: exactly go.mod's `toolchain` line (downloaded once), not whatever go is on
#    PATH, which may be newer. GOTOOLCHAIN=local opts out, for offline work.
#  - No user `go env` file: CI has none, and one can carry GOEXPERIMENT or GOFLAGS
#    that change the build.
#  - golangci-lint, govulncheck and deadcode: the go.mod `tool` directive.
#  - gitleaks and markdownlint-cli2: tools/versions.env and
#    tools/markdownlint's lock, installed into .tools by scripts/ensure-tools.sh,
#    which CI runs too.
# scripts/ci-parity-check.sh fails when a `check` gate is not a CI step or back.
GO_TOOLCHAIN := $(shell awk '$$1 == "toolchain" {print $$2}' go.mod)
export GOTOOLCHAIN := $(GO_TOOLCHAIN)
export GOENV := off
export PATH := $(CURDIR)/.tools/bin:$(PATH)
ENSURE   := bash scripts/ensure-tools.sh
GOLANGCI := go tool golangci-lint
GOVULN   := go tool govulncheck

# CHECK_STRICT=1 (the pre-push hook) fails a gate that would otherwise be skipped
# here, which is only the history secret scan outside the project's own repository.
CHECK_STRICT ?=

# ── Build ────────────────────────────────────────────────────────────────────
# Built as deployed: CGO_ENABLED=0 (stdlib only). `test` keeps cgo: -race needs it. Outside its own repository go would stamp that repository's
# commit into the binaries, so vcs stamping is off there.
OWN_REPO := $(shell [ "$$(git rev-parse --show-toplevel 2>/dev/null)" = "$(CURDIR)" ] && echo yes)
BUILDVCS := $(if $(OWN_REPO),,-buildvcs=false)
GOBUILD := CGO_ENABLED=0 go build $(BUILDVCS)
GORUN   := CGO_ENABLED=0 go run $(BUILDVCS)

# Every package, as deployed (CI's Build step).
compile:
	$(GOBUILD) ./...

build:
	$(GOBUILD) -o hirdman .

# ── Run from source ──────────────────────────────────────────────────────────
run:
	$(GORUN) .

# One coverage profile per make invocation, so concurrent runs (agents, hooks, a
# developer) never write the same file. Pass COVERPROFILE=path to choose one.
# Named by make's PID: unique per invocation, and nothing is created until go test
# writes it.
ifndef COVERPROFILE
COVERPROFILE := $(or $(TMPDIR),/tmp)/hirdman-cover-$(shell echo $$PPID).out
endif

# -count=1: never serve cached results. The profile feeds coverage-check.
test:
	go test -race -count=1 -coverprofile=$(COVERPROFILE) ./...

# The pre-push gate's tests: no race detector, and from Go's test cache, so a package
# whose code and tests did not change is not run again. CI runs `test`.
test-quick:
	go test ./...

# --allow-serial-runners: a second concurrent run waits for the first instead of
# failing with "parallel golangci-lint is running".
lint:
	$(GOLANGCI) run --allow-serial-runners ./...

fmt:
	$(GOLANGCI) fmt ./...

# Reports diffs without rewriting (for check/CI).
fmt-check:
	$(GOLANGCI) fmt --diff ./...

# go vet alone (the pre-commit hook; lint runs it too, with more).
vet:
	go vet ./...

# Auto-fix everything that can be fixed without human judgment.
fix:
	go mod tidy
	go fmt ./...
	$(GOLANGCI) run --fix ./...

# Advisories npm audit may report without failing the gate. GHSA-fc5q-39xc-vjpf: picomatch
# ReDoS, no patched release yet, and markdownlint only globs our own paths. Drop it once one ships.
NPM_AUDIT_IGNORE := GHSA-fc5q-39xc-vjpf

# Go modules, and the one npm tree the gates run (markdownlint-cli2's lock).
vuln:
	$(GOVULN) ./...
	@cd tools/markdownlint && found=$$(npm audit --omit=dev --json | jq -r --arg ignore '$(NPM_AUDIT_IGNORE)' \
		'[.vulnerabilities[].via[] | objects | select(.url | split("/")[-1] | IN($$ignore | split(" ")[]) | not)] \
		| unique | .[] | "\(.severity) \(.name): \(.url)"') || exit 1; \
		if [ -n "$$found" ]; then echo "npm audit:"; echo "$$found"; exit 1; fi; echo "npm audit: nothing unignored"

# Scan history (only when this dir is its own repo root) and the working tree.
secrets:
	@$(ENSURE) gitleaks
	@if [ -n "$(OWN_REPO)" ]; then \
		gitleaks git --redact --no-banner; \
	elif [ -n "$(CHECK_STRICT)" ]; then \
		echo "secrets: FAILED — no history to scan: $(CURDIR) is not the root of its own git repository." >&2; \
		exit 1; \
	else \
		echo "secrets: history scan SKIPPED — $(CURDIR) is not the root of its own git repository."; \
	fi
	gitleaks dir --redact --no-banner

# Supply-chain guard: every GitHub Actions `uses:` ref must stay SHA-pinned.
pin-check:
	bash scripts/actions-pin-check.sh

# The tools go.mod cannot pin, at tools/versions.env's versions (cheap when current).
ensure-tools:
	@$(ENSURE)

# docs/ is gitignored (internal planning notes), so only the published Markdown.
docs-check:
	@$(ENSURE) markdownlint
	markdownlint-cli2 "*.md" ".github/**/*.md"

# go.mod/go.sum are tidy and every module matches its checksum.
mod-check:
	go mod tidy -diff
	go mod verify

# Every `check` gate is a CI step, and every CI make target is a `check` gate.
ci-parity:
	bash scripts/ci-parity-check.sh

# The gate scripts' own tests (ensure-tools, ci-parity), offline.
script-check:
	bash scripts/test/tools-test.sh

# No function may be reachable only from tests (tests build their own fixtures;
# scripts/deadcode-allow.txt lists the reasoned exceptions).
deadcode:
	bash scripts/deadcode-check.sh

# Enable the tracked pre-commit/pre-push hooks for this clone.
hooks:
	git config core.hooksPath .githooks
	@echo "hooks enabled: secrets + fmt + vet before a commit, secrets + make quick before a push."
	@echo "Install gitleaks for full secret-scan coverage:"
	@echo "  https://github.com/gitleaks/gitleaks"

# The tools the workflows pin by hand (Dependabot does not bump them) against their
# latest releases. Needs network.
pins-outdated:
	bash scripts/pinned-tools.sh

# Print the pinned versions of every tool (sanity check).
tools: ensure-tools
	@go version
	@go tool golangci-lint version
	@go tool govulncheck -version | head -2
	@echo "gitleaks $$(gitleaks version)"
	@markdownlint-cli2 --help | head -1

# Test-coverage report: total summary to stdout + browsable HTML in coverage.html.
coverage:
	go test -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1
	go tool cover -html=coverage.out -o coverage.html

# Floors in scripts/coverage-gate.sh; reuses the profile from `test`.
coverage-check: test
	bash scripts/coverage-gate.sh $(COVERPROFILE); status=$$?; rm -f $(COVERPROFILE); exit $$status

# The pre-push gate: what this machine can say in about a minute. The race detector and
# the coverage floors, the vulnerability scan and deadcode are CI's (`check`); pushing a
# tag runs them here too.
.PHONY: quick test-quick
quick: mod-check compile test-quick lint fmt-check secrets pin-check \
	ci-parity script-check docs-check

# Every CI gate, with CI's tools (ci-parity-check.sh holds the two lists equal).
# Non-mutating. CHECK_STRICT=1 also fails what would be skipped here.
check: mod-check compile coverage-check lint fmt-check vuln secrets pin-check \
	deadcode ci-parity script-check docs-check

# ── Deploy ───────────────────────────────────────────────────────────────────
# Default install is per-user ($HOME/.local/bin); `make deploy PREFIX=/usr/local`
# for machine-wide (sudo only if BINDIR is not writable).
PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin
BINARIES := hirdman

# Build and copy the binary into BINDIR. `install` replaces each file (unlink +
# create), so a hirdman that is running keeps its old binary.
install: build
	@set -eu; \
	if { [ -d "$(BINDIR)" ] && [ -w "$(BINDIR)" ]; } || mkdir -p "$(BINDIR)" 2>/dev/null; then SUDO=""; \
	else SUDO="sudo"; echo "==> $(BINDIR) is not writable — installing the binaries with sudo"; sudo mkdir -p "$(BINDIR)"; fi; \
	for b in $(BINARIES); do $$SUDO install -m755 "$$b" "$(BINDIR)/$$b"; done; \
	echo "==> binaries -> $(foreach b,$(BINARIES),$(BINDIR)/$(b))"; \
	case ":$$PATH:" in *":$(BINDIR):"*) ;; *) echo "!!  $(BINDIR) is not on PATH" ;; esac

# Build and install, with no gates: the pre-push hook runs `make quick` and CI `make
# check`. The running service is the nixos package (nixos/pkgs/hirdman.nix), not this.
deploy: install

# Remove what `install` wrote. Config and board data are left in place.
undeploy:
	@set -eu; \
	if [ -w "$(BINDIR)" ]; then SUDO=""; else SUDO="sudo"; fi; \
	for b in $(BINARIES); do \
	  if [ -e "$(BINDIR)/$$b" ]; then $$SUDO rm -f "$(BINDIR)/$$b"; echo "==> removed $(BINDIR)/$$b"; fi; \
	done; \
	echo "    config and board data were left in place"
