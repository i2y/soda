// This file is adapted from github.com/pocketbase/pocketbase/plugins/jsvm.
// Copyright (c) 2022 - present, Gani Georgiev. Distributed under the MIT License.
// https://github.com/pocketbase/pocketbase/blob/master/LICENSE.md
//
// Modifications for the Ramune JS engine, Workers-style handlers,
// soda.toml config, and related Soda features:
// Copyright (c) 2026 - present, Yasushi Itoh.

// Package soda is a drop-in Ramune-backed replacement for upstream
// PocketBase's plugins/jsvm. Register it onto a *pocketbase.PocketBase
// instance to load pb_hooks/*.pb.{js,ts} and pb_migrations/*.{js,ts}
// with async/await, TypeScript, npm packages, Web Crypto, and the
// Cloudflare-Workers-style fetch/scheduled module API.
//
// Example:
//
//	app := pocketbase.New()
//	soda.MustRegister(app, soda.Config{HooksWatch: true})
//	app.Start()
package soda

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	esbuildapi "github.com/evanw/esbuild/pkg/api"
	"github.com/fatih/color"
	"github.com/fsnotify/fsnotify"
	"github.com/i2y/ramune"
	"github.com/pocketbase/pocketbase/core"
	"github.com/i2y/soda/internal/types/generated"
	"github.com/pocketbase/pocketbase/tools/router"
	"github.com/pocketbase/pocketbase/tools/template"
)

const typesFileName = "types.d.ts"

// Config defines the config options of the jsvm plugin.
type Config struct {
	// OnInit is an optional function that will be called
	// after a JS runtime is initialized, allowing you to
	// attach custom Go variables and functions.
	OnInit func(rt *ramune.Runtime)

	// HooksWatch enables auto app restarts when a JS app hook file changes.
	//
	// Note that currently the application cannot be automatically restarted on Windows
	// because the restart process relies on execve.
	HooksWatch bool

	// HooksDir specifies the JS app hooks directory.
	//
	// If not set it fallbacks to a relative "pb_data/../pb_hooks" directory.
	HooksDir string

	// HooksFilesPattern specifies a regular expression pattern that
	// identify which file to load by the hook vm(s).
	//
	// If not set it fallbacks to `^.*(\.pb\.js|\.pb\.ts)$`, aka. any
	// HooksDir file ending in ".pb.js" or ".pb.ts".
	HooksFilesPattern string

	// HooksPoolSize specifies how many Ramune Runtime instances to prewarm
	// and keep for the JS app hooks goroutine execution.
	//
	// Zero or negative value means that it will create a new Runtime
	// on every fired goroutine.
	HooksPoolSize int

	// MigrationsDir specifies the JS migrations directory.
	//
	// If not set it fallbacks to a relative "pb_data/../pb_migrations" directory.
	MigrationsDir string

	// If not set it fallbacks to `^.*(\.js|\.ts)$`, aka. any MigrationDir file
	// ending in ".js" or ".ts".
	MigrationsFilesPattern string

	// TypesDir specifies the directory where to store the embedded
	// TypeScript declarations file.
	//
	// If not set it fallbacks to "pb_data".
	//
	// Note: Avoid using the same directory as the HooksDir when HooksWatch is enabled
	// to prevent unnecessary app restarts when the types file is initially created.
	TypesDir string

	// NPMPackages specifies npm packages to make available in the JS runtime.
	// Example: []string{"lodash@4", "date-fns"}
	NPMPackages []string

	// Permissions controls sandbox access for the JS runtime.
	// If nil, all permissions are granted (default PocketBase behavior).
	//
	// Example (restrict to specific network hosts):
	//
	//	jsvm.Config{
	//	    Permissions: &ramune.Permissions{
	//	        Net:      ramune.PermDenied,
	//	        NetHosts: []string{"hooks.slack.com", "api.sendgrid.com"},
	//	        Read:     ramune.PermGranted,
	//	        Write:    ramune.PermDenied,
	//	        Run:      ramune.PermDenied,
	//	    },
	//	}
	Permissions *ramune.Permissions

	// WaitUntilTimeout bounds how long a Workers-style fetch handler's
	// executor VM stays held after the HTTP response has been written,
	// waiting for promises registered via ctx.waitUntil(promise) to settle.
	//
	// When the timeout elapses, the VM is returned to the pool and any
	// still-pending waitUntil promises are abandoned (they continue in
	// memory until the VM is re-initialized).
	//
	// If zero, defaults to 30s. Set negative to disable the timeout.
	WaitUntilTimeout time.Duration
}

