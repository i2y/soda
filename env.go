package soda

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/i2y/ramune"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/dbx"
)

// kvCollectionCache prevents redundant DB lookups and concurrent
// creation races in ensureKVCollection.
var kvCollectionCache sync.Map // namespace string → *core.Collection

// workersEnvBinds registers all Go-side functions that the JS __buildEnv()
// helper depends on. Called once per executor VM during pool creation.
func workersEnvBinds(app core.App, vm *ramune.Runtime) {
	envDBBinds(app, vm)
	envKVBinds(app, vm)
	envSecretsBinds(vm)
	envJSInstall(vm)
}

// -------------------------------------------------------------------
// env.DB — D1-like SQL API
// -------------------------------------------------------------------

func envDBBinds(app core.App, vm *ramune.Runtime) {
	// __env_db_exec(sql, params, isQuery) — execute a SQL statement.
	//   isQuery=true  → SELECT: returns { results: [...], success: true }
	//   isQuery=false → write:  returns { success: true, meta: { changes, last_row_id } }
	vm.RegisterFunc("__env_db_exec", func(args []any) (any, error) {
		if len(args) < 1 {
			return nil, fmt.Errorf("__env_db_exec: sql argument required")
		}

		sql, _ := args[0].(string)
		if sql == "" {
			return nil, fmt.Errorf("__env_db_exec: sql must be a non-empty string")
		}

		var params []any
		if len(args) > 1 {
			if p, ok := args[1].([]any); ok {
				params = p
			}
		}

		isQuery := true
		if len(args) > 2 {
			if b, ok := args[2].(bool); ok {
				isQuery = b
			}
		}

		// Rewrite ?-style positional params to dbx {:pN} format.
		rewritten, dbxParams := rewritePositionalParams(sql, params)

		if isQuery {
			var rows []map[string]any
			err := app.ConcurrentDB().NewQuery(rewritten).Bind(dbxParams).All(&rows)
			if err != nil {
				return nil, err
			}
			if rows == nil {
				rows = []map[string]any{}
			}
			return map[string]any{
				"results": rows,
				"success": true,
			}, nil
		}

		// Write path.
		result, err := app.NonconcurrentDB().NewQuery(rewritten).Bind(dbxParams).Execute()
		if err != nil {
			return nil, err
		}
		changes, _ := result.RowsAffected()
		lastID, _ := result.LastInsertId()
		return map[string]any{
			"success": true,
			"meta": map[string]any{
				"changes":     float64(changes),
				"last_row_id": float64(lastID),
			},
		}, nil
	})
}

// rewritePositionalParams replaces ? placeholders with {:p0}, {:p1}, …
// and builds the corresponding dbx.Params map.
func rewritePositionalParams(sql string, params []any) (string, dbx.Params) {
	dbxParams := dbx.Params{}
	idx := 0

	var b strings.Builder
	b.Grow(len(sql) + len(params)*4)

	inString := false
	var quote byte

	for i := 0; i < len(sql); i++ {
		ch := sql[i]

		// Track string literals to avoid replacing ? inside them.
		if ch == '\'' || ch == '"' {
			if !inString {
				inString = true
				quote = ch
			} else if ch == quote {
				inString = false
			}
			b.WriteByte(ch)
			continue
		}

		if ch == '?' && !inString && idx < len(params) {
			key := fmt.Sprintf("p%d", idx)
			b.WriteString("{:" + key + "}")
			dbxParams[key] = params[idx]
			idx++
		} else {
			b.WriteByte(ch)
		}
	}

	return b.String(), dbxParams
}

// -------------------------------------------------------------------
// env.KV — key-value store backed by PocketBase collections
// -------------------------------------------------------------------

