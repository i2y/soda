// This file is adapted from github.com/pocketbase/pocketbase/plugins/jsvm.
// Copyright (c) 2022 - present, Gani Georgiev. Distributed under the MIT License.
// https://github.com/pocketbase/pocketbase/blob/master/LICENSE.md
//
// Modifications for the Ramune JS engine, Workers-style handlers,
// and related Soda features:
// Copyright (c) 2026 - present, Yasushi Itoh.

package soda

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/golang-jwt/jwt/v5"
	"github.com/i2y/ramune"
	"github.com/pocketbase/dbx"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/forms"
	"github.com/pocketbase/pocketbase/mails"
	"github.com/pocketbase/pocketbase/tools/filesystem"
	"github.com/pocketbase/pocketbase/tools/hook"
	"github.com/pocketbase/pocketbase/tools/inflector"
	"github.com/pocketbase/pocketbase/tools/mailer"
	"github.com/pocketbase/pocketbase/tools/router"
	"github.com/pocketbase/pocketbase/tools/security"
	"github.com/pocketbase/pocketbase/tools/store"
	"github.com/pocketbase/pocketbase/tools/subscriptions"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/spf13/cast"
	"github.com/spf13/cobra"
)

// hooksBinds adds wrapped "on*" hook methods by reflecting on core.App.
func hooksBinds(app core.App, loader *ramune.Runtime, executors *vmsPool, proxy *typeProxy) {
	appType := reflect.TypeOf(app)
	appValue := reflect.ValueOf(app)
	totalMethods := appType.NumMethod()
	excludeHooks := []string{"OnServe"}

	for i := 0; i < totalMethods; i++ {
		method := appType.Method(i)
		if !strings.HasPrefix(method.Name, "On") || slices.Contains(excludeHooks, method.Name) {
			continue // not a hook or excluded
		}

		jsName := convertGoToJSName(method.Name)
		methodName := method.Name

		// register the hook to the loader
		loader.RegisterFunc(jsName, func(args []any) (any, error) {
			if len(args) < 1 {
				return nil, fmt.Errorf("%s requires at least a callback argument", jsName)
			}

			callback, ok := args[0].(string)
			if !ok {
				return nil, fmt.Errorf("%s: callback must be a function", jsName)
			}

			// Extract optional tags (collection names, etc.)
			var tags []string
			for _, a := range args[1:] {
				if s, ok := a.(string); ok {
					tags = append(tags, s)
				}
			}

			tagsAsValues := make([]reflect.Value, len(tags))
			for i, tag := range tags {
				tagsAsValues[i] = reflect.ValueOf(tag)
			}

			hookInstance := appValue.MethodByName(methodName).Call(tagsAsValues)[0]
			hookBindFunc := hookInstance.MethodByName("BindFunc")

			handlerType := hookBindFunc.Type().In(0)

			handler := reflect.MakeFunc(handlerType, func(reflectArgs []reflect.Value) (results []reflect.Value) {
				handlerArgs := make([]any, len(reflectArgs))
				for i, arg := range reflectArgs {
					handlerArgs[i] = arg.Interface()
				}

				err := executors.run(func(executor *ramune.Runtime) error {
					// Register the event object as a proxy
					eventHandle := proxy.registry.register(handlerArgs[0])
					defer proxy.registry.unregister(eventHandle)

					// Determine event type name
					eventTypeName := proxy.resolveTypeName(handlerArgs[0])
					if eventTypeName == "" {
						eventTypeName = "HookEvent"
					}

					// Build and execute the handler
					// The callback is wrapped to:
					// 1. Create a proxy for the event object
					// 2. Set $app to the event's app property
					// 3. Support async handlers via EvalAsync
					code := fmt.Sprintf(
						`(async function() {
							var __e = __proxy_wrap(%q, %d);
							$app = __e.app ? __e.app : $app;
							return await (%s)(__e);
						})()`,
						eventTypeName, eventHandle, callback,
					)

					_, err := executor.EvalAsync(code)
					return err
				})

				return []reflect.Value{reflect.ValueOf(&err).Elem()}
			})

			// register the wrapped hook handler
			hookBindFunc.Call([]reflect.Value{handler})
			return nil, nil
		})
	}
}

func cronBinds(app core.App, loader *ramune.Runtime, executors *vmsPool, proxy *typeProxy) {
	cronAdd := func(args []any) (any, error) {
		if len(args) < 3 {
			return nil, fmt.Errorf("cronAdd requires 3 arguments: jobId, cronExpr, handler")
		}
		jobId, _ := args[0].(string)
		cronExpr, _ := args[1].(string)
		handler, _ := args[2].(string)

		err := app.Cron().Add(jobId, cronExpr, func() {
			err := executors.run(func(executor *ramune.Runtime) error {
				code := fmt.Sprintf(`(async function() { await (%s)(); })()`, handler)
				_, err := executor.EvalAsync(code)
				return err
			})

			if err != nil {
				app.Logger().Error(
					"[cronAdd] failed to execute cron job",
					slog.String("jobId", jobId),
					slog.String("error", err.Error()),
				)
			}
		})
		if err != nil {
			panic("[cronAdd] failed to register cron job " + jobId + ": " + err.Error())
		}
		return nil, nil
	}
	loader.RegisterFunc("cronAdd", cronAdd)

	cronRemove := func(args []any) (any, error) {
		if len(args) < 1 {
			return nil, fmt.Errorf("cronRemove requires jobId argument")
		}
		jobId, _ := args[0].(string)
		app.Cron().Remove(jobId)
		return nil, nil
	}
	loader.RegisterFunc("cronRemove", cronRemove)

	// register the removal helper also in the executors
	oldFactory := executors.factory
	executors.factory = func() *ramune.Runtime {
		vm := oldFactory()
		vm.RegisterFunc("cronAdd", cronAdd)
		vm.RegisterFunc("cronRemove", cronRemove)
		return vm
	}
	executors.forEach(func(vm *ramune.Runtime) {
		vm.RegisterFunc("cronAdd", cronAdd)
		vm.RegisterFunc("cronRemove", cronRemove)
	})
}

