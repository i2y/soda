package soda

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/i2y/ramune"
)

// objectRegistry manages Go objects that are referenced by integer handles from JS.
type objectRegistry struct {
	mu      sync.RWMutex
	objects map[int64]any
	nextID  atomic.Int64
}

func newObjectRegistry() *objectRegistry {
	return &objectRegistry{
		objects: make(map[int64]any),
	}
}

func (r *objectRegistry) register(v any) int64 {
	id := r.nextID.Add(1)
	r.mu.Lock()
	r.objects[id] = v
	r.mu.Unlock()
	return id
}

func (r *objectRegistry) get(id int64) (any, bool) {
	r.mu.RLock()
	v, ok := r.objects[id]
	r.mu.RUnlock()
	return v, ok
}

func (r *objectRegistry) unregister(id int64) {
	r.mu.Lock()
	delete(r.objects, id)
	r.mu.Unlock()
}

// wrap registers a Go value and returns the JS-side proxy handle map.
func (r *objectRegistry) wrap(v any, typeName string) map[string]any {
	handle := r.register(v)
	return map[string]any{"__handle": float64(handle), "__type": typeName}
}

// resolve extracts a Go object from a JS proxy handle value.
func (r *objectRegistry) resolve(jsVal any) (any, bool) {
	m, ok := jsVal.(map[string]any)
	if !ok {
		return nil, false
	}
	handleRaw, exists := m["__handle"]
	if !exists {
		return nil, false
	}
	handle := int64(handleRaw.(float64))
	return r.get(handle)
}

var errorType = reflect.TypeOf((*error)(nil)).Elem()

type typeInfo struct {
	name      string
	typ       reflect.Type
	methods   []methodInfo
	wrapperJS string // cached JS wrapper code, computed once in registerType
}

type methodInfo struct {
	goName string
	jsName string
	index  int
}

type typeProxy struct {
	registry      *objectRegistry
	types         map[string]*typeInfo    // JS name -> typeInfo
	typesByGoType map[reflect.Type]string // reverse: Go type -> JS name
	mu            sync.RWMutex
}

func newTypeProxy(registry *objectRegistry) *typeProxy {
	return &typeProxy{
		registry:      registry,
		types:         make(map[string]*typeInfo),
		typesByGoType: make(map[reflect.Type]string),
	}
}

// registerType reflects on a Go type and pre-computes method info.
// The specimen is used to determine the type (can be a value or pointer).
func (tp *typeProxy) registerType(name string, specimen any) *typeInfo {
	tp.mu.Lock()
	defer tp.mu.Unlock()

	if existing, ok := tp.types[name]; ok {
		return existing
	}

	t := reflect.TypeOf(specimen)

	info := &typeInfo{
		name: name,
		typ:  t,
	}

	for i := 0; i < t.NumMethod(); i++ {
		method := t.Method(i)
		if !method.IsExported() {
			continue
		}
		info.methods = append(info.methods, methodInfo{
			goName: method.Name,
			jsName: convertGoToJSName(method.Name),
			index:  i,
		})
	}

	// Cache the generated JS wrapper code
	info.wrapperJS = generateWrapperJS(info)

	tp.types[name] = info
	tp.typesByGoType[t] = name
	return info
}

// getTypeInfo retrieves the type info for a registered type name.
func (tp *typeProxy) getTypeInfo(name string) (*typeInfo, bool) {
	tp.mu.RLock()
	info, ok := tp.types[name]
	tp.mu.RUnlock()
	return info, ok
}

func (tp *typeProxy) resolveTypeName(v any) string {
	if v == nil {
		return ""
	}
	tp.mu.RLock()
	name := tp.typesByGoType[reflect.TypeOf(v)]
	tp.mu.RUnlock()
	return name
}