// MustRegister registers the jsvm plugin in the provided app instance
// and panics if it fails.
func MustRegister(app core.App, config Config) {
	if err := Register(app, config); err != nil {
		panic(err)
	}
}

// Register registers the jsvm plugin in the provided app instance.
func Register(app core.App, config Config) error {
	p := &plugin{app: app, config: config}

	if p.config.HooksDir == "" {
		p.config.HooksDir = filepath.Join(app.DataDir(), "../pb_hooks")
	}

	if p.config.MigrationsDir == "" {
		p.config.MigrationsDir = filepath.Join(app.DataDir(), "../pb_migrations")
	}

	if p.config.HooksFilesPattern == "" {
		p.config.HooksFilesPattern = `^.*(\.pb\.js|\.pb\.ts)$`
	}

	if p.config.MigrationsFilesPattern == "" {
		p.config.MigrationsFilesPattern = `^.*(\.js|\.ts)$`
	}

	if p.config.TypesDir == "" {
		p.config.TypesDir = app.DataDir()
	}

	if p.config.WaitUntilTimeout == 0 {
		p.config.WaitUntilTimeout = 30 * time.Second
	}

	// Optional declarative config: soda.toml next to pb_hooks fills any
	// fields left unset on the Go-side Config (Go values win).
	tomlPath := filepath.Join(filepath.Dir(p.config.HooksDir), "soda.toml")
	tomlCfg, err := loadSodaTOML(tomlPath)
	if err != nil {
		return fmt.Errorf("loadSodaTOML: %w", err)
	}
	if err := applySodaTOML(&p.config, tomlCfg); err != nil {
		return err
	}
	if tomlCfg != nil {
		p.extraEnvBindingsJS = buildExtraEnvBindingsJS(tomlCfg.KVNamespaces)
	}

	p.app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		err := e.Next()
		if err != nil {
			return err
		}

		// ensure that the user has the latest types declaration
		err = p.refreshTypesFile()
		if err != nil {
			color.Yellow("Unable to refresh app types file: %v", err)
		}

		return nil
	})

	if err := p.registerMigrations(); err != nil {
		return fmt.Errorf("registerMigrations: %w", err)
	}

	if err := p.registerHooks(); err != nil {
		return fmt.Errorf("registerHooks: %w", err)
	}

	return nil
}

type plugin struct {
	app    core.App
	config Config

	// extraEnvBindingsJS is JS generated from soda.toml [[kv_namespaces]]
	// that installs globalThis.__extraEnvBindings(env) — read by the
	// __buildEnv helper on each executor VM. Empty when no bindings.
	extraEnvBindingsJS string
}

// newRuntime creates a new Ramune runtime with standard options.
func (p *plugin) newRuntime() (*ramune.Runtime, error) {
	opts := []ramune.Option{
		ramune.NodeCompat(),
		ramune.WithFetch(),
	}
	if len(p.config.NPMPackages) > 0 {
		opts = append(opts, ramune.Dependencies(p.config.NPMPackages...))
	}
	if p.config.Permissions != nil {
		opts = append(opts, ramune.WithPermissions(p.config.Permissions))
	}
	return ramune.New(opts...)
}

// transpileTypeScript converts TypeScript source to JavaScript using esbuild.
func transpileTypeScript(filename string, code string) (string, error) {
	result := esbuildapi.Transform(code, esbuildapi.TransformOptions{
		Sourcefile: filepath.Base(filename),
		Loader:     esbuildapi.LoaderTS,
		Target:     esbuildapi.ESNext,
	})
	if len(result.Errors) > 0 {
		return "", fmt.Errorf("TypeScript error in %s: %s", filename, result.Errors[0].Text)
	}
	return string(result.Code), nil
}