func routerBinds(app core.App, loader *ramune.Runtime, executors *vmsPool, proxy *typeProxy) {
	loader.RegisterFunc("routerAdd", func(args []any) (any, error) {
		if len(args) < 3 {
			return nil, fmt.Errorf("routerAdd requires at least 3 arguments: method, path, handler")
		}
		method, _ := args[0].(string)
		path, _ := args[1].(string)
		handlerStr, _ := args[2].(string)

		wrappedMiddlewares, err := wrapMiddlewares(executors, proxy, args[3:])
		if err != nil {
			panic("[routerAdd] failed to wrap middlewares: " + err.Error())
		}

		wrappedHandler := wrapHandlerFunc(executors, proxy, handlerStr)

		app.OnServe().BindFunc(func(e *core.ServeEvent) error {
			e.Router.Route(strings.ToUpper(method), path, wrappedHandler).Bind(wrappedMiddlewares...)
			return e.Next()
		})

		return nil, nil
	})

	loader.RegisterFunc("routerUse", func(args []any) (any, error) {
		wrappedMiddlewares, err := wrapMiddlewares(executors, proxy, args)
		if err != nil {
			panic("[routerUse] failed to wrap middlewares: " + err.Error())
		}

		app.OnServe().BindFunc(func(e *core.ServeEvent) error {
			e.Router.Bind(wrappedMiddlewares...)
			return e.Next()
		})

		return nil, nil
	})
}

func wrapHandlerFunc(executors *vmsPool, proxy *typeProxy, handlerStr string) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		return executors.run(func(executor *ramune.Runtime) error {
			eventHandle := proxy.registry.register(e)
			defer proxy.registry.unregister(eventHandle)

			code := fmt.Sprintf(
				`(async function() {
					var __e = __proxy_wrap("RequestEvent", %d);
					$app = __e.app ? __e.app : $app;
					return await (%s)(__e);
				})()`,
				eventHandle, handlerStr,
			)

			_, err := executor.EvalAsync(code)
			return err
		})
	}
}

type jsHookHandler struct {
	id             string
	serializedFunc string
	priority       int
}

func wrapMiddlewares(executors *vmsPool, proxy *typeProxy, rawMiddlewares []any) ([]*hook.Handler[*core.RequestEvent], error) {
	wrappedMiddlewares := make([]*hook.Handler[*core.RequestEvent], 0, len(rawMiddlewares))

	for _, m := range rawMiddlewares {
		if m == nil {
			continue
		}

		switch v := m.(type) {
		case string:
			// JS function source code
			funcStr := v
			wrappedMiddlewares = append(wrappedMiddlewares, &hook.Handler[*core.RequestEvent]{
				Func: wrapHandlerFunc(executors, proxy, funcStr),
			})
		case map[string]any:
			// Could be a Middleware object: {id, serializedFunc, priority}
			funcStr, _ := v["serializedFunc"].(string)
			if funcStr == "" {
				return nil, errors.New("missing or invalid Middleware function")
			}
			id, _ := v["id"].(string)
			priority := 0
			if p, ok := v["priority"].(float64); ok {
				priority = int(p)
			}
			wrappedMiddlewares = append(wrappedMiddlewares, &hook.Handler[*core.RequestEvent]{
				Id:       id,
				Priority: priority,
				Func:     wrapHandlerFunc(executors, proxy, funcStr),
			})
		default:
			return nil, fmt.Errorf("unsupported middleware type: %T", m)
		}
	}

	return wrappedMiddlewares, nil
}

var cachedArrayOfTypes = store.New[reflect.Type, reflect.Type](nil)

