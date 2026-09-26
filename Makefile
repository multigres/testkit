##@ General

.DEFAULT_GOAL := help

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: fmt
fmt: ## Run go fmt against code
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code
	go vet ./...

# The version floor here is not cosmetic. 2.12.2 bundles honnef.co/go/tools
# v0.7.0, whose IR builder panics on Linux under Go 1.27 ("unexpected expr:
# *ast.KeyValueExpr"), taking buildir down and cascading into every analyzer
# that depends on it until the metalinter itself cannot run. It is invisible on
# darwin, so CI is the first place it shows up. The same release also fixed an
# incremental-cache bug that reported a generic method promoted from an
# embedded type in another package as undefined, which is the shape this whole
# module is built on. Do not go back below 2.13.2.
.PHONY: lint
lint: golangci-lint ## Run golangci-lint
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint and apply fixes
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify the golangci-lint configuration
	$(GOLANGCI_LINT) config verify

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum
	go mod tidy

.PHONY: verify
verify: tidy ## Fail if the tree is dirty after tidy
	@git diff --exit-code -- go.mod go.sum || { \
		echo "go.mod/go.sum are not tidy; commit the result of 'make tidy'"; \
		exit 1; \
	}

##@ Test

# -p 1 because every package that boots envtest starts its own API server, and
# parallel packages would race for ports and for the shared assets directory.
#
# -v is load-bearing rather than cosmetic, for the same reason multigres-operator
# passes it to its suite: a KnownDefect pin logs the defect it stands on while
# that defect is live, and go test discards the output of a passing test without
# it.
.PHONY: test
test: setup-envtest ## Run all tests
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		go test -v -p 1 -timeout 15m ./... -coverprofile=cover.out

# A separate target rather than a flag on the one above, because the budget is
# different: this package is where concurrent writers to the op log and
# concurrent reconciles through the interceptor are exercised, so the race
# detector has real work to do here and the slow tail is fatter.
.PHONY: test-race
test-race: setup-envtest test-race-tools ## Run all tests under the race detector, including the tools module
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		go test -race -v -p 1 -timeout 30m ./...

.PHONY: test-coverage
test-coverage: setup-envtest ## Run tests and report total coverage
	@mkdir -p coverage
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		go test -p 1 -timeout 15m ./... -coverprofile=coverage/combined.out -covermode=atomic
	@go tool cover -html=coverage/combined.out -o=coverage/combined.html
	@go tool cover -func=coverage/combined.out | tail -1

.PHONY: check
check: lint license-check test ## Lint and test this module, the pre-push gate

.PHONY: check-all
check-all: check check-tools ## Everything CI runs bar the race job

##@ Tools module

# tools/assertfix is its own Go module, so the root ./... never reaches it.
# Every target here is the same work done in that directory instead, and CI
# runs check-tools as its own job: a tool that rewrites other people's test
# sources is the last thing in this repo that should go unchecked.
TOOLS_DIR := tools/assertfix

.PHONY: check-tools
check-tools: lint-tools vet-tools test-tools ## Lint, vet and test the tools module

.PHONY: vet-tools
vet-tools: ## Run go vet against the tools module
	cd $(TOOLS_DIR) && go vet ./...

.PHONY: test-tools
test-tools: ## Run the tools module's tests
	cd $(TOOLS_DIR) && go test ./...

.PHONY: test-race-tools
test-race-tools: ## Run the tools module's tests under the race detector
	cd $(TOOLS_DIR) && go test -race ./...

.PHONY: lint-tools
lint-tools: golangci-lint ## Run golangci-lint against the tools module
	cd $(TOOLS_DIR) && $(GOLANGCI_LINT) run

.PHONY: tidy-tools
tidy-tools: ## Tidy the tools module
	cd $(TOOLS_DIR) && go mod tidy

##@ Security

.PHONY: vulncheck
vulncheck: ## Report known vulnerabilities in both modules
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...
	cd $(TOOLS_DIR) && go run golang.org/x/vuln/cmd/govulncheck@latest ./...

##@ Licensing

# Every source file carries an SPDX identifier. Checked rather than trusted,
# because the failure mode is a file added without one and nobody noticing for
# a year, by which point "add the header" is a commit touching every file and
# no reviewer reads it.
LICENSE_ID := SPDX-License-Identifier: Apache-2.0

.PHONY: license-check
license-check: ## Fail if any source file is missing its SPDX header
	@missing=$$(git ls-files '*.go' '*.yaml' '*.yml' '*.toml' 'Makefile' \
		| xargs grep -L '$(LICENSE_ID)' || true); \
	if [ -n "$$missing" ]; then \
		echo "missing '$(LICENSE_ID)':"; \
		echo "$$missing" | sed 's/^/  /'; \
		echo "run 'make license-fix'"; \
		exit 1; \
	fi

.PHONY: license-fix
license-fix: ## Add the SPDX header to any source file missing it
	@git ls-files '*.go' | xargs grep -L '$(LICENSE_ID)' | while read -r f; do \
		printf '// %s\n\n%s\n' '$(LICENSE_ID)' "$$(cat "$$f")" > "$$f"; \
		echo "added header to $$f"; \
	done

##@ Tooling

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

# renovate: datasource=github-releases depName=golangci/golangci-lint
GOLANGCI_LINT_VERSION ?= v2.13.2

# The setup-envtest tool is versioned off the controller-runtime release branch
# this module actually depends on, so the two cannot drift.
ENVTEST_VERSION ?= $(shell go list -m -f "{{ .Version }}" sigs.k8s.io/controller-runtime | awk -F'[v.]' '{printf "release-%d.%d", $$2, $$3}')

# Must match multigres-operator's, since its suite runs this code against the
# same API server version. A mismatch would mean the harness is tested against
# one Kubernetes and used against another.
ENVTEST_K8S_VERSION ?= 1.35

.PHONY: setup-envtest
setup-envtest: envtest ## Download the envtest binaries into ./bin
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path || { \
		echo "Error: failed to set up envtest binaries for Kubernetes $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Install setup-envtest locally if necessary
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Install golangci-lint locally if necessary
# golangci-lint's own go.mod selects an older toolchain than this module, and
# installing under it fails. Pin the install to whatever Go is running make.
$(GOLANGCI_LINT): export GOTOOLCHAIN = $(shell go env GOVERSION)
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

# go-install-tool installs $2@$3 as $1, keeping one binary per version so a
# version bump reinstalls rather than silently reusing the old one.
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $$(realpath $(1)-$(3)) $(1)
endef
