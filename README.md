# MemoryStore

MemoryStore is a simple, in-memory key-value store written in Go, optimized for flexibility and simplicity. It offers thread-safe operations to set, get, and delete byte-slice data, allowing custom serialization and deserialization methods.

## Installation

To install MemoryStore, clone this repository:

```bash
git clone https://github.com/BryceWayne/MemoryStore.git
cd MemoryStore
```

## Usage

MemoryStore operates with byte slices (`[]byte`) as values, giving you the freedom to serialize and deserialize data in any format you choose (JSON, gob, protobuf, etc.).

Here's an example of how to use MemoryStore in your Go project:

```go
import (
    "encoding/json"
    "log"
    "time"

    "github.com/BryceWayne/MemoryStore/memorystore"
)

type ExampleStruct struct {
    // Your struct fields here
}

func main() {
    ms := memorystore.NewMemoryStore()
    defer ms.Stop()

    // Serialize your data to a byte slice
    exampleData := ExampleStruct{/* ... */}
    data, err := json.Marshal(exampleData)
    if err != nil {
        log.Fatal(err)
    }

    // Set data in the MemoryStore
    ms.Set("exampleKey", data, 10*time.Second)

    // Get data from the MemoryStore
    if retrievedData, exists, _ := ms.Get("exampleKey"); exists {
        var decodedData ExampleStruct
        if err := json.Unmarshal(retrievedData, &decodedData); err != nil {
            log.Fatal(err)
        }
        // Use `decodedData`...
    }

    // Delete data from the MemoryStore
    ms.Delete("exampleKey")
}
```

## Building the Project

You can build the project using the provided `Makefile`:

```bash
make build
```

This will compile the source code into an executable.

## Running Tests

Run the tests to ensure everything is functioning as expected:

```bash
make test
```

## Contributing

Contributions to MemoryStore are welcome! Please feel free to submit pull requests or open issues to discuss potential improvements or report bugs.