func baseBinds(vm *ramune.Runtime, proxy *typeProxy) {
	// deprecated: use toString
	vm.RegisterFunc("readerToString", func(args []any) (any, error) {
		if len(args) < 1 {
			return "", nil
		}
		r, ok := args[0].(io.Reader)
		if !ok {
			return cast.ToStringE(args[0])
		}
		maxBytes := int64(router.DefaultMaxMemory)
		if len(args) > 1 {
			if mb, ok := toFloat(args[1]); ok && int(mb) > 0 {
				maxBytes = int64(mb)
			}
		}
		bodyBytes, err := io.ReadAll(io.LimitReader(r, maxBytes))
		if err != nil {
			return "", err
		}
		return string(bodyBytes), nil
	})

	vm.RegisterFunc("toBytes", func(args []any) (any, error) {
		if len(args) < 1 {
			return []byte{}, nil
		}
		raw := args[0]
		maxReaderBytes := 0
		if len(args) > 1 {
			if mb, ok := toFloat(args[1]); ok {
				maxReaderBytes = int(mb)
			}
		}

		switch v := raw.(type) {
		case nil:
			return []byte{}, nil
		case string:
			return []byte(v), nil
		case []byte:
			return v, nil
		case io.Reader:
			if maxReaderBytes == 0 {
				maxReaderBytes = int(router.DefaultMaxMemory)
			}
			return io.ReadAll(io.LimitReader(v, int64(maxReaderBytes)))
		default:
			b, err := cast.ToUint8SliceE(v)
			if err == nil {
				return b, nil
			}
			str, err := cast.ToStringE(v)
			if err == nil {
				return []byte(str), nil
			}
			rawBytes, _ := json.Marshal(raw)
			return rawBytes, nil
		}
	})

	vm.RegisterFunc("toString", func(args []any) (any, error) {
		if len(args) < 1 {
			return "", nil
		}
		raw := args[0]
		maxReaderBytes := 0
		if len(args) > 1 {
			if mb, ok := toFloat(args[1]); ok {
				maxReaderBytes = int(mb)
			}
		}

		switch v := raw.(type) {
		case io.Reader:
			if maxReaderBytes == 0 {
				maxReaderBytes = int(router.DefaultMaxMemory)
			}
			bodyBytes, err := io.ReadAll(io.LimitReader(v, int64(maxReaderBytes)))
			if err != nil {
				return "", err
			}
			return string(bodyBytes), nil
		default:
			str, err := cast.ToStringE(v)
			if err == nil {
				return str, nil
			}
			rawBytes, _ := json.Marshal(raw)
			return string(rawBytes), nil
		}
	})

	vm.RegisterFunc("sleep", func(args []any) (any, error) {
		if len(args) < 1 {
			return nil, nil
		}
		ms, _ := toFloat(args[0])
		time.Sleep(time.Duration(int64(ms)) * time.Millisecond)
		return nil, nil
	})

	vm.RegisterFunc("arrayOf", func(args []any) (any, error) {
		if len(args) < 1 {
			return nil, fmt.Errorf("arrayOf requires a model argument")
		}
		model := args[0]
		// If it's a proxy handle, resolve it
		if m, ok := model.(map[string]any); ok {
			if handleRaw, exists := m["__handle"]; exists {
				handle := int64(handleRaw.(float64))
				obj, found := proxy.registry.get(handle)
				if found {
					model = obj
				}
			}
		}
		mt := reflect.TypeOf(model)
		st := cachedArrayOfTypes.GetOrSet(mt, func() reflect.Type {
			return reflect.SliceOf(mt)
		})
		result := reflect.New(st).Elem().Addr().Interface()
		handle := proxy.registry.register(result)
		return map[string]any{"__handle": float64(handle), "__type": "array"}, nil
	})

	vm.RegisterFunc("unmarshal", func(args []any) (any, error) {
		if len(args) < 2 {
			return nil, fmt.Errorf("unmarshal requires data and destination arguments")
		}
		data := args[0]
		dst := args[1]

		// If dst is a proxy handle, resolve it
		if m, ok := dst.(map[string]any); ok {
			if handleRaw, exists := m["__handle"]; exists {
				handle := int64(handleRaw.(float64))
				obj, found := proxy.registry.get(handle)
				if found {
					dst = obj
				}
			}
		}

		raw, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		return nil, json.Unmarshal(raw, &dst)
	})

	vm.RegisterFunc("Context", func(args []any) (any, error) {
		var instance context.Context

		if len(args) > 0 {
			if m, ok := args[0].(map[string]any); ok {
				if handleRaw, exists := m["__handle"]; exists {
					handle := int64(handleRaw.(float64))
					if obj, found := proxy.registry.get(handle); found {
						if ctx, ok := obj.(context.Context); ok {
							instance = ctx
						}
					}
				}
			}
		}
		if instance == nil {
			instance = context.Background()
		}

		if len(args) > 1 && args[1] != nil {
			key := args[1]
			var val any
			if len(args) > 2 {
				val = args[2]
			}
			instance = context.WithValue(instance, key, val)
		}

		handle := proxy.registry.register(instance)
		return map[string]any{"__handle": float64(handle), "__type": "Context"}, nil
	})

	vm.RegisterFunc("DynamicModel", func(args []any) (any, error) {
		if len(args) < 1 {
			return nil, fmt.Errorf("[DynamicModel] missing shape data")
		}
		shape, ok := args[0].(map[string]any)
		if !ok || len(shape) == 0 {
			return nil, fmt.Errorf("[DynamicModel] missing shape data")
		}
		instance := newDynamicModel(shape)
		handle := proxy.registry.register(instance)
		return map[string]any{"__handle": float64(handle), "__type": "DynamicModel"}, nil
	})

	// nullable helpers
	vm.RegisterFunc("nullString", func(args []any) (any, error) {
		var v string
		handle := proxy.registry.register(&v)
		return map[string]any{"__handle": float64(handle), "__type": "nullString"}, nil
	})
	vm.RegisterFunc("nullFloat", func(args []any) (any, error) {
		var v float64
		handle := proxy.registry.register(&v)
		return map[string]any{"__handle": float64(handle), "__type": "nullFloat"}, nil
	})
	vm.RegisterFunc("nullInt", func(args []any) (any, error) {
		var v int64
		handle := proxy.registry.register(&v)
		return map[string]any{"__handle": float64(handle), "__type": "nullInt"}, nil
	})
	vm.RegisterFunc("nullBool", func(args []any) (any, error) {
		var v bool
		handle := proxy.registry.register(&v)
		return map[string]any{"__handle": float64(handle), "__type": "nullBool"}, nil
	})
	vm.RegisterFunc("nullArray", func(args []any) (any, error) {
		v := types.JSONArray[any]{}
		handle := proxy.registry.register(&v)
		return map[string]any{"__handle": float64(handle), "__type": "nullArray"}, nil
	})
	vm.RegisterFunc("nullObject", func(args []any) (any, error) {
		v := types.JSONMap[any]{}
		handle := proxy.registry.register(&v)
		return map[string]any{"__handle": float64(handle), "__type": "nullObject"}, nil
	})

	// Record constructor
	vm.RegisterFunc("Record", func(args []any) (any, error) {
		var instance *core.Record

		if len(args) > 0 && args[0] != nil {
			// Try to resolve as proxy handle (collection)
			if m, ok := args[0].(map[string]any); ok {
				if handleRaw, exists := m["__handle"]; exists {
					handle := int64(handleRaw.(float64))
					if obj, found := proxy.registry.get(handle); found {
						if collection, ok := obj.(*core.Collection); ok {
							instance = core.NewRecord(collection)
							// Load data from second argument if present
							if len(args) > 1 {
								if data, ok := args[1].(map[string]any); ok {
									instance.Load(data)
								}
							}
						}
					}
				}
			}
		}

		if instance == nil {
			instance = &core.Record{}
		}

		handle := proxy.registry.register(instance)
		return map[string]any{"__handle": float64(handle), "__type": "Record"}, nil
	})

	// Collection constructor
	vm.RegisterFunc("Collection", func(args []any) (any, error) {
		instance := &core.Collection{}
		if len(args) > 0 {
			if data := args[0]; data != nil {
				raw, _ := json.Marshal(data)
				json.Unmarshal(raw, instance)
			}
		}
		handle := proxy.registry.register(instance)
		return map[string]any{"__handle": float64(handle), "__type": "Collection"}, nil
	})

	// FieldsList constructor
	vm.RegisterFunc("FieldsList", func(args []any) (any, error) {
		instance := &core.FieldsList{}
		if len(args) > 0 && args[0] != nil {
			raw, _ := json.Marshal(args[0])
			json.Unmarshal(raw, instance)
		}
		handle := proxy.registry.register(instance)
		return map[string]any{"__handle": float64(handle), "__type": "FieldsList"}, nil
	})

	// Field constructors
	registerFieldConstructor(vm, proxy, "Field", func(args []any) any {
		if len(args) == 0 || args[0] == nil {
			return nil
		}
		data, ok := args[0].(map[string]any)
		if !ok {
			return nil
		}
		rawDataSlice, _ := json.Marshal([]any{data})
		fieldsList := core.NewFieldsList()
		_ = fieldsList.UnmarshalJSON(rawDataSlice)
		if len(fieldsList) == 0 {
			return nil
		}
		return fieldsList[0]
	})
	registerStructConstructor(vm, proxy, "NumberField", func() any { return &core.NumberField{} })
	registerStructConstructor(vm, proxy, "BoolField", func() any { return &core.BoolField{} })
	registerStructConstructor(vm, proxy, "TextField", func() any { return &core.TextField{} })
	registerStructConstructor(vm, proxy, "URLField", func() any { return &core.URLField{} })
	registerStructConstructor(vm, proxy, "EmailField", func() any { return &core.EmailField{} })
	registerStructConstructor(vm, proxy, "EditorField", func() any { return &core.EditorField{} })
	registerStructConstructor(vm, proxy, "PasswordField", func() any { return &core.PasswordField{} })
	registerStructConstructor(vm, proxy, "DateField", func() any { return &core.DateField{} })
	registerStructConstructor(vm, proxy, "AutodateField", func() any { return &core.AutodateField{} })
	registerStructConstructor(vm, proxy, "JSONField", func() any { return &core.JSONField{} })
	registerStructConstructor(vm, proxy, "RelationField", func() any { return &core.RelationField{} })
	registerStructConstructor(vm, proxy, "SelectField", func() any { return &core.SelectField{} })
	registerStructConstructor(vm, proxy, "FileField", func() any { return &core.FileField{} })
	registerStructConstructor(vm, proxy, "GeoPointField", func() any { return &core.GeoPointField{} })

	// Other constructors
	registerStructConstructor(vm, proxy, "MailerMessage", func() any { return &mailer.Message{} })
	registerStructConstructor(vm, proxy, "Command", func() any { return &cobra.Command{} })
	registerStructConstructor(vm, proxy, "RequestInfo", func() any {
		return &core.RequestInfo{Context: core.RequestInfoContextDefault}
	})
	registerStructConstructor(vm, proxy, "Cookie", func() any { return &http.Cookie{} })
	registerStructConstructor(vm, proxy, "SubscriptionMessage", func() any { return &subscriptions.Message{} })

	// Middleware constructor
	vm.RegisterFunc("Middleware", func(args []any) (any, error) {
		instance := map[string]any{}
		if len(args) > 0 {
			instance["serializedFunc"] = cast.ToString(args[0])
		}
		if len(args) > 1 {
			instance["priority"] = args[1]
		}
		if len(args) > 2 {
			instance["id"] = cast.ToString(args[2])
		}
		return instance, nil
	})

	// Timezone constructor
	vm.RegisterFunc("Timezone", func(args []any) (any, error) {
		name := ""
		if len(args) > 0 {
			name, _ = args[0].(string)
		}
		instance, err := time.LoadLocation(name)
		if err != nil {
			instance = time.UTC
		}
		handle := proxy.registry.register(instance)
		return map[string]any{"__handle": float64(handle), "__type": "Timezone"}, nil
	})

	// DateTime constructor
	vm.RegisterFunc("DateTime", func(args []any) (any, error) {
		instance := types.NowDateTime()

		rawDate := ""
		locName := ""
		if len(args) > 0 {
			rawDate, _ = args[0].(string)
		}
		if len(args) > 1 {
			locName, _ = args[1].(string)
		}

		if rawDate != "" && locName != "" {
			loc, err := time.LoadLocation(locName)
			if err != nil {
				loc = time.UTC
			}
			instance, _ = types.ParseDateTime(cast.ToTimeInDefaultLocation(rawDate, loc))
		} else if rawDate != "" {
			instance, _ = types.ParseDateTime(rawDate)
		}

		handle := proxy.registry.register(instance)
		return map[string]any{"__handle": float64(handle), "__type": "DateTime"}, nil
	})

	// ValidationError constructor
	vm.RegisterFunc("ValidationError", func(args []any) (any, error) {
		code := ""
		message := ""
		if len(args) > 0 {
			code, _ = args[0].(string)
		}
		if len(args) > 1 {
			message, _ = args[1].(string)
		}
		instance := validation.NewError(code, message)
		handle := proxy.registry.register(instance)
		return map[string]any{"__handle": float64(handle), "__type": "ValidationError"}, nil
	})
}

