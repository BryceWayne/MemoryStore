# Makefile for MemoryStore

.PHONY: all build clean test bench lint fmt vet coverage help

# Default target
all: clean fmt vet lint test build

# Build the project
build:
	@echo "Building MemoryStore..."
	go build -o memory_store

# Run tests with race detection and coverage
test:
	@echo "Running tests..."
	go test -count=1 -race -cover ./...

# Generate HTML coverage report
coverage:
	@echo "Generating coverage report..."
	go test -race -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report written to coverage.html"

# Run benchmarks with memory allocation stats
bench:
	@echo "Running benchmarks..."
	go test -run='^$$' -bench=. -benchmem ./...

# Run linter
lint:
	@echo "Running linter..."
	golangci-lint run ./...

# Format code
fmt:
	@echo "Formatting code..."
	go fmt ./...

# Vet code
vet:
	@echo "Vetting code..."
	go vet ./...

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
	@echo "  all       Run clean, fmt, vet, lint, test, and build"
	@echo "  build     Build the project"
	@echo "  test      Run tests with race detector and coverage"
	@echo "  coverage  Generate HTML coverage report (coverage.html)"
	@echo "  bench     Run benchmarks with memory stats"
	@echo "  lint      Run linter (golangci-lint)"
	@echo "  fmt       Format code (go fmt)"
	@echo "  vet       Vet code (go vet)"
	@echo "  clean     Remove build and test artifacts"
	@echo "  help      Show this help message"
