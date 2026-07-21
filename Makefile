GOBIN ?= $$(go env GOPATH)/bin

# Rebuild the embedded local dashboard from web/dashboard into
# pkg/web/go-grpc/out. The out/ directory is a committed, generated artifact;
# regenerate and commit it whenever the dashboard source changes.
.PHONY: dashboard
dashboard:
	cd web/dashboard && npm ci && npm run build

.PHONY: install-go-test-coverage
install-go-test-coverage:
	go install github.com/vladopajic/go-test-coverage/v2@latest

.PHONY: check-coverage
check-coverage: install-go-test-coverage
	go test ./... -coverprofile=./cover.out -covermode=atomic -coverpkg=./...
	${GOBIN}/go-test-coverage --config=./.testcoverage.yaml