// registerStructConstructor creates a constructor function that instantiates a struct,
// optionally unmarshals the first argument into it, and returns a proxy handle.
func registerStructConstructor(vm *ramune.Runtime, proxy *typeProxy, name string, factory func() any) {
	vm.RegisterFunc(name, func(args []any) (any, error) {
		instance := factory()
		if len(args) > 0 && args[0] != nil {
			if raw, err := json.Marshal(args[0]); err == nil {
				json.Unmarshal(raw, instance)
			}
		}
		handle := proxy.registry.register(instance)
		return map[string]any{"__handle": float64(handle), "__type": name}, nil
	})
}

// registerFieldConstructor creates a constructor with a custom factory.
func registerFieldConstructor(vm *ramune.Runtime, proxy *typeProxy, name string, factory func(args []any) any) {
	vm.RegisterFunc(name, func(args []any) (any, error) {
		instance := factory(args)
		if instance == nil {
			return nil, nil
		}
		handle := proxy.registry.register(instance)
		return map[string]any{"__handle": float64(handle), "__type": name}, nil
	})
}

func dbxBinds(vm *ramune.Runtime) {
	vm.RegisterFunc("__dbx_exp", func(args []any) (any, error) {
		if len(args) < 1 {
			return nil, fmt.Errorf("$dbx.exp requires expression string")
		}
		expr, _ := args[0].(string)
		var params map[string]any
		if len(args) > 1 {
			params, _ = args[1].(map[string]any)
		}
		// Convert params to dbx.Params
		dbxParams := dbx.Params{}
		for k, v := range params {
			dbxParams[k] = v
		}
		return dbx.NewExp(expr, dbxParams), nil
	})

	vm.RegisterFunc("__dbx_hashExp", func(args []any) (any, error) {
		if len(args) < 1 {
			return nil, nil
		}
		data, _ := args[0].(map[string]any)
		return dbx.HashExp(data), nil
	})

	// Register simple expression builder functions
	for _, fn := range []struct {
		name string
		fn   func(...any) dbx.Expression
	}{
		{"not", func(args ...any) dbx.Expression { return dbx.Not(args[0].(dbx.Expression)) }},
		{"and", func(args ...any) dbx.Expression {
			exprs := make([]dbx.Expression, len(args))
			for i, a := range args {
				exprs[i] = a.(dbx.Expression)
			}
			return dbx.And(exprs...)
		}},
		{"or", func(args ...any) dbx.Expression {
			exprs := make([]dbx.Expression, len(args))
			for i, a := range args {
				exprs[i] = a.(dbx.Expression)
			}
			return dbx.Or(exprs...)
		}},
	} {
		fnCopy := fn
		vm.RegisterFunc("__dbx_"+fnCopy.name, func(args []any) (any, error) {
			return fnCopy.fn(args...), nil
		})
	}

	// in, notIn: col IN (values...)
	vm.RegisterFunc("__dbx_in", func(args []any) (any, error) {
		if len(args) < 2 {
			return nil, fmt.Errorf("$dbx.in requires col and values")
		}
		col := cast.ToString(args[0])
		return dbx.In(col, args[1:]...), nil
	})
	vm.RegisterFunc("__dbx_notIn", func(args []any) (any, error) {
		if len(args) < 2 {
			return nil, fmt.Errorf("$dbx.notIn requires col and values")
		}
		col := cast.ToString(args[0])
		return dbx.NotIn(col, args[1:]...), nil
	})

	// like, orLike, notLike, orNotLike
	// like, orLike, notLike, orNotLike — table-driven
	for _, lf := range []struct {
		name string
		fn   func(string, ...string) *dbx.LikeExp
	}{
		{"like", dbx.Like},
		{"orLike", dbx.OrLike},
		{"notLike", dbx.NotLike},
		{"orNotLike", dbx.OrNotLike},
	} {
		lfCopy := lf
		vm.RegisterFunc("__dbx_"+lfCopy.name, func(args []any) (any, error) {
			if len(args) < 2 {
				return nil, fmt.Errorf("$dbx.%s requires col and values", lfCopy.name)
			}
			col := cast.ToString(args[0])
			vals := make([]string, len(args)-1)
			for i, a := range args[1:] {
				vals[i] = cast.ToString(a)
			}
			return lfCopy.fn(col, vals...), nil
		})
	}

	// exists, notExists
	vm.RegisterFunc("__dbx_exists", func(args []any) (any, error) {
		if len(args) < 1 {
			return nil, fmt.Errorf("$dbx.exists requires an expression")
		}
		return dbx.Exists(dbx.NewExp(cast.ToString(args[0]))), nil
	})
	vm.RegisterFunc("__dbx_notExists", func(args []any) (any, error) {
		if len(args) < 1 {
			return nil, fmt.Errorf("$dbx.notExists requires an expression")
		}
		return dbx.NotExists(dbx.NewExp(cast.ToString(args[0]))), nil
	})

	// between, notBetween
	vm.RegisterFunc("__dbx_between", func(args []any) (any, error) {
		if len(args) < 3 {
			return nil, fmt.Errorf("$dbx.between requires col, from, and to")
		}
		return dbx.Between(cast.ToString(args[0]), args[1], args[2]), nil
	})
	vm.RegisterFunc("__dbx_notBetween", func(args []any) (any, error) {
		if len(args) < 3 {
			return nil, fmt.Errorf("$dbx.notBetween requires col, from, and to")
		}
		return dbx.NotBetween(cast.ToString(args[0]), args[1], args[2]), nil
	})

	// Install JS namespace
	vm.Exec(`globalThis.$dbx = {
		exp: __dbx_exp,
		hashExp: __dbx_hashExp,
		not: __dbx_not,
		and: __dbx_and,
		or: __dbx_or,
		"in": __dbx_in,
		notIn: __dbx_notIn,
		like: __dbx_like,
		orLike: __dbx_orLike,
		notLike: __dbx_notLike,
		orNotLike: __dbx_orNotLike,
		exists: __dbx_exists,
		notExists: __dbx_notExists,
		between: __dbx_between,
		notBetween: __dbx_notBetween,
	};`)
}