// registerMigrations registers the JS migrations loader.
func (p *plugin) registerMigrations() error {
	files, err := filesContent(p.config.MigrationsDir, p.config.MigrationsFilesPattern)
	if err != nil {
		return err
	}

	absHooksDir, err := filepath.Abs(p.config.HooksDir)
	if err != nil {
		return err
	}

	templateRegistry := template.NewRegistry()

	// Shared proxy infrastructure for migrations
	registry := newObjectRegistry()
	proxy := newTypeProxy(registry)

	// Register core types
	registerCoreTypes(p.app, proxy)

	for file, content := range files {
		vm, err := p.newRuntime()
		if err != nil {
			return fmt.Errorf("failed to create migration VM for %s: %w", file, err)
		}

		// Install proxy runtime
		proxy.installProxyRuntime(vm)

		// Register all type wrappers
		installTypeWrappers(vm, proxy)

		// Register bindings
		baseBinds(vm, proxy)
		dbxBinds(vm)
		securityBinds(vm)
		osBinds(vm, proxy)
		filepathBinds(vm)
		httpClientBinds(vm, proxy)
		filesystemBinds(vm, proxy)
		formsBinds(vm, proxy)
		mailsBinds(vm, proxy)

		// Bind $app and template registry
		appHandle := registry.register(p.app)
		vm.Exec(fmt.Sprintf(`globalThis.$app = __proxy_wrap("App", %d);`, appHandle))

		templateHandle := registry.register(templateRegistry)
		vm.Exec(fmt.Sprintf(`globalThis.$template = __proxy_wrap("TemplateRegistry", %d);`, templateHandle))

		vm.Exec(fmt.Sprintf(`globalThis.__hooks = %q;`, absHooksDir))

		// Register migrate function
		vm.RegisterFunc("migrate", func(args []any) (any, error) {
			if len(args) < 2 {
				return nil, fmt.Errorf("migrate requires up and down function arguments")
			}
			upCode, _ := args[0].(string)
			downCode, _ := args[1].(string)

			up := func(txApp core.App) error {
				return p.runMigrationFunc(txApp, upCode)
			}
			down := func(txApp core.App) error {
				return p.runMigrationFunc(txApp, downCode)
			}

			core.AppMigrations.Register(up, down, file)
			return nil, nil
		})

		if p.config.OnInit != nil {
			p.config.OnInit(vm)
		}

		// Execute the migration file
		code := string(content)
		if strings.HasSuffix(file, ".ts") {
			code, err = transpileTypeScript(file, code)
			if err != nil {
				return fmt.Errorf("failed to transpile migration %s: %w", file, err)
			}
		}

		if err := vm.Exec(code); err != nil {
			return fmt.Errorf("failed to run migration %s: %w", file, err)
		}
	}

	return nil
}

