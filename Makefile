# Makefile for MemoryStore

.PHONY: all build clean test bench bench-compare lint fmt vet coverage run-example help

SUBMODULES := pubsub/gcp pubsub/azure pubsub/redis pubsub/nats pubsub/aws pubsub/kafka

# Default target
all: clean fmt vet lint test build

# Build all modules
build:
	@echo "Building MemoryStore..."
	go build ./...
	@for mod in $(SUBMODULES); do \
		echo "Building $$mod..."; \
		(cd $$mod && go build ./...) || exit 1; \
	done
	@echo "Verifying pubsub/kafka pure-Go build (CGO_ENABLED=0)..."
	cd pubsub/kafka && CGO_ENABLED=0 go build ./...

# Run tests with race detection and coverage across all modules
test:
	@echo "Running tests (root module)..."
	go test -count=1 -race -cover ./...
	@for mod in $(SUBMODULES); do \
		echo "Running tests ($$mod)..."; \
		(cd $$mod && go test -count=1 -race -cover ./...) || exit 1; \
	done

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

# Run head-to-head comparison benchmarks against peer Go caches
bench-compare:
	@echo "Running head-to-head comparison benchmarks..."
	cd benchmarks && go test -run='^$$' -bench=. -benchmem -benchtime=400ms -count=5 .

# Run linter across all modules
lint:
	@echo "Running linter (root module)..."
	golangci-lint run ./...
	@for mod in $(SUBMODULES); do \
		echo "Running linter ($$mod)..."; \
		(cd $$mod && golangci-lint run --config ../../.golangci.yml ./...) || exit 1; \
	done

# Format code across all modules
fmt:
	@echo "Formatting code..."
	go fmt ./...
	@for mod in $(SUBMODULES); do \
		(cd $$mod && go fmt ./...) || exit 1; \
	done

# Vet code across all modules
vet:
	@echo "Vetting code..."
	go vet ./...
	@for mod in $(SUBMODULES); do \
		(cd $$mod && go vet ./...) || exit 1; \
	done

# Run the basic usage example
run-example:
	go run ./examples/basic

# Clean build and coverage artifacts
clean:
	@echo "Cleaning up..."
	go clean
	@for mod in $(SUBMODULES); do \
		(cd $$mod && go clean) || exit 1; \
	done
	rm -f memory_store coverage.out coverage.html gofmt_diff.txt pubsub/*/coverage.out pubsub/*/*.test

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
