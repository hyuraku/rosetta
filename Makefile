.PHONY: build test test-unit test-integration test-all clean run bench

# Build configuration
BINARY_NAME=rosetta
LDFLAGS=-ldflags="-linkmode=external"

# Go binary - use goenv if available, otherwise use system go
GO := $(shell command -v goenv >/dev/null 2>&1 && goenv which go 2>/dev/null || which go)
# Fix GOROOT for goenv bug where 1.23.4 binary points to 1.18.6 GOROOT
GO_VERSION := $(shell $(GO) version | grep -o 'go[0-9.]*' | head -1)
GOROOT := $(shell echo $(GO) | sed 's|/bin/go||' )

# Build the application
build:
	$(GO) build $(LDFLAGS) -o $(BINARY_NAME) main.go

# Run all tests
test: test-unit test-integration

# Run unit tests
test-unit:
	GOROOT=$(GOROOT) $(GO) test ./tests/unit -v $(LDFLAGS)

# Run integration tests
test-integration:
	GOROOT=$(GOROOT) $(GO) test ./tests/integration -v $(LDFLAGS)

# Run all tests including performance
test-all:
	GOROOT=$(GOROOT) $(GO) test ./... -v $(LDFLAGS)

# Run benchmarks
bench:
	GOROOT=$(GOROOT) $(GO) test ./... -bench=. -benchmem $(LDFLAGS)

# Run with race detector
test-race:
	GOROOT=$(GOROOT) $(GO) test ./... -race $(LDFLAGS)

# Clean build artifacts
clean:
	$(GO) clean
	rm -f $(BINARY_NAME)
	rm -rf data-*

# Run a single node for development
run:
	$(GO) run $(LDFLAGS) main.go -id=node1 -listen=localhost:8080 -http=localhost:9080

# Format code
fmt:
	$(GO) fmt ./...

# Run linter
lint:
	golangci-lint run

# Tidy dependencies
tidy:
	$(GO) mod tidy

# Install development dependencies
deps:
	$(GO) mod download
	$(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

# Display help
help:
	@echo "Rosetta Makefile Commands:"
	@echo "  make build           - Build the application"
	@echo "  make test            - Run unit and integration tests"
	@echo "  make test-unit       - Run unit tests only"
	@echo "  make test-integration - Run integration tests only"
	@echo "  make test-all        - Run all tests"
	@echo "  make bench           - Run benchmarks"
	@echo "  make test-race       - Run tests with race detector"
	@echo "  make clean           - Clean build artifacts"
	@echo "  make run             - Run a single node for development"
	@echo "  make fmt             - Format code"
	@echo "  make lint            - Run linter"
	@echo "  make tidy            - Tidy dependencies"
	@echo "  make deps            - Install development dependencies"
	@echo "  make help            - Display this help message"