func mailsBinds(vm *ramune.Runtime, proxy *typeProxy) {
	registerProxiedFunc(vm, proxy, "__mails_sendRecordPasswordReset", mails.SendRecordPasswordReset)
	registerProxiedFunc(vm, proxy, "__mails_sendRecordVerification", mails.SendRecordVerification)
	registerProxiedFunc(vm, proxy, "__mails_sendRecordChangeEmail", mails.SendRecordChangeEmail)
	registerProxiedFunc(vm, proxy, "__mails_sendRecordOTP", mails.SendRecordOTP)
	registerProxiedFunc(vm, proxy, "__mails_sendRecordAuthAlert", mails.SendRecordAuthAlert)

	vm.Exec(`globalThis.$mails = {
		sendRecordPasswordReset: __mails_sendRecordPasswordReset,
		sendRecordVerification: __mails_sendRecordVerification,
		sendRecordChangeEmail: __mails_sendRecordChangeEmail,
		sendRecordOTP: __mails_sendRecordOTP,
		sendRecordAuthAlert: __mails_sendRecordAuthAlert,
	};`)
}

// registerProxiedFunc registers a Go function that accepts proxy-handled arguments.
func registerProxiedFunc(vm *ramune.Runtime, proxy *typeProxy, name string, fn any) {
	fnVal := reflect.ValueOf(fn)
	fnType := fnVal.Type()

	vm.RegisterFunc(name, func(args []any) (any, error) {
		numIn := fnType.NumIn()
		in := make([]reflect.Value, numIn)
		for i := 0; i < numIn; i++ {
			if i < len(args) {
				converted, err := proxy.jsToGo(args[i], fnType.In(i))
				if err != nil {
					return nil, fmt.Errorf("%s arg %d: %w", name, i, err)
				}
				in[i] = converted
			} else {
				in[i] = reflect.Zero(fnType.In(i))
			}
		}

		out := fnVal.Call(in)
		return proxy.processReturnValues(fnType, out)
	})
}

func securityBinds(vm *ramune.Runtime) {
	// crypto
	ramune.Register(vm, "__security_md5", security.MD5)
	ramune.Register(vm, "__security_sha256", security.SHA256)
	ramune.Register(vm, "__security_sha512", security.SHA512)
	ramune.Register(vm, "__security_hs256", security.HS256)
	ramune.Register(vm, "__security_hs512", security.HS512)
	ramune.Register(vm, "__security_equal", security.Equal)

	// random
	ramune.Register(vm, "__security_randomString", security.RandomString)
	ramune.Register(vm, "__security_randomStringByRegex", security.RandomStringByRegex)
	ramune.Register(vm, "__security_randomStringWithAlphabet", security.RandomStringWithAlphabet)
	ramune.Register(vm, "__security_pseudorandomString", security.PseudorandomString)
	ramune.Register(vm, "__security_pseudorandomStringWithAlphabet", security.PseudorandomStringWithAlphabet)

	// jwt
	vm.RegisterFunc("__security_parseUnverifiedJWT", func(args []any) (any, error) {
		if len(args) < 1 {
			return nil, fmt.Errorf("parseUnverifiedJWT requires a token string")
		}
		token, _ := args[0].(string)
		return security.ParseUnverifiedJWT(token)
	})
	vm.RegisterFunc("__security_parseJWT", func(args []any) (any, error) {
		if len(args) < 2 {
			return nil, fmt.Errorf("parseJWT requires token and verificationKey")
		}
		token, _ := args[0].(string)
		key, _ := args[1].(string)
		return security.ParseJWT(token, key)
	})
	vm.RegisterFunc("__security_createJWT", func(args []any) (any, error) {
		if len(args) < 3 {
			return nil, fmt.Errorf("createJWT requires payload, signingKey, and secDuration")
		}
		payload, _ := args[0].(map[string]any)
		signingKey, _ := args[1].(string)
		secDuration, _ := toFloat(args[2])
		claims := jwt.MapClaims{}
		for k, v := range payload {
			claims[k] = v
		}
		return security.NewJWT(claims, signingKey, time.Duration(int(secDuration))*time.Second)
	})

	// encryption
	vm.RegisterFunc("__security_encrypt", func(args []any) (any, error) {
		if len(args) < 2 {
			return nil, fmt.Errorf("encrypt requires data and key")
		}
		data := []byte(cast.ToString(args[0]))
		key, _ := args[1].(string)
		return security.Encrypt(data, key)
	})
	vm.RegisterFunc("__security_decrypt", func(args []any) (any, error) {
		if len(args) < 2 {
			return nil, fmt.Errorf("decrypt requires cipherText and key")
		}
		cipherText, _ := args[0].(string)
		key, _ := args[1].(string)
		result, err := security.Decrypt(cipherText, key)
		if err != nil {
			return "", err
		}
		return string(result), nil
	})

	vm.Exec(`globalThis.$security = {
		md5: __security_md5,
		sha256: __security_sha256,
		sha512: __security_sha512,
		hs256: __security_hs256,
		hs512: __security_hs512,
		equal: __security_equal,
		randomString: __security_randomString,
		randomStringByRegex: __security_randomStringByRegex,
		randomStringWithAlphabet: __security_randomStringWithAlphabet,
		pseudorandomString: __security_pseudorandomString,
		pseudorandomStringWithAlphabet: __security_pseudorandomStringWithAlphabet,
		parseUnverifiedJWT: __security_parseUnverifiedJWT,
		parseJWT: __security_parseJWT,
		createJWT: __security_createJWT,
		encrypt: __security_encrypt,
		decrypt: __security_decrypt,
	};`)
}