// registerHooks registers the JS app hooks loader.
func (p *plugin) registerHooks() error {
	files, err := filesContent(p.config.HooksDir, p.config.HooksFilesPattern)
	if err != nil {
		return err
	}

	// prepend the types reference directive
	for name, content := range files {
		if len(content) != 0 {
			continue
		}
		path := filepath.Join(p.config.HooksDir, name)
		directive := `/// <reference path="` + p.relativeTypesPath(p.config.HooksDir) + `" />`
		if err := prependToEmptyFile(path, directive+"\n\n"); err != nil {
			color.Yellow("Unable to prepend the types reference: %v", err)
		}
	}

	// initialize the hooks dir watcher
	if p.config.HooksWatch {
		if err := p.watchHooks(); err != nil {
			color.Yellow("Unable to init hooks watcher: %v", err)
		}
	}

	if len(files) == 0 {
		return nil
	}

	absHooksDir, err := filepath.Abs(p.config.HooksDir)
	if err != nil {
		return err
	}


	// Shared infrastructure
	registry := newObjectRegistry()
	proxy := newTypeProxy(registry)
	templateRegistry := template.NewRegistry()

	// Register core types
	registerCoreTypes(p.app, proxy)

	// Register $app handle (shared across loader and executors)
	appHandle := registry.register(p.app)
	templateHandle := registry.register(templateRegistry)

	sharedBinds := func(vm *ramune.Runtime) {
		// Install proxy runtime
		proxy.installProxyRuntime(vm)

		// Register all type wrappers
		installTypeWrappers(vm, proxy)

		// Register all bindings
		baseBinds(vm, proxy)
		dbxBinds(vm)
		filesystemBinds(vm, proxy)
		securityBinds(vm)
		osBinds(vm, proxy)
		filepathBinds(vm)
		httpClientBinds(vm, proxy)
		formsBinds(vm, proxy)
		apisBinds(vm, proxy)
		mailsBinds(vm, proxy)

		// Bind $app and $template
		vm.Exec(fmt.Sprintf(`globalThis.$app = __proxy_wrap("App", %d);`, appHandle))
		vm.Exec(fmt.Sprintf(`globalThis.$template = __proxy_wrap("TemplateRegistry", %d);`, templateHandle))
		vm.Exec(fmt.Sprintf(`globalThis.__hooks = %q;`, absHooksDir))

		// Workers-style env and request/response bindings
		workersEnvBinds(p.app, vm)
		workersRequestBinds(vm, proxy)

		if p.extraEnvBindingsJS != "" {
			if err := vm.Exec(p.extraEnvBindingsJS); err != nil {
				panic(fmt.Sprintf("extraEnvBindingsJS: %v", err))
			}
		}

		if p.config.OnInit != nil {
			p.config.OnInit(vm)
		}
	}

	// Initialize the executor VMs pool
	executors := newPool(p.config.HooksPoolSize, func() *ramune.Runtime {
		executor, err := p.newRuntime()
		if err != nil {
			panic(fmt.Sprintf("failed to create executor VM: %v", err))
		}
		sharedBinds(executor)
		return executor
	})

	// Initialize the loader VM
	loader, err := p.newRuntime()
	if err != nil {
		return fmt.Errorf("failed to create loader VM: %w", err)
	}
	sharedBinds(loader)
	hooksBinds(p.app, loader, executors, proxy)
	cronBinds(p.app, loader, executors, proxy)
	routerBinds(p.app, loader, executors, proxy)

	// Wrap all Go-registered callback-accepting functions so that JS
	// function arguments are serialised via .toString() before crossing
	// the Go boundary.  Ramune ≥ v0.4 passes functions as *JSFunc
	// instead of auto-serialising them to strings, but the executor
	// pool model requires string-form callbacks for re-evaluation.
	if err := loader.Exec(serializeFuncArgsJS); err != nil {
		return fmt.Errorf("serializeFuncArgsJS: %w", err)
	}

	for file, content := range files {
		func() {
			defer func() {
				if err := recover(); err != nil {
					fmtErr := fmt.Errorf("failed to execute %s:\n - %v", file, err)

					if p.config.HooksWatch {
						color.Red("%v", fmtErr)
					} else {
						panic(fmtErr)
					}
				}
			}()

			code := string(content)

			// Detect Workers-style files (export default { fetch, scheduled })
			// before transpilation since the raw source is the reliable place
			// to check for ESM syntax.
			if isWorkersStyle(code) {
				transformed, tErr := transpileWorkersModule(file, code)
				if tErr != nil {
					panic(tErr)
				}
				if err := loader.Exec(transformed); err != nil {
					panic(err)
				}
				if err := registerWorkersHandler(p.app, loader, executors, proxy, file, transformed, p.config.WaitUntilTimeout); err != nil {
					panic(err)
				}
				return
			}

			// Old-style hook file: transpile TS if needed, then exec.
			if strings.HasSuffix(file, ".ts") {
				var tsErr error
				code, tsErr = transpileTypeScript(file, code)
				if tsErr != nil {
					panic(tsErr)
				}
			}

			// Use Exec for hook file loading (not EvalAsync) since
			// hook files contain statements, not expressions.
			// The individual handlers will use EvalAsync when dispatched.
			if err := loader.Exec(code); err != nil {
				panic(err)
			}
		}()
	}

	return nil
}

// runMigrationFunc executes a JS migration function in an isolated VM.
func (p *plugin) runMigrationFunc(txApp core.App, jsCode string) error {
	vm, err := p.newRuntime()
	if err != nil {
		return err
	}
	defer vm.Close()

	reg := newObjectRegistry()
	prx := newTypeProxy(reg)
	registerCoreTypes(txApp, prx)
	prx.installProxyRuntime(vm)
	installTypeWrappers(vm, prx)
	baseBinds(vm, prx)
	dbxBinds(vm)

	txHandle := reg.register(txApp)
	vm.Exec(fmt.Sprintf(`globalThis.$app = __proxy_wrap("App", %d);`, txHandle))

	code := fmt.Sprintf(`(async function() { await (%s)($app); })()`, jsCode)
	_, err = vm.EvalAsync(code)
	return err
}

// registerCoreTypes registers all PocketBase core types with the proxy system.
func registerCoreTypes(app core.App, proxy *typeProxy) {
	proxy.registerType("App", app)
	proxy.registerType("Record", &core.Record{})
	proxy.registerType("Collection", &core.Collection{})
	proxy.registerType("RequestEvent", &core.RequestEvent{})
	proxy.registerType("TemplateRegistry", template.NewRegistry())

	// Register HTTP/router types for nested property access (e.g., e.request.pathValue())
	proxy.registerType("Request", &http.Request{})
	proxy.registerType("RouterEvent", &router.Event{})
}