func envKVBinds(app core.App, vm *ramune.Runtime) {
	// __env_kv_get(namespace, key) → value string or null
	vm.RegisterFunc("__env_kv_get", func(args []any) (any, error) {
		if len(args) < 2 {
			return nil, nil
		}
		ns, _ := args[0].(string)
		key, _ := args[1].(string)
		if ns == "" || key == "" {
			return nil, nil
		}

		collName := kvCollectionName(ns)
		record, err := app.FindFirstRecordByData(collName, "key", key)
		if err != nil {
			return nil, nil // key not found
		}
		return record.GetString("value"), nil
	})

	// __env_kv_put(namespace, key, value)
	vm.RegisterFunc("__env_kv_put", func(args []any) (any, error) {
		if len(args) < 3 {
			return nil, fmt.Errorf("__env_kv_put: requires namespace, key, value")
		}
		ns, _ := args[0].(string)
		key, _ := args[1].(string)
		value, _ := args[2].(string)

		coll, err := ensureKVCollection(app, ns)
		if err != nil {
			return nil, fmt.Errorf("__env_kv_put: %w", err)
		}

		// Try to find existing record.
		record, err := app.FindFirstRecordByData(coll, "key", key)
		if err != nil {
			// Create new.
			record = core.NewRecord(coll)
			record.Set("key", key)
		}
		record.Set("value", value)

		if err := app.Save(record); err != nil {
			return nil, fmt.Errorf("__env_kv_put: %w", err)
		}
		return nil, nil
	})

	// __env_kv_delete(namespace, key)
	vm.RegisterFunc("__env_kv_delete", func(args []any) (any, error) {
		if len(args) < 2 {
			return nil, nil
		}
		ns, _ := args[0].(string)
		key, _ := args[1].(string)

		collName := kvCollectionName(ns)
		record, err := app.FindFirstRecordByData(collName, "key", key)
		if err != nil {
			return nil, nil // key not found, nothing to do
		}
		if err := app.Delete(record); err != nil {
			return nil, fmt.Errorf("__env_kv_delete: %w", err)
		}
		return nil, nil
	})

	// __env_kv_list(namespace, prefix, limit) → { keys: [...] }
	vm.RegisterFunc("__env_kv_list", func(args []any) (any, error) {
		if len(args) < 1 {
			return map[string]any{"keys": []any{}}, nil
		}
		ns, _ := args[0].(string)
		prefix := ""
		if len(args) > 1 {
			prefix, _ = args[1].(string)
		}
		limit := 1000
		if len(args) > 2 {
			if l, ok := toInt64(args[2]); ok && l > 0 {
				limit = int(l)
			}
		}

		collName := kvCollectionName(ns)
		coll, err := app.FindCollectionByNameOrId(collName)
		if err != nil {
			return map[string]any{"keys": []any{}}, nil
		}

		filter := ""
		filterParams := dbx.Params{}
		if prefix != "" {
			filter = "key ~ {:prefix}"
			filterParams["prefix"] = prefix + "%"
		}

		records, err := app.FindRecordsByFilter(coll, filter, "key", limit, 0, filterParams)
		if err != nil {
			return map[string]any{"keys": []any{}}, nil
		}

		keys := make([]any, len(records))
		for i, r := range records {
			keys[i] = map[string]any{
				"name": r.GetString("key"),
			}
		}
		return map[string]any{"keys": keys}, nil
	})
}

func kvCollectionName(namespace string) string {
	return "_kv_" + namespace
}

// ensureKVCollection returns the KV collection for the given namespace,
// creating it if it doesn't exist. Results are cached to avoid
// redundant DB lookups and concurrent creation races.
func ensureKVCollection(app core.App, namespace string) (*core.Collection, error) {
	if cached, ok := kvCollectionCache.Load(namespace); ok {
		return cached.(*core.Collection), nil
	}

	collName := kvCollectionName(namespace)
	coll, err := app.FindCollectionByNameOrId(collName)
	if err == nil {
		kvCollectionCache.Store(namespace, coll)
		return coll, nil
	}

	coll = core.NewBaseCollection(collName)
	coll.Fields.Add(&core.TextField{
		Name:     "key",
		Required: true,
		Max:      512,
	})
	coll.Fields.Add(&core.TextField{
		Name: "value",
	})
	coll.AddIndex("idx_"+collName+"_key", true, "key", "")

	if err := app.Save(coll); err != nil {
		// Another goroutine may have created it; retry lookup.
		if coll2, err2 := app.FindCollectionByNameOrId(collName); err2 == nil {
			kvCollectionCache.Store(namespace, coll2)
			return coll2, nil
		}
		return nil, err
	}
	kvCollectionCache.Store(namespace, coll)
	return coll, nil
}

