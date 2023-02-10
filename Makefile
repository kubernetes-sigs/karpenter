export KUBEBUILDER_ASSETS ?= ${HOME}/.kubebuilder/bin

help: ## Display help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

presubmit: verify test ## Run all steps required for code to be checked in

test: ## Run tests
	go test ./... \
		-race \
		--ginkgo.focus="${FOCUS}" \
		--ginkgo.v \
		-cover -coverprofile=coverage.out -outputdir=. -coverpkg=./...

deflake: ## Run randomized, racing tests until the test fails to catch flakes
	ginkgo \
		--race \
		--focus="${FOCUS}" \
		--randomize-all \
		--until-it-fails \
		-v \
		./pkg/...

verify: ## Verify code. Includes codegen, dependencies, linting, formatting, etc
	go mod tidy
	go generate ./...
	hack/boilerplate.sh
	go vet ./...
	golangci-lint run
	@git diff --quiet ||\
		{ echo "New file modification detected in the Git working tree. Please check in before commit."; git --no-pager diff --name-only | uniq | awk '{print "  - " $$0}'; \
		if [ "${CI}" == 'true' ]; then\
			exit 1;\
		fi;}

toolchain: ## Install developer toolchain
	./hack/toolchain.sh

.PHONY: help presubmit dev test verify toolchain