// installTypeWrappers generates JS wrapper code for all registered types on a runtime.
func installTypeWrappers(vm *ramune.Runtime, proxy *typeProxy) {
	proxy.mu.RLock()
	defer proxy.mu.RUnlock()

	for _, info := range proxy.types {
		proxy.registerTypeOnRuntime(vm, info)
	}
}

// watchHooks initializes a hooks file watcher that will restart the
// application (*if possible) in case of a change in the hooks directory.
func (p *plugin) watchHooks() error {
	watchDir := p.config.HooksDir

	hooksDirInfo, err := os.Lstat(p.config.HooksDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}

	if hooksDirInfo.Mode()&os.ModeSymlink == os.ModeSymlink {
		watchDir, err = filepath.EvalSymlinks(p.config.HooksDir)
		if err != nil {
			return fmt.Errorf("failed to resolve hooksDir symlink: %w", err)
		}
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	var debounceTimer *time.Timer

	stopDebounceTimer := func() {
		if debounceTimer != nil {
			debounceTimer.Stop()
			debounceTimer = nil
		}
	}

	p.app.OnTerminate().BindFunc(func(e *core.TerminateEvent) error {
		watcher.Close()
		stopDebounceTimer()
		return e.Next()
	})

	go func() {
		defer stopDebounceTimer()

		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}

				stopDebounceTimer()

				debounceTimer = time.AfterFunc(50*time.Millisecond, func() {
					if runtime.GOOS == "windows" {
						color.Yellow("File %s changed, please restart the app manually", event.Name)
					} else {
						color.Yellow("File %s changed, restarting...", event.Name)
						if err := p.app.Restart(); err != nil {
							color.Red("Failed to restart the app:", err)
						}
					}
				})
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				color.Red("Watch error:", err)
			}
		}
	}()

	dirsErr := filepath.WalkDir(watchDir, func(path string, entry fs.DirEntry, err error) error {
		if !entry.IsDir() || entry.Name() == "node_modules" || strings.HasPrefix(entry.Name(), ".") {
			return nil
		}
		return watcher.Add(path)
	})
	if dirsErr != nil {
		watcher.Close()
	}

	return dirsErr
}

// fullTypesPath returns the full path to the generated TS file.
func (p *plugin) fullTypesPath() string {
	return filepath.Join(p.config.TypesDir, typesFileName)
}

// relativeTypesPath returns a path to the generated TS file relative
// to the specified basepath.
func (p *plugin) relativeTypesPath(basepath string) string {
	fullPath := p.fullTypesPath()

	rel, err := filepath.Rel(basepath, fullPath)
	if err != nil {
		rel = fullPath
	}

	return rel
}

// refreshTypesFile saves the embedded TS declarations as a file on the disk.
func (p *plugin) refreshTypesFile() error {
	fullPath := p.fullTypesPath()

	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		return err
	}

	data, err := generated.Types.ReadFile(typesFileName)
	if err != nil {
		return err
	}

	existingFile, err := os.Open(fullPath)
	if err == nil {
		timestamp := make([]byte, 13)
		io.ReadFull(existingFile, timestamp)
		existingFile.Close()

		if len(data) >= len(timestamp) && bytes.Equal(data[:13], timestamp) {
			return nil
		}
	}

	return os.WriteFile(fullPath, data, 0644)
}

// prependToEmptyFile prepends the specified text to an empty file.
func prependToEmptyFile(path, text string) error {
	info, err := os.Stat(path)

	if err == nil && info.Size() == 0 {
		return os.WriteFile(path, []byte(text), 0644)
	}

	return err
}

// filesContent returns a map with all direct files within the specified dir and their content.
func filesContent(dirPath string, pattern string) (map[string][]byte, error) {
	files, err := os.ReadDir(dirPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string][]byte{}, nil
		}
		return nil, err
	}

	var exp *regexp.Regexp
	if pattern != "" {
		var err error
		if exp, err = regexp.Compile(pattern); err != nil {
			return nil, err
		}
	}

	result := map[string][]byte{}

	for _, f := range files {
		if f.IsDir() || (exp != nil && !exp.MatchString(f.Name())) {
			continue
		}

		raw, err := os.ReadFile(filepath.Join(dirPath, f.Name()))
		if err != nil {
			return nil, err
		}

		result[f.Name()] = raw
	}

	return result, nil
}