// goToJS converts a Go value to a JS-friendly representation.
// Primitives and maps are returned directly.
// Registered Go types are wrapped as proxy handles (returned as map with __handle and __type).
// Unregistered structs are serialized as maps.
func (tp *typeProxy) goToJS(v any) any {
	if v == nil {
		return nil
	}

	rv := reflect.ValueOf(v)

	// Unwrap interface
	if rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
		v = rv.Interface()
	}

	// Check for nil pointer
	if rv.Kind() == reflect.Ptr && rv.IsNil() {
		return nil
	}

	typeName := tp.resolveTypeName(v)
	if typeName != "" {
		return tp.registry.wrap(v, typeName)
	}

	// Handle basic types
	switch val := v.(type) {
	case bool:
		return val
	case int:
		return float64(val)
	case int8:
		return float64(val)
	case int16:
		return float64(val)
	case int32:
		return float64(val)
	case int64:
		return float64(val)
	case uint:
		return float64(val)
	case uint8:
		return float64(val)
	case uint16:
		return float64(val)
	case uint32:
		return float64(val)
	case uint64:
		return float64(val)
	case float32:
		return float64(val)
	case float64:
		return val
	case string:
		return val
	case []byte:
		return val
	case error:
		return val.Error()
	}

	// Handle slices
	if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
		if rv.IsNil() {
			return nil
		}
		result := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			result[i] = tp.goToJS(rv.Index(i).Interface())
		}
		return result
	}

	// Handle maps
	if rv.Kind() == reflect.Map {
		if rv.IsNil() {
			return nil
		}
		result := make(map[string]any)
		for _, key := range rv.MapKeys() {
			result[fmt.Sprint(key.Interface())] = tp.goToJS(rv.MapIndex(key).Interface())
		}
		return result
	}

	// Handle structs (unregistered) - serialize as JSON map
	if rv.Kind() == reflect.Struct || (rv.Kind() == reflect.Ptr && rv.Elem().Kind() == reflect.Struct) {
		// Try JSON serialization first for types that implement json.Marshaler
		if data, err := json.Marshal(v); err == nil {
			var result any
			if err := json.Unmarshal(data, &result); err == nil {
				return result
			}
		}
	}

	// Fallback: convert to string
	return fmt.Sprint(v)
}

