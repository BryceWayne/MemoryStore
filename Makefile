# Makefile for MemoryStore

.PHONY: all build clean test bench lint fmt vet coverage run-example help

# Default target
all: clean fmt vet lint test build

# Build all modules
build:
	@echo "Building MemoryStore..."
	go build ./...
	@echo "Building pubsub/gcp..."
	cd pubsub/gcp && go build ./...

# Run tests with race detection and coverage across all modules
test:
	@echo "Running tests (root module)..."
	go test -count=1 -race -cover ./...
	@echo "Running tests (pubsub/gcp)..."
	cd pubsub/gcp && go test -count=1 -race -cover ./...

# Generate HTML coverage report for core memorystore
coverage:
	@echo "Generating coverage report..."
	go test -race -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report written to coverage.html"

# Run benchmarks with memory allocation stats
bench:
	@echo "Running benchmarks..."
	go test -run='^$$' -bench=. -benchmem ./memorystore

# Run linter across all modules
lint:
	@echo "Running linter (root module)..."
	golangci-lint run ./...
	@echo "Running linter (pubsub/gcp)..."
	cd pubsub/gcp && golangci-lint run --config ../../.golangci.yml ./...

# Format code across all modules
fmt:
	@echo "Formatting code..."
	go fmt ./...
	cd pubsub/gcp && go fmt ./...

# Vet code across all modules
vet:
	@echo "Vetting code..."
	go vet ./...
	cd pubsub/gcp && go vet ./...

# Run the basic usage example
run-example:
	go run ./examples/basic

# Clean build and coverage artifacts
clean:
	@echo "Cleaning up..."
	go clean
	rm -f memory_store coverage.out coverage.html gofmt_diff.txt

# Show help
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@echo "  all          Run clean, fmt, vet, lint, test, and build"
	@echo "  build        Build all modules"
	@echo "  test         Run tests with race detector and coverage"
	@echo "  coverage     Generate HTML coverage report (coverage.html)"
	@echo "  bench        Run benchmarks with memory stats"
	@echo "  lint         Run linter (golangci-lint) across all modules"
	@echo "  fmt          Format code (go fmt) across all modules"
	@echo "  vet          Vet code (go vet) across all modules"
	@echo "  run-example  Run examples/basic"
	@echo "  clean        Remove build and test artifacts"
	@echo "  help         Show this help message"
