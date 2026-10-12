GOBIN ?= $$(go env GOPATH)/bin

# Rebuild the embedded local dashboard from web/dashboard into
# pkg/web/go-grpc/out. The out/ directory is a committed, generated artifact;
# regenerate and commit it whenever the dashboard source changes.
.PHONY: dashboard
dashboard:
	cd web/dashboard && npm ci && npm run build

# Keep in sync with the pinned version in .github/workflows/go.yml.
GOLANGCI_LINT_VERSION ?= v2.14.0

.PHONY: install-golangci-lint
install-golangci-lint:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}

# The gate itself: the whole repository, no baseline — CI runs the same full
# report, and the rule is that it is clean at the head, pre-existing findings
# fixed in the PR that meets them.
.PHONY: lint
lint: install-golangci-lint
	${GOBIN}/golangci-lint run ./...

.PHONY: install-go-test-coverage
install-go-test-coverage:
	go install github.com/vladopajic/go-test-coverage/v2@latest

.PHONY: check-coverage
check-coverage: install-go-test-coverage
	go test ./... -coverprofile=./cover.out -covermode=atomic -coverpkg=./...
	${GOBIN}/go-test-coverage --config=./.testcoverage.yaml