// jsToGo converts a JS value back to a Go value suitable for a reflected method parameter.
// If the target type is a registered proxy type, it looks up the handle from the registry.
func (tp *typeProxy) jsToGo(jsVal any, targetType reflect.Type) (reflect.Value, error) {
	if jsVal == nil {
		return reflect.Zero(targetType), nil
	}

	// Check if the JS value is a proxy handle (map with __handle)
	if m, ok := jsVal.(map[string]any); ok {
		if handleRaw, exists := m["__handle"]; exists {
			handle := int64(handleRaw.(float64))
			obj, found := tp.registry.get(handle)
			if !found {
				return reflect.Value{}, fmt.Errorf("proxy handle %d not found", handle)
			}
			objVal := reflect.ValueOf(obj)
			if objVal.Type().AssignableTo(targetType) {
				return objVal, nil
			}
			if targetType.Kind() == reflect.Interface && objVal.Type().Implements(targetType) {
				return objVal, nil
			}
			return reflect.Value{}, fmt.Errorf("proxy object type %T not assignable to %s", obj, targetType)
		}
	}

	// Handle basic type conversions
	switch targetType.Kind() {
	case reflect.String:
		if s, ok := jsVal.(string); ok {
			return reflect.ValueOf(s), nil
		}
		return reflect.ValueOf(fmt.Sprint(jsVal)), nil
	case reflect.Bool:
		if b, ok := jsVal.(bool); ok {
			return reflect.ValueOf(b), nil
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if f, ok := toFloat(jsVal); ok {
			return reflect.ValueOf(f).Convert(targetType), nil
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if f, ok := toFloat(jsVal); ok {
			return reflect.ValueOf(f).Convert(targetType), nil
		}
	case reflect.Float32, reflect.Float64:
		if f, ok := toFloat(jsVal); ok {
			return reflect.ValueOf(f).Convert(targetType), nil
		}
	case reflect.Map:
		if m, ok := jsVal.(map[string]any); ok {
			if targetType == reflect.TypeOf(map[string]any(nil)) {
				return reflect.ValueOf(m), nil
			}
			// Try JSON round-trip for custom map types
			data, _ := json.Marshal(m)
			ptr := reflect.New(targetType)
			if err := json.Unmarshal(data, ptr.Interface()); err == nil {
				return ptr.Elem(), nil
			}
		}
	case reflect.Slice:
		if s, ok := jsVal.([]any); ok {
			if targetType == reflect.TypeOf([]any(nil)) {
				return reflect.ValueOf(s), nil
			}
			if targetType == reflect.TypeOf([]string(nil)) {
				strs := make([]string, len(s))
				for i, v := range s {
					strs[i] = fmt.Sprint(v)
				}
				return reflect.ValueOf(strs), nil
			}
		}
	case reflect.Interface:
		// any / interface{} — pass through as-is
		return reflect.ValueOf(jsVal), nil
	case reflect.Ptr:
		// For pointer types, try to create the pointed-to value
		elemType := targetType.Elem()
		if elemType.Kind() == reflect.Struct {
			if m, ok := jsVal.(map[string]any); ok {
				data, _ := json.Marshal(m)
				ptr := reflect.New(elemType)
				if err := json.Unmarshal(data, ptr.Interface()); err == nil {
					return ptr, nil
				}
			}
		}
	case reflect.Func:
		// For function types, if we receive a string (serialized JS function),
		// we can't convert it here — the caller must handle function args specially.
		return reflect.Zero(targetType), fmt.Errorf("function parameters must be handled by the caller")
	}

	// Last resort: try JSON round-trip
	data, err := json.Marshal(jsVal)
	if err == nil {
		ptr := reflect.New(targetType)
		if err := json.Unmarshal(data, ptr.Interface()); err == nil {
			return ptr.Elem(), nil
		}
	}

	return reflect.Value{}, fmt.Errorf("cannot convert %T to %s", jsVal, targetType)
}

// jsNameToGoName tries to find a Go field name in a struct type that matches the JS name.
// It checks all visible fields including promoted fields from embedded structs.
func jsNameToGoName(jsName string, st reflect.Type) string {
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		if !f.IsExported() {
			// Check embedded struct fields
			if f.Anonymous && f.Type.Kind() == reflect.Struct {
				if name := jsNameToGoName(jsName, f.Type); name != "" {
					return name
				}
			}
			continue
		}
		if convertGoToJSName(f.Name) == jsName {
			return f.Name
		}
		// Also check embedded struct fields
		if f.Anonymous {
			embType := f.Type
			if embType.Kind() == reflect.Ptr {
				embType = embType.Elem()
			}
			if embType.Kind() == reflect.Struct {
				if name := jsNameToGoName(jsName, embType); name != "" {
					return name
				}
			}
		}
	}
	return ""
}

func toFloat(v any) (float64, bool) {
	switch val := v.(type) {
	case float64:
		return val, true
	case float32:
		return float64(val), true
	case int:
		return float64(val), true
	case int8:
		return float64(val), true
	case int16:
		return float64(val), true
	case int32:
		return float64(val), true
	case int64:
		return float64(val), true
	case uint:
		return float64(val), true
	case uint8:
		return float64(val), true
	case uint16:
		return float64(val), true
	case uint32:
		return float64(val), true
	case uint64:
		return float64(val), true
	default:
		return 0, false
	}
}

func (tp *typeProxy) processReturnValues(fnType reflect.Type, out []reflect.Value) (any, error) {
	switch len(out) {
	case 0:
		return nil, nil
	case 1:
		if fnType.Out(0).Implements(errorType) {
			if out[0].IsNil() {
				return nil, nil
			}
			return nil, out[0].Interface().(error)
		}
		return tp.goToJS(out[0].Interface()), nil
	case 2:
		var retErr error
		if !out[1].IsNil() {
			retErr = out[1].Interface().(error)
		}
		return tp.goToJS(out[0].Interface()), retErr
	default:
		return tp.goToJS(out[0].Interface()), nil
	}
}

func extractFloat64(args []any, idx int) (float64, bool) {
	if idx >= len(args) {
		return 0, false
	}
	f, ok := args[idx].(float64)
	return f, ok
}

func extractString(args []any, idx int) (string, bool) {
	if idx >= len(args) {
		return "", false
	}
	s, ok := args[idx].(string)
	return s, ok
}

// installProxyRuntime registers the __proxy_call and __proxy_wrap functions
// on a Ramune runtime, and generates the JS-side wrapper factory.
func (tp *typeProxy) installProxyRuntime(rt *ramune.Runtime) error {
	// __proxy_call: Call a method on a proxied Go object.
	// args: [handle (float64), typeName (string), methodName (string), ...methodArgs]
	err := rt.RegisterFunc("__proxy_call", func(args []any) (any, error) {
		if len(args) < 3 {
			return nil, fmt.Errorf("__proxy_call requires at least 3 args (handle, type, method)")
		}

		handleF, ok := args[0].(float64)
		if !ok {
			return nil, fmt.Errorf("__proxy_call: handle must be a number")
		}
		typeName, ok := args[1].(string)
		if !ok {
			return nil, fmt.Errorf("__proxy_call: typeName must be a string")
		}
		methodName, ok := args[2].(string)
		if !ok {
			return nil, fmt.Errorf("__proxy_call: methodName must be a string")
		}
		handle := int64(handleF)
		methodArgs := args[3:]

		obj, found := tp.registry.get(handle)
		if !found {
			return nil, fmt.Errorf("proxy handle %d not found", handle)
		}

		info, ok := tp.getTypeInfo(typeName)
		if !ok {
			return nil, fmt.Errorf("unknown proxy type: %s", typeName)
		}

		// Find the method
		var mi *methodInfo
		for idx := range info.methods {
			if info.methods[idx].jsName == methodName {
				mi = &info.methods[idx]
				break
			}
		}
		if mi == nil {
			return nil, fmt.Errorf("unknown method %s on type %s", methodName, typeName)
		}

		// Call the method via reflection
		objVal := reflect.ValueOf(obj)
		method := objVal.Method(mi.index)
		methodType := method.Type()

		numIn := methodType.NumIn()
		in := make([]reflect.Value, numIn)
		for i := 0; i < numIn; i++ {
			if i < len(methodArgs) {
				converted, err := tp.jsToGo(methodArgs[i], methodType.In(i))
				if err != nil {
					return nil, fmt.Errorf("arg %d of %s.%s: %w", i, typeName, methodName, err)
				}
				in[i] = converted
			} else {
				in[i] = reflect.Zero(methodType.In(i))
			}
		}

		out := method.Call(in)
		return tp.processReturnValues(methodType, out)
	})
	if err != nil {
		return fmt.Errorf("registering __proxy_call: %w", err)
	}

	// __proxy_get_field: Get a field value from a proxied Go struct.
	// args: [handle (float64), fieldName (string)]
	err = rt.RegisterFunc("__proxy_get_field", func(args []any) (any, error) {
		if len(args) < 2 {
			return nil, fmt.Errorf("__proxy_get_field requires 2 args")
		}
		handle := int64(args[0].(float64))
		fieldName := args[1].(string)

		obj, found := tp.registry.get(handle)
		if !found {
			return nil, fmt.Errorf("proxy handle %d not found", handle)
		}

		rv := reflect.ValueOf(obj)
		if rv.Kind() == reflect.Ptr {
			rv = rv.Elem()
		}
		if rv.Kind() != reflect.Struct {
			return nil, fmt.Errorf("object is not a struct")
		}

		// Find field by JS name (including promoted fields from embedded structs)
		st := rv.Type()
		// First check direct and promoted fields
		for i := 0; i < st.NumField(); i++ {
			f := st.Field(i)
			if !f.IsExported() {
				continue
			}
			if convertGoToJSName(f.Name) == fieldName {
				return tp.goToJS(rv.Field(i).Interface()), nil
			}
		}

		// Try FieldByName for promoted fields from embedded structs
		goName := jsNameToGoName(fieldName, st)
		if goName != "" {
			fv := rv.FieldByName(goName)
			if fv.IsValid() {
				return tp.goToJS(fv.Interface()), nil
			}
		}

		return nil, fmt.Errorf("field %s not found", fieldName)
	})
	if err != nil {
		return fmt.Errorf("registering __proxy_get_field: %w", err)
	}

	// __proxy_set_field: Set a field value on a proxied Go struct.
	err = rt.RegisterFunc("__proxy_set_field", func(args []any) (any, error) {
		if len(args) < 3 {
			return nil, fmt.Errorf("__proxy_set_field requires 3 args")
		}
		handle := int64(args[0].(float64))
		fieldName := args[1].(string)
		value := args[2]

		obj, found := tp.registry.get(handle)
		if !found {
			return nil, fmt.Errorf("proxy handle %d not found", handle)
		}

		rv := reflect.ValueOf(obj)
		if rv.Kind() == reflect.Ptr {
			rv = rv.Elem()
		}
		if rv.Kind() != reflect.Struct {
			return nil, fmt.Errorf("object is not a struct")
		}

		st := rv.Type()
		for i := 0; i < st.NumField(); i++ {
			f := st.Field(i)
			if !f.IsExported() {
				continue
			}
			if convertGoToJSName(f.Name) == fieldName {
				fieldVal := rv.Field(i)
				if !fieldVal.CanSet() {
					return nil, fmt.Errorf("field %s is not settable", fieldName)
				}
				converted, err := tp.jsToGo(value, f.Type)
				if err != nil {
					return nil, fmt.Errorf("setting field %s: %w", fieldName, err)
				}
				fieldVal.Set(converted)
				return nil, nil
			}
		}

		return nil, fmt.Errorf("field %s not found", fieldName)
	})
	if err != nil {
		return fmt.Errorf("registering __proxy_set_field: %w", err)
	}

	// __proxy_release: Release a proxy handle.
	err = rt.RegisterFunc("__proxy_release", func(args []any) (any, error) {
		if len(args) < 1 {
			return nil, nil
		}
		handle := int64(args[0].(float64))
		tp.registry.unregister(handle)
		return nil, nil
	})
	if err != nil {
		return fmt.Errorf("registering __proxy_release: %w", err)
	}

	// Install the JS-side __proxy_wrap factory and __proxy_type_info
	return rt.Exec(proxyRuntimeJS)
}


// collectExportedFields recursively collects exported field names from a type,
// including promoted fields from embedded structs.
func collectExportedFields(t reflect.Type) []string {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}

	var fields []string
	seen := map[string]bool{}

	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.IsExported() && !f.Anonymous {
			jsName := convertGoToJSName(f.Name)
			if !seen[jsName] {
				fields = append(fields, jsName)
				seen[jsName] = true
			}
		}
		// Recurse into embedded structs for promoted fields
		if f.Anonymous {
			embType := f.Type
			if embType.Kind() == reflect.Ptr {
				embType = embType.Elem()
			}
			if embType.Kind() == reflect.Struct {
				for _, ef := range collectExportedFields(embType) {
					if !seen[ef] {
						fields = append(fields, ef)
						seen[ef] = true
					}
				}
			}
		}
	}
	return fields
}

