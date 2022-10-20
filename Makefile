export KUBEBUILDER_ASSETS ?= ${HOME}/.kubebuilder/bin

help: ## Display help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

presubmit: verify codegen test ## Run all steps required for code to be checked in

test: ## Run tests
	go test -v -run=${TEST_FILTER} ./... \
		-race \
		-cover -coverprofile=coverage.out -outputdir=. -coverpkg=./...

verify: ## Verify code. Includes dependencies, linting, formatting, etc
	go mod tidy
	go vet ./...
	@git diff --quiet ||\
		{ echo "New file modification detected in the Git working tree. Please check in before commit."; git --no-pager diff --name-only | uniq | awk '{print "  - " $$0}'; \
		if [ $(MAKECMDGOALS) = 'ci' ]; then\
			exit 1;\
		fi;}

codegen: ## Generate code. Must be run if changes are made to ./pkg/apis/...
	controller-gen \
		object:headerFile="hack/boilerplate.go.txt" \
		crd \
		paths="./pkg/..." \
		output:crd:artifacts:config=chart/crds
	hack/boilerplate.sh

toolchain: ## Install developer toolchain
	./hack/toolchain.sh

.PHONY: help presubmit dev test verify codegen toolchain