// -------------------------------------------------------------------
// env.SECRETS — read-only map from POCKETBASE_SECRET_* env vars
// -------------------------------------------------------------------

const secretsPrefix = "POCKETBASE_SECRET_"

func envSecretsBinds(vm *ramune.Runtime) {
	// __env_list_secrets() → { KEY: "value", ... }
	vm.RegisterFunc("__env_list_secrets", func(args []any) (any, error) {
		result := map[string]any{}
		for _, e := range os.Environ() {
			if !strings.HasPrefix(e, secretsPrefix) {
				continue
			}
			parts := strings.SplitN(e, "=", 2)
			key := strings.TrimPrefix(parts[0], secretsPrefix)
			val := ""
			if len(parts) > 1 {
				val = parts[1]
			}
			result[key] = val
		}
		return result, nil
	})
}

// -------------------------------------------------------------------
// JS-side env builders — installed on each executor VM
// -------------------------------------------------------------------

const envJSCode = `
// env.DB — D1-like prepare/bind/all/first/run
globalThis.__buildEnvDB = function() {
	return {
		prepare: function(sql) {
			var _sql = sql;
			var _params = [];
			return {
				bind: function() {
					_params = Array.prototype.slice.call(arguments);
					return this;
				},
				all: function() {
					return __env_db_exec(_sql, _params, true);
				},
				first: function(colName) {
					var r = __env_db_exec(_sql + " LIMIT 1", _params, true);
					var row = r && r.results && r.results[0];
					if (!row) return null;
					return colName ? row[colName] : row;
				},
				run: function() {
					return __env_db_exec(_sql, _params, false);
				}
			};
		},
		exec: function(sql) {
			return __env_db_exec(sql, [], false);
		}
	};
};

// env.KV — Workers KV-like get/put/delete/list
globalThis.__buildEnvKV = function(namespace) {
	var kv = {
		get: function(key, opts) {
			var v = __env_kv_get(namespace, key);
			if (v === null || v === undefined) return null;
			if (opts && opts.type === "json") {
				try { return JSON.parse(v); } catch(e) { return null; }
			}
			return v;
		},
		put: function(key, value) {
			if (typeof value === "object" && value !== null) {
				value = JSON.stringify(value);
			} else {
				value = String(value);
			}
			return __env_kv_put(namespace, key, value);
		},
		delete: function(key) {
			return __env_kv_delete(namespace, key);
		},
		list: function(opts) {
			opts = opts || {};
			return __env_kv_list(namespace, opts.prefix || "", opts.limit || 1000);
		},
		namespace: function(name) {
			return __buildEnvKV(name);
		}
	};
	return kv;
};

// env.SECRETS — frozen read-only map, cached per VM since env vars
// rarely change at runtime.
globalThis.__cachedSecrets = null;
globalThis.__buildEnvSecrets = function() {
	if (!__cachedSecrets) {
		__cachedSecrets = Object.freeze(__env_list_secrets());
	}
	return __cachedSecrets;
};

// __buildEnv — assembles the full env object.
// DB and KV are lightweight stateless facades; SECRETS is cached.
// Named KV bindings declared in soda.toml are layered on via
// __extraEnvBindings (installed at plugin init if any are declared).
globalThis.__buildEnv = function() {
	var env = {
		DB: __buildEnvDB(),
		KV: __buildEnvKV("default"),
		SECRETS: __buildEnvSecrets(),
		APP: $app,
	};
	if (typeof globalThis.__extraEnvBindings === "function") {
		globalThis.__extraEnvBindings(env);
	}
	return env;
};
`

func envJSInstall(vm *ramune.Runtime) {
	if err := vm.Exec(envJSCode); err != nil {
		panic(fmt.Sprintf("[workers] failed to install env JS builders: %v", err))
	}
}

