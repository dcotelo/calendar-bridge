# calendar-bridge developer tasks.
#
# `make ci` runs exactly what the CI workflow runs, so a green local run means
# a green pipeline. Everything here works with a stock Go toolchain; the tools
# target installs the two extras (golangci-lint, govulncheck) at the versions
# CI pins.

GO              ?= go
GOLANGCI_VERSION ?= v2.13.1
GOVULNCHECK_VERSION ?= v1.7.0

# Coverage gates. A single repo-wide percentage is a weak signal, so the gate is
# a modest overall floor plus a real floor for each package that carries logic.
# cmd/calendar-bridge is deliberately ungated: it is process plumbing (flag
# parsing, signal handling, os.Exit, ListenAndServe) whose unit tests would
# assert almost nothing. These numbers are what the suite actually meets today;
# raising them is a deliberate act, not something that happens by accident.
COVERAGE_MIN ?= 65
PKG_FLOORS ?= internal/sync:85 internal/webui:88 internal/config:90 \
              internal/googleauth:75 internal/webhook:65 internal/atomicfile:60

BIN      := bin/calendar-bridge
COVEROUT := coverage.out

# Must match prepare.sh's CB_DEMO_DIR default: `make demos` puts this on PATH
# so each tape's `Require calendar-bridge` can find the fixture binary.
DEMO_DIR ?= /tmp/calendar-bridge-demo

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: help
help: ## Show this help
	@# Plain ERE: `.*?` is a PCRE lazy quantifier, and POSIX leaves adjacent
	@# duplication operators undefined, so it is only portable by accident.
	@grep -hE '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | \
	  awk 'BEGIN {FS = ":.*## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the binary into bin/
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/calendar-bridge

.PHONY: fmt
fmt: ## Format all Go files
	gofmt -w .

.PHONY: fmt-check
fmt-check: ## Fail if any file is not gofmt'd
	@# gofmt reports unformatted files on stdout, but reports its OWN failures
	@# (an unparseable file, or a missing gofmt) on stderr with a non-zero
	@# status and an EMPTY stdout. Checking only $$out would then pass: a file
	@# with a syntax error silently satisfies fmt-check. Check the status first.
	@out=$$(gofmt -l .) || { echo "gofmt failed"; exit 1; }; \
	if [ -n "$$out" ]; then echo "not gofmt'd:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: test
test: ## Run the test suite
	$(GO) test ./...

.PHONY: test-race
test-race: ## Run the test suite under the race detector
	$(GO) test ./... -race

.PHONY: cover
cover: ## Run tests with coverage and enforce the gates
	$(GO) test ./... -race -coverprofile=$(COVEROUT) -covermode=atomic
	@$(GO) tool cover -func=$(COVEROUT) | tail -1
	@# Each coverage number is produced by a command whose exit status a pipe
	@# would discard. An empty total then takes the else branch and prints
	@# "ok total %" — a gate that passes when it could not measure anything is
	@# worse than no gate, because it is trusted. Capture the status, then
	@# require the value to be an integer before comparing.
	@cover_raw=$$($(GO) tool cover -func=$(COVEROUT)) || { echo "FAIL  could not read $(COVEROUT)"; exit 1; }; \
	total=$$(printf '%s\n' "$$cover_raw" | awk '/^total:/ {gsub(/%/,"",$$3); print int($$3)}'); \
	case "$$total" in \
	  ''|*[!0-9]*) echo "FAIL  total coverage unreadable (got '$$total')"; exit 1;; \
	esac; \
	fail=0; \
	if [ "$$total" -lt "$(COVERAGE_MIN)" ]; then \
	  echo "FAIL  total $$total% < $(COVERAGE_MIN)%"; fail=1; \
	else \
	  echo "ok    total $$total% (floor $(COVERAGE_MIN)%)"; \
	fi; \
	for spec in $(PKG_FLOORS); do \
	  pkg=$${spec%%:*}; floor=$${spec##*:}; \
	  if ! out=$$($(GO) test ./$$pkg/ -cover 2>&1); then \
	    echo "FAIL  $$pkg: tests failed, coverage not trusted"; fail=1; continue; \
	  fi; \
	  got=$$(printf '%s\n' "$$out" | sed -n 's/.*coverage: \([0-9]*\)\.[0-9]*%.*/\1/p'); \
	  case "$$got" in \
	    ''|*[!0-9]*) echo "FAIL  $$pkg: could not read coverage"; fail=1; continue;; \
	  esac; \
	  if [ "$$got" -lt "$$floor" ]; then \
	    echo "FAIL  $$pkg $$got% < $$floor%"; fail=1; \
	  else \
	    echo "ok    $$pkg $$got% (floor $$floor%)"; \
	  fi; \
	done; \
	exit $$fail

.PHONY: cover-html
cover-html: ## Open the coverage report in a browser
	$(GO) test ./... -coverprofile=$(COVEROUT)
	$(GO) tool cover -html=$(COVEROUT)

.PHONY: lint
lint: ## Run golangci-lint
	@command -v golangci-lint >/dev/null || { echo "golangci-lint not found; run: make tools"; exit 1; }
	golangci-lint run

.PHONY: vuln
vuln: ## Scan dependencies for known vulnerabilities
	@command -v govulncheck >/dev/null || { echo "govulncheck not found; run: make tools"; exit 1; }
	govulncheck ./...

.PHONY: fuzz
fuzz: ## Run each fuzz target briefly (FUZZTIME to change the budget)
	$(GO) test ./internal/config/ -run=Fuzz -fuzz=FuzzLoad -fuzztime=$(or $(FUZZTIME),30s)
	$(GO) test ./internal/sync/ -run=Fuzz -fuzz=FuzzSourceIdentity -fuzztime=$(or $(FUZZTIME),30s)

.PHONY: tools
tools: ## Install the pinned lint and vulnerability tools
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)
	$(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

.PHONY: ci
ci: build vet fmt-check test-race cover lint vuln ## Everything CI runs
	@echo "ci: all checks passed"

.PHONY: screenshots
screenshots: ## Regenerate the web UI screenshots from the fixture config
	./scripts/screenshots/capture.sh

.PHONY: demos
demos: ## Regenerate the terminal demo GIFs (requires vhs)
	@command -v vhs >/dev/null || { echo "vhs not found: https://github.com/charmbracelet/vhs"; exit 1; }
	@# The fixture must exist BEFORE vhs runs. Nothing invoked prepare.sh
	@# before, so `make demos` failed at each tape's `Require calendar-bridge`
	@# with no hint that a setup step was missing.
	./scripts/demos/prepare.sh
	@# And the fixture directory must be on PATH here, not just inside the
	@# recorded shell: `Require` is evaluated when the tape starts, before the
	@# tape's own line that extends PATH for the recording.
	for tape in scripts/demos/*.tape; do PATH="$(DEMO_DIR):$$PATH" vhs "$$tape"; done

.PHONY: clean
clean: ## Remove build and coverage artifacts
	rm -rf bin $(COVEROUT)
