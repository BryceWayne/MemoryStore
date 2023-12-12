# MemoryStore

MemoryStore is a simple, in-memory key-value store written in Go. It offers thread-safe operations to set, get, and delete data.

## Installation

To install MemoryStore, clone this repository:

```bash
git clone https://github.com/BryceWayne/MemoryStore.git
cd MemoryStore
```

## Usage

To use MemoryStore in your Go project, import it and create a new instance:

```go
import "github.com/BryceWayne/MemoryStore/memorystore"

func main() {
    ms := MemoryStore.NewMemoryStore()
    defer ms.Stop()
    
    ms.Set("key", "value", time.Second*10)

    // ...
}
```

## Building the Project

You can build the project using the provided `Makefile`:

```bash
make build
```

This will compile the source code into an executable.

## Running Tests

Run the tests using:

```bash
make test
```

## Contributing

Contributions to MemoryStore are welcome! Please feel free to submit pull requests.