// generateWrapperJS generates JS code that creates a wrapper object for a given type.
// This is called once per type and the result is cached in the JS global __proxy_types.
func generateWrapperJS(info *typeInfo) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("__proxy_types[%q] = function(handle) {\n", info.name))
	sb.WriteString("  var obj = { __handle: handle, __type: " + fmt.Sprintf("%q", info.name) + " };\n")

	// Generate field accessors via Object.defineProperty
	fields := collectExportedFields(info.typ)
	for _, fieldName := range fields {
		jsNameEsc, _ := json.Marshal(fieldName)
		sb.WriteString(fmt.Sprintf("  Object.defineProperty(obj, %s, {\n", string(jsNameEsc)))
		sb.WriteString(fmt.Sprintf("    get: function() {\n"))
		sb.WriteString(fmt.Sprintf("      var result = __proxy_get_field(handle, %s);\n", string(jsNameEsc)))
		sb.WriteString("      if (result && typeof result === 'object' && result.__handle !== undefined && result.__type) {\n")
		sb.WriteString("        return __proxy_wrap(result.__type, result.__handle);\n")
		sb.WriteString("      }\n")
		sb.WriteString("      return result;\n")
		sb.WriteString("    },\n")
		sb.WriteString(fmt.Sprintf("    set: function(v) { __proxy_set_field(handle, %s, v); },\n", string(jsNameEsc)))
		sb.WriteString("    enumerable: true,\n")
		sb.WriteString("    configurable: true\n")
		sb.WriteString("  });\n")
	}

	// Generate method wrappers
	for _, m := range info.methods {
		sb.WriteString(fmt.Sprintf("  obj[%q] = function() {\n", m.jsName))
		sb.WriteString(fmt.Sprintf("    var args = [handle, %q, %q];\n", info.name, m.jsName))
		sb.WriteString("    for (var i = 0; i < arguments.length; i++) {\n")
		sb.WriteString("      var a = arguments[i];\n")
		sb.WriteString("      if (typeof a === 'function') { args.push(a.toString()); }\n")
		sb.WriteString("      else if (a && a.__handle !== undefined) { args.push({__handle: a.__handle, __type: a.__type}); }\n")
		sb.WriteString("      else { args.push(a); }\n")
		sb.WriteString("    }\n")
		sb.WriteString("    var result = __proxy_call.apply(null, args);\n")
		sb.WriteString("    if (result && typeof result === 'object' && result.__handle !== undefined && result.__type) {\n")
		sb.WriteString("      return __proxy_wrap(result.__type, result.__handle);\n")
		sb.WriteString("    }\n")
		sb.WriteString("    return result;\n")
		sb.WriteString("  };\n")
	}

	sb.WriteString("  return obj;\n")
	sb.WriteString("};\n")

	return sb.String()
}

func (tp *typeProxy) registerTypeOnRuntime(rt *ramune.Runtime, info *typeInfo) error {
	return rt.Exec(info.wrapperJS)
}

const proxyRuntimeJS = `
(function() {
  // Global registry of type wrapper factories
  if (typeof globalThis.__proxy_types === 'undefined') {
    globalThis.__proxy_types = {};
  }

  // Wrap a Go object handle into a JS proxy object
  globalThis.__proxy_wrap = function(typeName, handle) {
    var factory = __proxy_types[typeName];
    if (!factory) {
      // Return a minimal wrapper if type not registered
      return { __handle: handle, __type: typeName };
    }
    return factory(handle);
  };

  // Unwrap a proxy object to its handle for passing back to Go
  globalThis.__proxy_unwrap = function(obj) {
    if (obj && obj.__handle !== undefined) {
      return { __handle: obj.__handle, __type: obj.__type };
    }
    return obj;
  };
})();
`
