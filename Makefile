# Makefile for MemoryStore

.PHONY: all build clean test bench lint fmt vet help

# Default target
all: clean fmt vet lint test build

# Build the project
build:
	@echo "Building MemoryStore..."
	go build -o memory_store

# Run tests with race detection
test:
	@echo "Running tests..."
	go test -race -cover ./...

# Run benchmarks
bench:
	@echo "Running benchmarks..."
	go test -bench=. ./...

# Run linter
lint:
	@echo "Running linter..."
	golangci-lint run ./...

# Format code
fmt:
	@echo "Formatting code..."
	go fmt ./...

# Vetting code
vet:
	@echo "Vetting code..."
	go vet ./...

# Clean build artifacts
clean:
	@echo "Cleaning up..."
	go clean
	rm -f memory_store

# Show help
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@echo "  all       Run clean, fmt, vet, lint, test, and build"
	@echo "  build     Build the project"
	@echo "  test      Run tests with race detector"
	@echo "  bench     Run benchmarks"
	@echo "  lint      Run linter (golangci-lint)"
	@echo "  fmt       Format code (go fmt)"
	@echo "  vet       Vet code (go vet)"
	@echo "  clean     Remove build artifacts"
	@echo "  help      Show this help message"
