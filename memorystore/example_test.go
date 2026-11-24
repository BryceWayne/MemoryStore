package memorystore_test

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"fmt"
	"time"

	"github.com/BryceWayne/MemoryStore/v2/memorystore"
)

func ExampleMemoryStore_Set() {
	store, err := memorystore.New()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer func() { _ = store.Stop() }()

	if err := store.Set("greeting", []byte("hello, world"), time.Minute); err != nil {
		fmt.Println("error:", err)
		return
	}

	if val, ok := store.Get("greeting"); ok {
		fmt.Println(string(val))
	}
	// Output:
	// hello, world
}

func ExampleMemoryStore_SetJSON() {
	store, err := memorystore.New()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer func() { _ = store.Stop() }()

	type User struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}

	user := User{Name: "Alice", Role: "admin"}
	if err := store.SetJSON("user:1", &user, time.Minute); err != nil {
		fmt.Println("error:", err)
		return
	}

	var got User
	if exists, err := store.GetJSON("user:1", &got); err == nil && exists {
		fmt.Printf("%s (%s)\n", got.Name, got.Role)
	}
	// Output:
	// Alice (admin)
}

func ExampleMemoryStore_SetJSONWithOptions() {
	store, err := memorystore.New()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer func() { _ = store.Stop() }()

	data := map[string]int{"b": 2, "a": 1}
	if err := store.SetJSONWithOptions("sorted", data, time.Minute, json.Deterministic(true)); err != nil {
		fmt.Println("error:", err)
		return
	}

	if raw, ok := store.Get("sorted"); ok {
		fmt.Println(string(raw))
	}
	// Output:
	// {"a":1,"b":2}
}

func ExampleMemoryStore_SetMulti() {
	store, err := memorystore.New()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer func() { _ = store.Stop() }()

	items := map[string][]byte{
		"k1": []byte("v1"),
		"k2": []byte("v2"),
	}
	if err := store.SetMulti(items, time.Minute); err != nil {
		fmt.Println("error:", err)
		return
	}

	results := store.GetMulti([]string{"k1", "k2", "missing"})
	fmt.Printf("found=%d k1=%s k2=%s\n", len(results), results["k1"], results["k2"])
	// Output:
	// found=2 k1=v1 k2=v2
}

func ExampleMemoryStore_Subscribe() {
	store, err := memorystore.New()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer func() { _ = store.Stop() }()

	ch, err := store.Subscribe("events:*")
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	if err := store.Publish("events:login", []byte("user-42")); err != nil {
		fmt.Println("error:", err)
		return
	}

	msg := <-ch
	fmt.Println(string(msg))
	// Output:
	// user-42
}

func ExampleMemoryStore_GetJSONAs() {
	store, err := memorystore.New()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer func() { _ = store.Stop() }()

	type User struct {
		Name string `json:"name"`
	}

	_ = store.SetJSON("user:42", &User{Name: "Grace"}, time.Minute)

	user, ok, err := store.GetJSONAs[User]("user:42")
	if err == nil && ok {
		fmt.Println(user.Name)
	}
	// Output:
	// Grace
}

func ExampleMemoryStore_GetOrLoadJSON() {
	store, err := memorystore.New()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer func() { _ = store.Stop() }()

	type Product struct {
		SKU   string `json:"sku"`
		Price int    `json:"price"`
	}

	p, err := store.GetOrLoadJSON[Product](context.Background(), "sku:1", time.Minute, func(_ context.Context) (Product, error) {
		return Product{SKU: "sku:1", Price: 1999}, nil
	})
	if err == nil {
		fmt.Printf("%s=$%d\n", p.SKU, p.Price)
	}
	// Output:
	// sku:1=$1999
}

func ExampleMemoryStore_Save() {
	src, err := memorystore.New()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer func() { _ = src.Stop() }()

	_ = src.Set("session:1", []byte("active"), time.Minute)

	var snap bytes.Buffer
	if err := src.Save(&snap); err != nil {
		fmt.Println("save error:", err)
		return
	}

	dst, err := memorystore.New()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer func() { _ = dst.Stop() }()

	if err := dst.Load(&snap); err != nil {
		fmt.Println("load error:", err)
		return
	}
	if val, ok := dst.Get("session:1"); ok {
		fmt.Println(string(val))
	}
	// Output:
	// active
}
