BACKENDS ?= http://localhost:9001,http://localhost:9002
LISTEN ?= :8080
BACKEND_PORT ?= 9001

.PHONY: help run backend test test-race test-repeat vet

help: ## show available targets
	@printf "Available targets:\n"
	@grep -hE '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk -F': *## ' '{printf "  %-10s %s\n", $$1, $$2}'

run: ## run the load balancer
	go run ./cmd/lb -backends "$(BACKENDS)" -listen "$(LISTEN)"

backend: ## run a test backend
	go run ./cmd/backend -port $(BACKEND_PORT)

test: ## run all tests
	go test ./...

test-race: ## run tests with race detection
	go test -race ./...

test-repeat: ## run core tests 50 times to catch flakiness
	go test -count=50 -run 'TestPool|TestServeHTTP' ./...

vet: ## static analysis
	go vet ./...