func filesystemBinds(vm *ramune.Runtime, proxy *typeProxy) {
	registerProxiedFunc(vm, proxy, "__fs_s3", filesystem.NewS3)
	registerProxiedFunc(vm, proxy, "__fs_local", filesystem.NewLocal)
	ramune.Register(vm, "__fs_fileFromPath", filesystem.NewFileFromPath)
	registerProxiedFunc(vm, proxy, "__fs_fileFromBytes", filesystem.NewFileFromBytes)
	registerProxiedFunc(vm, proxy, "__fs_fileFromMultipart", filesystem.NewFileFromMultipart)

	vm.RegisterFunc("__fs_fileFromURL", func(args []any) (any, error) {
		if len(args) < 1 {
			return nil, fmt.Errorf("fileFromURL requires url argument")
		}
		url, _ := args[0].(string)
		secTimeout := 120
		if len(args) > 1 {
			if t, ok := toFloat(args[1]); ok && int(t) > 0 {
				secTimeout = int(t)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(secTimeout)*time.Second)
		defer cancel()
		file, err := filesystem.NewFileFromURL(ctx, url)
		if err != nil {
			return nil, err
		}
		handle := proxy.registry.register(file)
		return map[string]any{"__handle": float64(handle), "__type": "File"}, nil
	})

	vm.Exec(`globalThis.$filesystem = {
		s3: __fs_s3,
		local: __fs_local,
		fileFromPath: __fs_fileFromPath,
		fileFromBytes: __fs_fileFromBytes,
		fileFromMultipart: __fs_fileFromMultipart,
		fileFromURL: __fs_fileFromURL,
	};`)
}

func filepathBinds(vm *ramune.Runtime) {
	ramune.Register(vm, "__fp_base", filepath.Base)
	ramune.Register(vm, "__fp_clean", filepath.Clean)
	ramune.Register(vm, "__fp_dir", filepath.Dir)
	ramune.Register(vm, "__fp_ext", filepath.Ext)
	ramune.Register(vm, "__fp_fromSlash", filepath.FromSlash)
	ramune.Register(vm, "__fp_isAbs", filepath.IsAbs)
	ramune.Register(vm, "__fp_toSlash", filepath.ToSlash)

	// Multi-arg functions
	vm.RegisterFunc("__fp_join", func(args []any) (any, error) {
		parts := make([]string, len(args))
		for i, a := range args {
			parts[i] = cast.ToString(a)
		}
		return filepath.Join(parts...), nil
	})
	vm.RegisterFunc("__fp_rel", func(args []any) (any, error) {
		if len(args) < 2 {
			return nil, fmt.Errorf("rel requires basepath and targpath")
		}
		return filepath.Rel(cast.ToString(args[0]), cast.ToString(args[1]))
	})
	vm.RegisterFunc("__fp_match", func(args []any) (any, error) {
		if len(args) < 2 {
			return nil, fmt.Errorf("match requires pattern and name")
		}
		return filepath.Match(cast.ToString(args[0]), cast.ToString(args[1]))
	})
	vm.RegisterFunc("__fp_glob", func(args []any) (any, error) {
		if len(args) < 1 {
			return nil, fmt.Errorf("glob requires a pattern")
		}
		return filepath.Glob(cast.ToString(args[0]))
	})
	vm.RegisterFunc("__fp_split", func(args []any) (any, error) {
		if len(args) < 1 {
			return nil, nil
		}
		dir, file := filepath.Split(cast.ToString(args[0]))
		return []any{dir, file}, nil
	})
	vm.RegisterFunc("__fp_splitList", func(args []any) (any, error) {
		if len(args) < 1 {
			return nil, nil
		}
		result := filepath.SplitList(cast.ToString(args[0]))
		anyResult := make([]any, len(result))
		for i, s := range result {
			anyResult[i] = s
		}
		return anyResult, nil
	})

	vm.Exec(`globalThis.$filepath = {
		base: __fp_base,
		clean: __fp_clean,
		dir: __fp_dir,
		ext: __fp_ext,
		fromSlash: __fp_fromSlash,
		glob: __fp_glob,
		isAbs: __fp_isAbs,
		join: __fp_join,
		match: __fp_match,
		rel: __fp_rel,
		split: __fp_split,
		splitList: __fp_splitList,
		toSlash: __fp_toSlash,
	};`)
}

func osBinds(vm *ramune.Runtime, proxy *typeProxy) {
	vm.RegisterFunc("__os_cmd", func(args []any) (any, error) {
		if len(args) < 1 {
			return nil, fmt.Errorf("cmd requires at least a command name")
		}
		name := cast.ToString(args[0])
		cmdArgs := make([]string, 0, len(args)-1)
		for _, a := range args[1:] {
			cmdArgs = append(cmdArgs, cast.ToString(a))
		}
		cmd := exec.Command(name, cmdArgs...)
		handle := proxy.registry.register(cmd)
		return map[string]any{"__handle": float64(handle), "__type": "Command"}, nil
	})
	ramune.Register(vm, "__os_exit", os.Exit)
	ramune.Register(vm, "__os_getenv", os.Getenv)
	ramune.Register(vm, "__os_tempDir", os.TempDir)
	ramune.Register(vm, "__os_getwd", os.Getwd)

	vm.RegisterFunc("__os_readFile", func(args []any) (any, error) {
		if len(args) < 1 {
			return nil, fmt.Errorf("readFile requires a path")
		}
		data, err := os.ReadFile(cast.ToString(args[0]))
		if err != nil {
			return nil, err
		}
		return string(data), nil
	})
	vm.RegisterFunc("__os_writeFile", func(args []any) (any, error) {
		if len(args) < 3 {
			return nil, fmt.Errorf("writeFile requires path, data, and perm")
		}
		path := cast.ToString(args[0])
		data := []byte(cast.ToString(args[1]))
		perm := os.FileMode(cast.ToInt(args[2]))
		return nil, os.WriteFile(path, data, perm)
	})
	vm.RegisterFunc("__os_mkdir", func(args []any) (any, error) {
		if len(args) < 2 {
			return nil, fmt.Errorf("mkdir requires path and perm")
		}
		return nil, os.Mkdir(cast.ToString(args[0]), os.FileMode(cast.ToInt(args[1])))
	})
	vm.RegisterFunc("__os_mkdirAll", func(args []any) (any, error) {
		if len(args) < 2 {
			return nil, fmt.Errorf("mkdirAll requires path and perm")
		}
		return nil, os.MkdirAll(cast.ToString(args[0]), os.FileMode(cast.ToInt(args[1])))
	})
	ramune.Register(vm, "__os_rename", os.Rename)
	ramune.Register(vm, "__os_remove", os.Remove)
	ramune.Register(vm, "__os_removeAll", os.RemoveAll)

	vm.RegisterFunc("__os_stat", func(args []any) (any, error) {
		if len(args) < 1 {
			return nil, fmt.Errorf("stat requires a path")
		}
		info, err := os.Stat(cast.ToString(args[0]))
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"name":    info.Name(),
			"size":    float64(info.Size()),
			"mode":    float64(info.Mode()),
			"isDir":   info.IsDir(),
			"modTime": info.ModTime().Format(time.RFC3339),
		}, nil
	})
	vm.RegisterFunc("__os_readDir", func(args []any) (any, error) {
		if len(args) < 1 {
			return nil, fmt.Errorf("readDir requires a path")
		}
		entries, err := os.ReadDir(cast.ToString(args[0]))
		if err != nil {
			return nil, err
		}
		result := make([]any, len(entries))
		for i, entry := range entries {
			info, _ := entry.Info()
			size := float64(0)
			if info != nil {
				size = float64(info.Size())
			}
			result[i] = map[string]any{
				"name":  entry.Name(),
				"isDir": entry.IsDir(),
				"size":  size,
			}
		}
		return result, nil
	})
	vm.RegisterFunc("__os_truncate", func(args []any) (any, error) {
		if len(args) < 2 {
			return nil, fmt.Errorf("truncate requires path and size")
		}
		size, _ := toFloat(args[1])
		return nil, os.Truncate(cast.ToString(args[0]), int64(size))
	})

	vm.Exec(`globalThis.$os = {
		args: [],
		exec: __os_cmd,
		cmd: __os_cmd,
		exit: __os_exit,
		getenv: __os_getenv,
		stat: __os_stat,
		readFile: __os_readFile,
		writeFile: __os_writeFile,
		readDir: __os_readDir,
		tempDir: __os_tempDir,
		truncate: __os_truncate,
		getwd: __os_getwd,
		mkdir: __os_mkdir,
		mkdirAll: __os_mkdirAll,
		rename: __os_rename,
		remove: __os_remove,
		removeAll: __os_removeAll,
	};`)
}

