# Makefile for MemoryStore

.PHONY: build clean test

build:
    @echo "Building MemoryStore..."
    go build -o memory_store

test:
    @echo "Running tests..."
    go test ./...

clean:
    @echo "Cleaning up..."
    go clean
    rm -f memory_store
