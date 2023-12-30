# 🚀 MemoryStore: The Speedy In-Memory 🗂 Key-Value Store 🛠️

MemoryStore is an ultra-fast, in-memory key-value database developed in Go, emphasizing rapid data access, simple integration, and robust thread-safe operations. It excels in handling byte-slice data with flexible serialization options.

## Quick Start

Clone and navigate to the MemoryStore repository:

```bash
git clone https://github.com/BryceWayne/MemoryStore.git
cd MemoryStore
```

## How to Use

MemoryStore accepts `[]byte` as values, accommodating various serialization methods (JSON, gob, protobuf, etc.). Here's a quick guide to integrate MemoryStore into your Go application:

```go
import (
    "github.com/goccy/go-json" // Recommended for added performance in JSON serialization
    "log"
    "time"

    "github.com/BryceWayne/MemoryStore/memorystore"
)

type YourData struct {
    // Define your data structure here
}

func main() {
    ms := memorystore.NewMemoryStore()
    defer ms.Stop()

    // Convert your data to a byte slice
    dataToStore := YourData{/* ... */}
    serializedData, err := json.Marshal(dataToStore)
    if err != nil {
        log.Fatal(err)
    }

    // Store the data
    ms.Set("yourKey", serializedData, 10*time.Second)

    // Retrieve and use the data
    if data, found, _ := ms.Get("yourKey"); found {
        var yourData YourData
        if err := json.Unmarshal(data, &yourData); err != nil {
            log.Fatal(err)
        }
        // Process `yourData`...
    }

    // Remove the data
    ms.Delete("yourKey")
}
```

## Building and Testing

Build the project with the `Makefile`:

```bash
make build
```

This compiles the source into an executable.

Test the functionality:

```bash
make test
```

## Contributions

Your contributions make MemoryStore better. Feel free to submit pull requests, open issues for discussion, suggestions, or bug reports.