func formsBinds(vm *ramune.Runtime, proxy *typeProxy) {
	registerFactoryAsConstructorRamune(vm, proxy, "AppleClientSecretCreateForm", forms.NewAppleClientSecretCreate)
	registerFactoryAsConstructorRamune(vm, proxy, "RecordUpsertForm", forms.NewRecordUpsert)
	registerFactoryAsConstructorRamune(vm, proxy, "TestEmailSendForm", forms.NewTestEmailSend)
	registerFactoryAsConstructorRamune(vm, proxy, "TestS3FilesystemForm", forms.NewTestS3Filesystem)
}

func apisBinds(vm *ramune.Runtime, proxy *typeProxy) {
	vm.RegisterFunc("__apis_static", func(args []any) (any, error) {
		if len(args) < 1 {
			return nil, fmt.Errorf("static requires dir argument")
		}
		dir := cast.ToString(args[0])
		indexFallback := false
		if len(args) > 1 {
			indexFallback = cast.ToBool(args[1])
		}
		fn := apis.Static(os.DirFS(dir), indexFallback)
		handle := proxy.registry.register(fn)
		return map[string]any{"__handle": float64(handle), "__type": "HandlerFunc"}, nil
	})

	// Middlewares - these return Go handler functions that work with the proxy system
	registerProxiedFunc(vm, proxy, "__apis_requireGuestOnly", apis.RequireGuestOnly)
	registerProxiedFunc(vm, proxy, "__apis_requireAuth", apis.RequireAuth)
	registerProxiedFunc(vm, proxy, "__apis_requireSuperuserAuth", apis.RequireSuperuserAuth)
	registerProxiedFunc(vm, proxy, "__apis_requireSuperuserOrOwnerAuth", apis.RequireSuperuserOrOwnerAuth)
	registerProxiedFunc(vm, proxy, "__apis_skipSuccessActivityLog", apis.SkipSuccessActivityLog)
	registerProxiedFunc(vm, proxy, "__apis_gzip", apis.Gzip)
	registerProxiedFunc(vm, proxy, "__apis_bodyLimit", apis.BodyLimit)

	// Record helpers
	registerProxiedFunc(vm, proxy, "__apis_recordAuthResponse", apis.RecordAuthResponse)
	registerProxiedFunc(vm, proxy, "__apis_enrichRecord", apis.EnrichRecord)
	registerProxiedFunc(vm, proxy, "__apis_enrichRecords", apis.EnrichRecords)

	// API error constructors
	registerFactoryAsConstructorRamune(vm, proxy, "ApiError", router.NewApiError)
	registerFactoryAsConstructorRamune(vm, proxy, "NotFoundError", router.NewNotFoundError)
	registerFactoryAsConstructorRamune(vm, proxy, "BadRequestError", router.NewBadRequestError)
	registerFactoryAsConstructorRamune(vm, proxy, "ForbiddenError", router.NewForbiddenError)
	registerFactoryAsConstructorRamune(vm, proxy, "UnauthorizedError", router.NewUnauthorizedError)
	registerFactoryAsConstructorRamune(vm, proxy, "TooManyRequestsError", router.NewTooManyRequestsError)
	registerFactoryAsConstructorRamune(vm, proxy, "InternalServerError", router.NewInternalServerError)

	vm.Exec(`globalThis.$apis = {
		static: __apis_static,
		requireGuestOnly: __apis_requireGuestOnly,
		requireAuth: __apis_requireAuth,
		requireSuperuserAuth: __apis_requireSuperuserAuth,
		requireSuperuserOrOwnerAuth: __apis_requireSuperuserOrOwnerAuth,
		skipSuccessActivityLog: __apis_skipSuccessActivityLog,
		gzip: __apis_gzip,
		bodyLimit: __apis_bodyLimit,
		recordAuthResponse: __apis_recordAuthResponse,
		enrichRecord: __apis_enrichRecord,
		enrichRecords: __apis_enrichRecords,
	};`)
}

