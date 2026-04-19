// This file is adapted from github.com/pocketbase/pocketbase/plugins/jsvm.
// Copyright (c) 2022 - present, Gani Georgiev. Distributed under the MIT License.
// https://github.com/pocketbase/pocketbase/blob/master/LICENSE.md
//
// Modifications for the Ramune JS engine and Soda-specific coverage:
// Copyright (c) 2026 - present, Yasushi Itoh.

package soda

import (
	"fmt"
	"testing"
	"time"

	"github.com/i2y/ramune"
)

// newTestRuntime creates a Ramune runtime with all shared bindings for testing.
func newTestRuntime(t *testing.T) (*ramune.Runtime, *typeProxy) {
	t.Helper()

	rt, err := ramune.New(ramune.NodeCompat())
	if err != nil {
		t.Fatalf("Failed to create runtime: %v", err)
	}

	registry := newObjectRegistry()
	proxy := newTypeProxy(registry)
	proxy.installProxyRuntime(rt)

	baseBinds(rt, proxy)

	return rt, proxy
}

func TestBaseBindsSleep(t *testing.T) {
	rt, _ := newTestRuntime(t)
	defer rt.Close()

	start := time.Now()
	err := rt.Exec(`sleep(100)`)
	duration := time.Since(start)

	if err != nil {
		t.Fatal(err)
	}
	if duration < 100*time.Millisecond || duration > 250*time.Millisecond {
		t.Fatalf("Expected ~100ms sleep, got %v", duration)
	}
}

func TestBaseBindsToString(t *testing.T) {
	rt, _ := newTestRuntime(t)
	defer rt.Close()

	scenarios := []struct {
		code     string
		expected string
	}{
		{`toString("")`, ""},
		{`toString("hello")`, "hello"},
		{`toString(123)`, "123"},
		{`toString(true)`, "true"},
	}

	for _, s := range scenarios {
		v, err := rt.Eval(s.code)
		if err != nil {
			t.Fatalf("Failed for %q: %v", s.code, err)
		}
		result, _ := v.GoString()
		v.Close()
		if result != s.expected {
			t.Fatalf("Expected %q for %s, got %q", s.expected, s.code, result)
		}
	}
}

func TestBaseBindsToBytes(t *testing.T) {
	rt, _ := newTestRuntime(t)
	defer rt.Close()

	v, err := rt.Eval(`toBytes("hello")`)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()

	bytes, err := v.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if string(bytes) != "hello" {
		t.Fatalf("Expected 'hello', got %q", string(bytes))
	}
}

func TestBaseBindsNullHelpers(t *testing.T) {
	rt, _ := newTestRuntime(t)
	defer rt.Close()

	// Each null helper should return a proxy handle
	for _, fn := range []string{"nullString", "nullFloat", "nullInt", "nullBool", "nullArray", "nullObject"} {
		v, err := rt.Eval(fn + "()")
		if err != nil {
			t.Fatalf("Failed for %s: %v", fn, err)
		}

		m, err := v.ToMap()
		v.Close()
		if err != nil {
			t.Fatalf("Expected map result for %s, got error: %v", fn, err)
		}
		if _, ok := m["__handle"]; !ok {
			t.Fatalf("Expected __handle in result of %s", fn)
		}
	}
}

func TestBaseBindsRecord(t *testing.T) {
	rt, _ := newTestRuntime(t)
	defer rt.Close()

	// Record with no args should return a proxy handle
	v, err := rt.Eval(`Record()`)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := v.ToMap()
	v.Close()

	if m["__type"] != "Record" {
		t.Fatalf("Expected __type 'Record', got %v", m["__type"])
	}
}

func TestBaseBindsCollection(t *testing.T) {
	rt, _ := newTestRuntime(t)
	defer rt.Close()

	v, err := rt.Eval(`Collection({name: "test"})`)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := v.ToMap()
	v.Close()

	if m["__type"] != "Collection" {
		t.Fatalf("Expected __type 'Collection', got %v", m["__type"])
	}
}

func TestBaseBindsMiddleware(t *testing.T) {
	rt, _ := newTestRuntime(t)
	defer rt.Close()

	v, err := rt.Eval(`Middleware(function(e) { return e.next(); }, 100, "test_mw")`)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := v.ToMap()
	v.Close()

	if m["id"] != "test_mw" {
		t.Fatalf("Expected id 'test_mw', got %v", m["id"])
	}
	priority, _ := m["priority"].(float64)
	if int(priority) != 100 {
		t.Fatalf("Expected priority 100, got %v", m["priority"])
	}
}

