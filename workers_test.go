package soda

import (
	"os"
	"strings"
	"testing"
)

func TestIsWorkersStyle(t *testing.T) {
	tests := []struct {
		name     string
		code     string
		expected bool
	}{
		{
			name:     "old-style routerAdd",
			code:     `routerAdd("GET", "/test", (e) => { e.json(200, {}); })`,
			expected: false,
		},
		{
			name:     "old-style cronAdd",
			code:     `cronAdd("job1", "* * * * *", () => { console.log("tick"); })`,
			expected: false,
		},
		{
			name:     "old-style onRecordCreate",
			code:     `onRecordCreate((e) => { console.log(e); }, "posts")`,
			expected: false,
		},
		{
			name:     "workers-style fetch only",
			code:     `export default { async fetch(req, env) { return new Response("hi"); } }`,
			expected: true,
		},
		{
			name:     "workers-style with route",
			code:     `export default { route: "/api/test", async fetch(req, env) { return new Response("hi"); } }`,
			expected: true,
		},
		{
			name:     "workers-style with scheduled",
			code:     `export default { cron: "* * * * *", async scheduled(event, env) {} }`,
			expected: true,
		},
		{
			name:     "workers-style TypeScript",
			code:     `export default { route: "/api", async fetch(req: Request, env: Env): Promise<Response> { return new Response("hi"); } }`,
			expected: true,
		},
		{
			name:     "empty file",
			code:     "",
			expected: false,
		},
		{
			name:     "export default in string literal still matches",
			code:     `var x = "export default is a keyword";`,
			expected: true, // acceptable false positive — old-style files never contain this
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isWorkersStyle(tt.code); got != tt.expected {
				t.Errorf("isWorkersStyle() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestTranspileWorkersModule(t *testing.T) {
	code := `export default { route: "/api/hello", async fetch(req, env) { return new Response("hello"); } };`

	result, err := transpileWorkersModule("test.pb.ts", code)
	if err != nil {
		t.Fatalf("transpileWorkersModule failed: %v", err)
	}

	// The output should define __workers_export as a global.
	if !strings.Contains(result, "__workers_export") {
		t.Fatal("expected __workers_export in IIFE output")
	}

	// It should be an IIFE (self-invoking function).
	if !strings.Contains(result, "(() => {") && !strings.Contains(result, "(function()") {
		t.Fatal("expected IIFE pattern in output")
	}
}

func TestTranspileWorkersModuleJS(t *testing.T) {
	code := `export default { async fetch(req, env) { return new Response("js"); } };`

	result, err := transpileWorkersModule("test.pb.js", code)
	if err != nil {
		t.Fatalf("transpileWorkersModule failed: %v", err)
	}

	if !strings.Contains(result, "__workers_export") {
		t.Fatal("expected __workers_export in IIFE output for .js file")
	}
}

func TestTranspileWorkersModuleError(t *testing.T) {
	// Invalid TypeScript should produce an error.
	code := `export default { fetch(: invalid syntax`

	_, err := transpileWorkersModule("bad.pb.ts", code)
	if err == nil {
		t.Fatal("expected an error for invalid syntax")
	}
}

func TestRewritePositionalParams(t *testing.T) {
	tests := []struct {
		sql      string
		params   []any
		expected string
		nParams  int
	}{
		{
			sql:      "SELECT * FROM users WHERE id = ?",
			params:   []any{123},
			expected: "SELECT * FROM users WHERE id = {:p0}",
			nParams:  1,
		},
		{
			sql:      "SELECT * FROM users WHERE name = ? AND age > ?",
			params:   []any{"alice", 30},
			expected: "SELECT * FROM users WHERE name = {:p0} AND age > {:p1}",
			nParams:  2,
		},
		{
			sql:      "SELECT * FROM users WHERE name = '?'",
			params:   []any{},
			expected: "SELECT * FROM users WHERE name = '?'",
			nParams:  0,
		},
		{
			sql:      "SELECT * FROM users",
			params:   nil,
			expected: "SELECT * FROM users",
			nParams:  0,
		},
	}

	for _, tt := range tests {
		result, params := rewritePositionalParams(tt.sql, tt.params)
		if result != tt.expected {
			t.Errorf("rewritePositionalParams(%q) = %q, want %q", tt.sql, result, tt.expected)
		}
		if len(params) != tt.nParams {
			t.Errorf("expected %d params, got %d", tt.nParams, len(params))
		}
	}
}

func TestEnvSecrets(t *testing.T) {
	// Set test secrets.
	os.Setenv("POCKETBASE_SECRET_API_KEY", "test-key-123")
	os.Setenv("POCKETBASE_SECRET_DB_PASS", "s3cret")
	defer os.Unsetenv("POCKETBASE_SECRET_API_KEY")
	defer os.Unsetenv("POCKETBASE_SECRET_DB_PASS")

	rt, _ := newTestRuntime(t)
	defer rt.Close()

	envSecretsBinds(rt)

	// Test listing secrets.
	v, err := rt.Eval(`JSON.stringify(__env_list_secrets())`)
	if err != nil {
		t.Fatalf("__env_list_secrets failed: %v", err)
	}
	result, _ := v.GoString()
	v.Close()

	if !strings.Contains(result, `"API_KEY":"test-key-123"`) {
		t.Fatalf("expected API_KEY in secrets, got %s", result)
	}
	if !strings.Contains(result, `"DB_PASS":"s3cret"`) {
		t.Fatalf("expected DB_PASS in secrets, got %s", result)
	}

	// Verify non-secret env vars are excluded.
	if strings.Contains(result, "PATH") {
		t.Fatal("non-secret env var PATH should not appear in secrets")
	}
}

func TestEnvSecretsJSBuilder(t *testing.T) {
	os.Setenv("POCKETBASE_SECRET_TOKEN", "abc123")
	defer os.Unsetenv("POCKETBASE_SECRET_TOKEN")

	rt, _ := newTestRuntime(t)
	defer rt.Close()

	envSecretsBinds(rt)
	envJSInstall(rt)

	// $app is needed by __buildEnv but for this test we just test __buildEnvSecrets.
	v, err := rt.Eval(`
		var s = __buildEnvSecrets();
		s.TOKEN;
	`)
	if err != nil {
		t.Fatalf("__buildEnvSecrets failed: %v", err)
	}
	result, _ := v.GoString()
	v.Close()

	if result != "abc123" {
		t.Fatalf("expected TOKEN = 'abc123', got %q", result)
	}
}

func TestEnvSecretsIsFrozen(t *testing.T) {
	os.Setenv("POCKETBASE_SECRET_X", "1")
	defer os.Unsetenv("POCKETBASE_SECRET_X")

	rt, _ := newTestRuntime(t)
	defer rt.Close()

	envSecretsBinds(rt)
	envJSInstall(rt)

	v, err := rt.Eval(`
		var s = __buildEnvSecrets();
		Object.isFrozen(s);
	`)
	if err != nil {
		t.Fatalf("Object.isFrozen check failed: %v", err)
	}
	frozen, _ := v.Bool()
	v.Close()

	if !frozen {
		t.Fatal("expected secrets object to be frozen")
	}
}

func TestEnvDBBuilder(t *testing.T) {
	rt, _ := newTestRuntime(t)
	defer rt.Close()

	// Register a mock __env_db_exec for testing the JS builder shape.
	rt.RegisterFunc("__env_db_exec", func(args []any) (any, error) {
		sql, _ := args[0].(string)
		params, _ := args[1].([]any)
		isQuery, _ := args[2].(bool)
		return map[string]any{
			"_sql":     sql,
			"_params":  params,
			"_isQuery": isQuery,
			"results":  []any{map[string]any{"id": float64(1), "name": "test"}},
			"success":  true,
		}, nil
	})

	envJSInstall(rt)

	// Test prepare().bind().all()
	v, err := rt.Eval(`
		var db = __buildEnvDB();
		var r = db.prepare("SELECT * FROM users WHERE id = ?").bind(42).all();
		JSON.stringify(r);
	`)
	if err != nil {
		t.Fatalf("DB.prepare.bind.all failed: %v", err)
	}
	result, _ := v.GoString()
	v.Close()

	if !strings.Contains(result, `"_sql":"SELECT * FROM users WHERE id = ?"`) {
		t.Fatalf("unexpected SQL in result: %s", result)
	}
	if !strings.Contains(result, `"_isQuery":true`) {
		t.Fatalf("expected isQuery=true: %s", result)
	}

	// Test prepare().bind().first()
	v, err = rt.Eval(`
		var db = __buildEnvDB();
		var r = db.prepare("SELECT * FROM users WHERE id = ?").bind(1).first();
		JSON.stringify(r);
	`)
	if err != nil {
		t.Fatalf("DB.prepare.bind.first failed: %v", err)
	}
	result, _ = v.GoString()
	v.Close()

	if !strings.Contains(result, `"id":1`) {
		t.Fatalf("expected first() to return first row: %s", result)
	}

	// Test prepare().bind().run()
	v, err = rt.Eval(`
		var db = __buildEnvDB();
		var r = db.prepare("INSERT INTO users (name) VALUES (?)").bind("alice").run();
		JSON.stringify(r);
	`)
	if err != nil {
		t.Fatalf("DB.prepare.bind.run failed: %v", err)
	}
	result, _ = v.GoString()
	v.Close()

	if !strings.Contains(result, `"_isQuery":false`) {
		t.Fatalf("expected isQuery=false for run(): %s", result)
	}
}

func TestEnvKVBuilder(t *testing.T) {
	rt, _ := newTestRuntime(t)
	defer rt.Close()

	// Mock KV Go functions.
	store := map[string]map[string]string{} // namespace -> key -> value

	rt.RegisterFunc("__env_kv_get", func(args []any) (any, error) {
		ns, _ := args[0].(string)
		key, _ := args[1].(string)
		if m, ok := store[ns]; ok {
			if v, ok := m[key]; ok {
				return v, nil
			}
		}
		return nil, nil
	})
	rt.RegisterFunc("__env_kv_put", func(args []any) (any, error) {
		ns, _ := args[0].(string)
		key, _ := args[1].(string)
		val, _ := args[2].(string)
		if store[ns] == nil {
			store[ns] = map[string]string{}
		}
		store[ns][key] = val
		return nil, nil
	})
	rt.RegisterFunc("__env_kv_delete", func(args []any) (any, error) {
		ns, _ := args[0].(string)
		key, _ := args[1].(string)
		if m, ok := store[ns]; ok {
			delete(m, key)
		}
		return nil, nil
	})
	rt.RegisterFunc("__env_kv_list", func(args []any) (any, error) {
		ns, _ := args[0].(string)
		prefix, _ := args[1].(string)
		keys := []any{}
		if m, ok := store[ns]; ok {
			for k := range m {
				if prefix == "" || strings.HasPrefix(k, prefix) {
					keys = append(keys, map[string]any{"name": k})
				}
			}
		}
		return map[string]any{"keys": keys}, nil
	})

	envJSInstall(rt)

	// put + get
	v, err := rt.Eval(`
		var kv = __buildEnvKV("default");
		kv.put("counter", "42");
		kv.get("counter");
	`)
	if err != nil {
		t.Fatalf("KV put/get failed: %v", err)
	}
	result, _ := v.GoString()
	v.Close()
	if result != "42" {
		t.Fatalf("expected '42', got %q", result)
	}

	// get with json type
	v, err = rt.Eval(`
		var kv = __buildEnvKV("default");
		kv.put("obj", { foo: "bar" });
		var r = kv.get("obj", { type: "json" });
		r.foo;
	`)
	if err != nil {
		t.Fatalf("KV json get failed: %v", err)
	}
	result, _ = v.GoString()
	v.Close()
	if result != "bar" {
		t.Fatalf("expected 'bar', got %q", result)
	}

	// delete
	err = rt.Exec(`
		var kv = __buildEnvKV("default");
		kv.delete("counter");
	`)
	if err != nil {
		t.Fatalf("KV delete failed: %v", err)
	}

	v, err = rt.Eval(`__buildEnvKV("default").get("counter")`)
	if err != nil {
		t.Fatal(err)
	}
	if !v.IsNull() && !v.IsUndefined() {
		s, _ := v.GoString()
		v.Close()
		t.Fatalf("expected null after delete, got %q", s)
	}
	v.Close()

	// namespace isolation
	err = rt.Exec(`
		__buildEnvKV("ns1").put("k", "v1");
		__buildEnvKV("ns2").put("k", "v2");
	`)
	if err != nil {
		t.Fatal(err)
	}

	v, err = rt.Eval(`__buildEnvKV("ns1").get("k")`)
	if err != nil {
		t.Fatal(err)
	}
	result, _ = v.GoString()
	v.Close()
	if result != "v1" {
		t.Fatalf("expected 'v1' from ns1, got %q", result)
	}

	v, err = rt.Eval(`__buildEnvKV("ns2").get("k")`)
	if err != nil {
		t.Fatal(err)
	}
	result, _ = v.GoString()
	v.Close()
	if result != "v2" {
		t.Fatalf("expected 'v2' from ns2, got %q", result)
	}

	// namespace() method
	v, err = rt.Eval(`
		var kv = __buildEnvKV("default");
		kv.namespace("ns1").get("k");
	`)
	if err != nil {
		t.Fatal(err)
	}
	result, _ = v.GoString()
	v.Close()
	if result != "v1" {
		t.Fatalf("expected 'v1' from namespace('ns1'), got %q", result)
	}
}

func TestKVCollectionName(t *testing.T) {
	if kvCollectionName("default") != "_kv_default" {
		t.Fatal("unexpected collection name")
	}
	if kvCollectionName("foo") != "_kv_foo" {
		t.Fatal("unexpected collection name")
	}
}