func httpClientBinds(vm *ramune.Runtime, proxy *typeProxy) {
	// FormData constructor
	vm.RegisterFunc("FormData", func(args []any) (any, error) {
		instance := FormData{}
		handle := proxy.registry.register(instance)
		return map[string]any{"__handle": float64(handle), "__type": "FormData"}, nil
	})

	vm.RegisterFunc("__http_send", func(args []any) (any, error) {
		if len(args) < 1 {
			return nil, fmt.Errorf("$http.send requires a config object")
		}
		params, ok := args[0].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("$http.send config must be an object")
		}

		method := cast.ToString(params["method"])
		if method == "" {
			method = "GET"
		}
		url := cast.ToString(params["url"])
		timeout := cast.ToInt(params["timeout"])
		if timeout <= 0 {
			timeout = 120
		}
		headers := cast.ToStringMapString(params["headers"])

		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
		defer cancel()

		var reqBody io.Reader
		var contentType string

		// legacy json body data
		if data := cast.ToStringMap(params["data"]); len(data) != 0 {
			encoded, err := json.Marshal(data)
			if err != nil {
				return nil, err
			}
			reqBody = bytes.NewReader(encoded)
		} else if rawBody := params["body"]; rawBody != nil {
			// Check if it's a FormData proxy handle
			if m, ok := rawBody.(map[string]any); ok {
				if handleRaw, exists := m["__handle"]; exists {
					handle := int64(handleRaw.(float64))
					if obj, found := proxy.registry.get(handle); found {
						if fd, ok := obj.(FormData); ok {
							body, mp, err := fd.toMultipart()
							if err != nil {
								return nil, err
							}
							reqBody = body
							contentType = mp.FormDataContentType()
						}
					}
				}
			}
			if reqBody == nil {
				reqBody = strings.NewReader(cast.ToString(rawBody))
			}
		}

		req, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), url, reqBody)
		if err != nil {
			return nil, err
		}

		for k, v := range headers {
			req.Header.Add(k, v)
		}
		if contentType != "" {
			req.Header.Set("content-type", contentType)
		}

		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()

		bodyRaw, _ := io.ReadAll(res.Body)

		result := map[string]any{
			"statusCode": float64(res.StatusCode),
			"headers":    map[string]any{},
			"cookies":    map[string]any{},
			"raw":        string(bodyRaw),
			"body":       string(bodyRaw),
		}

		headersMap := result["headers"].(map[string]any)
		for k, v := range res.Header {
			anyV := make([]any, len(v))
			for i, s := range v {
				anyV[i] = s
			}
			headersMap[k] = anyV
		}

		cookiesMap := result["cookies"].(map[string]any)
		for _, v := range res.Cookies() {
			cookiesMap[v.Name] = map[string]any{
				"name":  v.Name,
				"value": v.Value,
			}
		}

		if len(bodyRaw) > 0 {
			// try as map
			var jsonMap map[string]any
			if err := json.Unmarshal(bodyRaw, &jsonMap); err == nil {
				result["json"] = jsonMap
			} else {
				// try as array
				var jsonArr []any
				if err := json.Unmarshal(bodyRaw, &jsonArr); err == nil {
					result["json"] = jsonArr
				} else {
					result["json"] = nil
				}
			}
		}

		return result, nil
	})

	vm.Exec(`globalThis.$http = {
		send: __http_send,
	};`)
}

// -------------------------------------------------------------------

func registerFactoryAsConstructorRamune(vm *ramune.Runtime, proxy *typeProxy, constructorName string, factoryFunc any) {
	rv := reflect.ValueOf(factoryFunc)
	rt := reflect.TypeOf(factoryFunc)
	totalArgs := rt.NumIn()

	vm.RegisterFunc(constructorName, func(args []any) (any, error) {
		in := make([]reflect.Value, totalArgs)

		for i := 0; i < totalArgs; i++ {
			var jsVal any
			if i < len(args) {
				jsVal = args[i]
			}
			if jsVal == nil {
				in[i] = reflect.New(rt.In(i)).Elem()
			} else {
				converted, err := proxy.jsToGo(jsVal, rt.In(i))
				if err != nil {
					// Fallback: use zero value
					in[i] = reflect.New(rt.In(i)).Elem()
				} else {
					in[i] = converted
				}
			}
		}

		result := rv.Call(in)

		if len(result) != 1 {
			return nil, fmt.Errorf("the factory function should return only 1 item")
		}

		instance := result[0].Interface()
		handle := proxy.registry.register(instance)
		return map[string]any{"__handle": float64(handle), "__type": constructorName}, nil
	})
}

var cachedDynamicModelStructs = store.New[string, reflect.Type](nil)

func newDynamicModel(shape map[string]any) any {
	info := make([]*shapeFieldInfo, 0, len(shape))

	var hash strings.Builder

	sortedKeys := make([]string, 0, len(shape))
	for k := range shape {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)

	for _, k := range sortedKeys {
		v := shape[k]
		vt := reflect.TypeOf(v)

		switch vt.Kind() {
		case reflect.Map:
			raw, _ := json.Marshal(v)
			newV := types.JSONMap[any]{}
			newV.Scan(raw)
			v = newV
			vt = reflect.TypeOf(v)
		case reflect.Slice, reflect.Array:
			raw, _ := json.Marshal(v)
			newV := types.JSONArray[any]{}
			newV.Scan(raw)
			v = newV
			vt = reflect.TypeOf(newV)
		case reflect.Pointer:
			v = nil
		}

		hash.WriteString(k)
		hash.WriteString(":")
		hash.WriteString(vt.String())
		hash.WriteString("|")

		info = append(info, &shapeFieldInfo{key: k, value: v, valueType: vt})
	}

	st := cachedDynamicModelStructs.GetOrSet(hash.String(), func() reflect.Type {
		structFields := make([]reflect.StructField, len(info))

		for i, item := range info {
			structFields[i] = reflect.StructField{
				Name: inflector.UcFirst(item.key),
				Type: item.valueType,
				Tag:  reflect.StructTag(`db:"` + item.key + `" json:"` + item.key + `" form:"` + item.key + `"`),
			}
		}

		return reflect.StructOf(structFields)
	})

	elem := reflect.New(st).Elem()

	for i, item := range info {
		if item.value == nil {
			continue
		}
		elem.Field(i).Set(reflect.ValueOf(item.value))
	}

	return elem.Addr().Interface()
}

type shapeFieldInfo struct {
	value     any
	valueType reflect.Type
	key       string
}
