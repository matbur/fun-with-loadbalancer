BACKENDS ?= http://localhost:9001,http://localhost:9002
LISTEN ?= :8080

.PHONY: help run test test-race vet

help: ## show available targets
	@printf "Available targets:\n"
	@grep -hE '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk -F': *## ' '{printf "  %-10s %s\n", $$1, $$2}'

run: ## run the load balancer
	go run ./cmd/lb -backends "$(BACKENDS)" -listen "$(LISTEN)"

test: ## run all tests
	go test ./...

test-race: ## run tests with race detection
	go test -race ./...

vet: ## static analysis
	go vet ./...