func TestSecurityBinds(t *testing.T) {
	rt, _ := newTestRuntime(t)
	defer rt.Close()

	securityBinds(rt)

	// Test md5
	v, err := rt.Eval(`$security.md5("hello")`)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := v.GoString()
	v.Close()
	if result != "5d41402abc4b2a76b9719d911017c592" {
		t.Fatalf("Expected md5 hash, got %q", result)
	}

	// Test sha256
	v, err = rt.Eval(`$security.sha256("hello")`)
	if err != nil {
		t.Fatal(err)
	}
	result, _ = v.GoString()
	v.Close()
	if result != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("Expected sha256 hash, got %q", result)
	}

	// Test randomString
	v, err = rt.Eval(`$security.randomString(10)`)
	if err != nil {
		t.Fatal(err)
	}
	result, _ = v.GoString()
	v.Close()
	if len(result) != 10 {
		t.Fatalf("Expected 10 char random string, got %d chars: %q", len(result), result)
	}

	// Test encrypt/decrypt
	v, err = rt.Eval(`
		var encrypted = $security.encrypt("test data", "12345678901234567890123456789012");
		$security.decrypt(encrypted, "12345678901234567890123456789012");
	`)
	if err != nil {
		t.Fatal(err)
	}
	result, _ = v.GoString()
	v.Close()
	if result != "test data" {
		t.Fatalf("Expected 'test data' after encrypt/decrypt, got %q", result)
	}
}

func TestDbxBinds(t *testing.T) {
	rt, _ := newTestRuntime(t)
	defer rt.Close()

	dbxBinds(rt)

	// Test that $dbx object exists
	v, err := rt.Eval(`typeof $dbx`)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := v.GoString()
	v.Close()
	if result != "object" {
		t.Fatalf("Expected $dbx to be object, got %q", result)
	}

	// Test that key methods exist
	v, err = rt.Eval(`typeof $dbx.exp`)
	if err != nil {
		t.Fatal(err)
	}
	result, _ = v.GoString()
	v.Close()
	if result != "function" {
		t.Fatalf("Expected $dbx.exp to be function, got %q", result)
	}
}

func TestFilepathBinds(t *testing.T) {
	rt, _ := newTestRuntime(t)
	defer rt.Close()

	filepathBinds(rt)

	// Test join
	v, err := rt.Eval(`$filepath.join("a", "b", "c")`)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := v.GoString()
	v.Close()
	if result != "a/b/c" {
		t.Fatalf("Expected 'a/b/c', got %q", result)
	}

	// Test base
	v, err = rt.Eval(`$filepath.base("/foo/bar/baz.txt")`)
	if err != nil {
		t.Fatal(err)
	}
	result, _ = v.GoString()
	v.Close()
	if result != "baz.txt" {
		t.Fatalf("Expected 'baz.txt', got %q", result)
	}

	// Test ext
	v, err = rt.Eval(`$filepath.ext("/foo/bar/baz.txt")`)
	if err != nil {
		t.Fatal(err)
	}
	result, _ = v.GoString()
	v.Close()
	if result != ".txt" {
		t.Fatalf("Expected '.txt', got %q", result)
	}
}

func TestOsBinds(t *testing.T) {
	rt, _ := newTestRuntime(t)
	defer rt.Close()

	registry := newObjectRegistry()
	proxy := newTypeProxy(registry)
	osBinds(rt, proxy)

	// Test getenv
	v, err := rt.Eval(`$os.getenv("PATH")`)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := v.GoString()
	v.Close()
	if result == "" {
		t.Fatal("Expected non-empty PATH")
	}

	// Test getwd
	v, err = rt.Eval(`$os.getwd()`)
	if err != nil {
		t.Fatal(err)
	}
	result, _ = v.GoString()
	v.Close()
	if result == "" {
		t.Fatal("Expected non-empty working directory")
	}
}

func TestProxySystem(t *testing.T) {
	rt, err := ramune.New(ramune.NodeCompat())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	registry := newObjectRegistry()
	proxy := newTypeProxy(registry)

	// Register a test type
	type TestStruct struct {
		Name  string
		Value int
	}
	proxy.registerType("TestStruct", &TestStruct{})
	proxy.installProxyRuntime(rt)
	installTypeWrappers(rt, proxy)

	// Register an instance
	instance := &TestStruct{Name: "hello", Value: 42}
	handle := registry.register(instance)

	// Test field access
	v, err := rt.Eval(`
		var obj = __proxy_wrap("TestStruct", ` + itoa(handle) + `);
		obj.name;
	`)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := v.GoString()
	v.Close()
	if result != "hello" {
		t.Fatalf("Expected 'hello', got %q", result)
	}

	// Test field set
	err = rt.Exec(`
		var obj = __proxy_wrap("TestStruct", ` + itoa(handle) + `);
		__proxy_set_field(` + itoa(handle) + `, "name", "world");
	`)
	if err != nil {
		t.Fatal(err)
	}
	if instance.Name != "world" {
		t.Fatalf("Expected Name to be 'world', got %q", instance.Name)
	}
}

func itoa(n int64) string {
	return fmt.Sprintf("%d", n)
}
