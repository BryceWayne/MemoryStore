# 🚀 MemoryStore

MemoryStore is a high-performance, thread-safe, in-memory key-value store implemented in Go. It features automatic key expiration, JSON serialization support, and concurrent access safety.

[![Go Report Card](https://goreportcard.com/badge/github.com/BryceWayne/MemoryStore)](https://goreportcard.com/report/github.com/BryceWayne/MemoryStore)
[![GoDoc](https://godoc.org/github.com/BryceWayne/MemoryStore?status.svg)](https://godoc.org/github.com/BryceWayne/MemoryStore)

## Features

- 🔄 Thread-safe operations
- ⏰ Automatic key expiration
- 🧹 Background cleanup of expired keys
- 📦 Support for both raw bytes and JSON data
- 💪 High-performance using go-json
- 🔒 Clean shutdown mechanism
- 📝 Comprehensive documentation

## Installation

```bash
go get github.com/BryceWayne/MemoryStore
```

## Quick Start

Here's a simple example demonstrating basic usage:

```go
package main

import (
    "log"
    "time"
    "github.com/BryceWayne/MemoryStore/memorystore"
)

func main() {
    // Create a new store instance
    store := memorystore.NewMemoryStore()
    defer store.Stop()

    // Store a string (converted to bytes)
    err := store.Set("greeting", []byte("Hello, World!"), 1*time.Minute)
    if err != nil {
        log.Fatal(err)
    }

    // Retrieve the value
    if value, exists := store.Get("greeting"); exists {
        log.Printf("Value: %s", string(value))
    }
}
```

## Advanced Usage

### Working with JSON

MemoryStore provides convenient methods for JSON serialization:

```go
type User struct {
    Name  string `json:"name"`
    Email string `json:"email"`
}

func main() {
    store := memorystore.NewMemoryStore()
    defer store.Stop()

    // Store JSON data
    user := User{Name: "Alice", Email: "alice@example.com"}
    err := store.SetJSON("user:123", user, 1*time.Hour)
    if err != nil {
        log.Fatal(err)
    }

    // Retrieve JSON data
    var retrievedUser User
    exists, err := store.GetJSON("user:123", &retrievedUser)
    if err != nil {
        log.Fatal(err)
    }
    if exists {
        log.Printf("User: %+v", retrievedUser)
    }
}
```

### Expiration and Cleanup

Keys automatically expire after their specified duration:

```go
// Set a value that expires in 5 seconds
store.Set("temp", []byte("temporary value"), 5*time.Second)

// The background cleanup routine will automatically remove expired items
// You can also manually check if an item exists
time.Sleep(6 * time.Second)
if _, exists := store.Get("temp"); !exists {
    log.Println("Key has expired")
}
```

### Proper Shutdown

Always ensure proper cleanup by calling `Stop()`:

```go
store := memorystore.NewMemoryStore()
defer func() {
    if err := store.Stop(); err != nil {
        log.Printf("Error stopping store: %v", err)
    }
}()
```

## Performance Considerations

- Uses `github.com/goccy/go-json` for faster JSON operations
- Minimizes lock contention with RWMutex
- Efficient background cleanup of expired items
- Memory-efficient storage using byte slices

## Building and Testing

Use the provided Makefile:

```bash
# Build the project
make build

# Run tests
make test

# Run benchmarks
make bench
```

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request. For major changes, please open an issue first to discuss what you would like to change.

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/AmazingFeature`)
3. Commit your changes (`git commit -m 'Add some AmazingFeature'`)
4. Push to the branch (`git push origin feature/AmazingFeature`)
5. Open a Pull Request

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## Acknowledgments

- Thanks to the Go team for the amazing standard library
- [go-json](https://github.com/goccy/go-json) for high-performance JSON operations

## Todo

- [ ] Add support for batch operations
- [ ] Implement data persistence
- [ ] Add metrics and monitoring
- [ ] Support for pattern-based key deletion
- [ ] Add compression options

## Support

If you have any questions or need help integrating MemoryStore, please:

1. Check the [documentation](https://godoc.org/github.com/BryceWayne/MemoryStore)
2. Open an issue with a detailed description
3. Reach out through the discussions tab