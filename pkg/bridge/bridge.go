// Package bridge is the single seam between Go and Lua.
//
// Go packages register small interfaces with BindInterface and native defaults
// with Register. Lua scripts can then inspect, replace, or wrap those named
// interfaces through the global registry table.
package bridge

import (
	"context"
	"extensible-go/pkg/diagnostic"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	lua "github.com/yuin/gopher-lua"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Registry holds named interface implementations. Lua sees a safe handle to a
// Registry through the global registry table.
type Registry struct {
	entries   map[string]interface{}
	prototype map[string]interface{}
}

func newRegistry() *Registry {
	return &Registry{
		entries:   make(map[string]interface{}),
		prototype: make(map[string]interface{}),
	}
}

// Register stores a default implementation and remembers its prototype so
// Reload can rebuild from Go defaults before re-applying Lua scripts.
func (r *Registry) Register(name string, impl interface{}) {
	r.entries[name] = impl
	r.prototype[name] = impl
}

// CloneDefaults creates an isolated registry containing the original Go
// defaults. Reload and ValidateDir use this so failed scripts cannot corrupt
// the live registry.
func (r *Registry) CloneDefaults() *Registry {
	clone := newRegistry()
	for name, proto := range r.prototype {
		clone.prototype[name] = proto
		clone.entries[name] = proto
	}
	return clone
}

// Get returns the current implementation.
func (r *Registry) Get(name string) interface{} {
	return r.entries[name]
}

// Names returns sorted registered names.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.entries))
	for n := range r.entries {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ProxyFactory creates a Go proxy that delegates to a Lua table. The returned
// value must satisfy the interface registered for this slot.
type ProxyFactory func(L *lua.LState, tbl *lua.LTable) interface{}

// ValueValidator verifies that a value satisfies the registered Go interface.
type ValueValidator func(interface{}) bool

// MetatableSetup registers the Lua metatable for an interface type so that Go
// values exposed to Lua support obj:Method(args) syntax.
type MetatableSetup func(L *lua.LState)

type binding struct {
	name     string
	meta     MetatableSetup
	proxy    ProxyFactory
	validate ValueValidator
}

// RuntimeSetupResult lets applications stage runtime-owned registrations while
// a Lua directory is loading. Commit runs only after a successful Reload swap;
// Cleanup runs when validation or reload discards the candidate runtime.
type RuntimeSetupResult struct {
	Commit  func()
	Cleanup func()
}

// Bridge owns Lua state, interface bindings, script loading, validation, and
// transactional reload. Create one Bridge per application process.
type Bridge struct {
	mu       sync.RWMutex // protects state/registry/binding swaps
	state    *lua.LState
	reg      *Registry
	bindings map[string]*binding
	loaded   bool
	logger   *slog.Logger

	// OnSetup is called after metatables are registered on a new LState and
	// before scripts execute. Use it to expose app-specific Lua globals such as
	// constructors. It runs during LoadDir, ValidateDir, and Reload.
	OnSetup func(L *lua.LState)

	// OnRuntimeSetup is like OnSetup, but can return staged state that commits
	// only after a successful Reload. Use this for extension registrations such
	// as commands and event handlers so ValidateDir cannot mutate live state.
	OnRuntimeSetup func(L *lua.LState) RuntimeSetupResult
}

// New creates a Bridge with a fresh Lua VM. Call Close at shutdown.
func New() *Bridge {
	L := lua.NewState()
	b := &Bridge{
		state:    L,
		reg:      newRegistry(),
		bindings: make(map[string]*binding),
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	b.exposeRegistryTo(L, b.reg)
	return b
}

// Close releases the current Lua VM. Do not call it while other goroutines may
// still call Lua-backed implementations.
func (b *Bridge) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state != nil {
		b.state.Close()
		b.state = nil
	}
}

// SetLogger sets the bridge logger. Passing nil restores a discard logger.
func (b *Bridge) SetLogger(logger *slog.Logger) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	b.logger = logger
}

// BindInterface registers a Go interface as a Lua extension point.
//
// name  — the key Lua uses, e.g. "auth", "store", "transformer".
// meta  — registers the metatable for Go values exposed to Lua.
// proxy — creates a Go implementation from a Lua table.
//
// Bind interfaces before LoadDir/Reload for the clearest lifecycle. Pass a
// validator to make bad Lua values fail during load/reload instead of panicking
// at the later Go call site.
func (b *Bridge) BindInterface(name string, meta MetatableSetup, proxy ProxyFactory, validators ...ValueValidator) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var validate ValueValidator
	if len(validators) > 0 {
		validate = validators[0]
	}
	b.bindings[name] = &binding{name: name, meta: meta, proxy: proxy, validate: validate}
	meta(b.state)
}

// Register stores a default Go implementation for a named interface.
func (b *Bridge) Register(name string, impl interface{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reg.Register(name, impl)
}

// Require returns the current implementation for name. Callers should type
// assert at the boundary they consume:
//
//	store := b.Require("store").(Store)
func (b *Bridge) Require(name string) interface{} {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.reg.Get(name)
}

// Names returns the registered extension slot names.
func (b *Bridge) Names() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.reg.Names()
}

// LoadDir transactionally loads dir for initial startup. It has the same
// semantics as Reload: scripts are evaluated against an isolated registry and
// swapped live only after the whole directory succeeds.
func (b *Bridge) LoadDir(dir string) error {
	return b.LoadDirContext(context.Background(), dir)
}

// LoadDirContext is LoadDir with context propagation for tracing/cancellation.
func (b *Bridge) LoadDirContext(ctx context.Context, dir string) error {
	return b.ReloadContext(ctx, dir)
}

// ValidateDir parses and executes scripts against an isolated Lua VM and
// isolated registry, then discards the result. The live registry and live VM are
// unchanged whether validation succeeds or fails.
func (b *Bridge) ValidateDir(dir string) error {
	return b.ValidateDirContext(context.Background(), dir)
}

// ValidateDirContext is ValidateDir with context propagation.
func (b *Bridge) ValidateDirContext(ctx context.Context, dir string) error {
	ctx, span := tracer().Start(ctx, "bridge.validate")
	defer span.End()
	span.SetAttributes(attribute.String("lua.dir", dir))

	runtime, err := b.buildRuntime(ctx, dir)
	if runtime != nil {
		runtime.discard()
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

// Reload creates a fresh Lua VM, restores all slots to their Go defaults,
// executes every .lua file from dir, and swaps the new runtime live only if the
// entire load succeeds. Failed reloads leave the old runtime untouched.
func (b *Bridge) Reload(dir string) error {
	return b.ReloadContext(context.Background(), dir)
}

// ReloadContext is Reload with context propagation.
func (b *Bridge) ReloadContext(ctx context.Context, dir string) error {
	ctx, span := tracer().Start(ctx, "bridge.reload")
	defer span.End()
	span.SetAttributes(attribute.String("lua.dir", dir))

	b.mu.RLock()
	logger := b.logger
	b.mu.RUnlock()
	logger.InfoContext(ctx, "bridge reload starting", "dir", dir)

	runtime, err := b.buildRuntime(ctx, dir)
	if err != nil {
		diag := diagnostic.Go(diagnostic.CodeReload, "reload", "reload failed; previous runtime remains active", err)
		span.RecordError(diag)
		span.SetStatus(codes.Error, diag.Error())
		logger.ErrorContext(ctx, "bridge reload failed", diag.Fields()...)
		return diag
	}

	b.mu.Lock()
	oldL := b.state
	firstLoad := !b.loaded
	b.state = runtime.L
	b.reg = runtime.reg
	b.loaded = true
	b.mu.Unlock()

	if runtime.commit != nil {
		runtime.commit()
	}

	// The first state is only a placeholder used while interfaces/defaults are
	// registered, so it is safe to close after the first successful load. Later
	// states may still be referenced by cached old proxies and must remain alive.
	if firstLoad && oldL != nil {
		oldL.Close()
	}
	logger.InfoContext(ctx, "bridge reload complete", "dir", dir)
	return nil
}

// DoFile executes a single Lua file against the current VM. This is an escape
// hatch; prefer LoadDir/Reload for transactional semantics.
func (b *Bridge) DoFile(path string) error {
	b.mu.RLock()
	L := b.state
	b.mu.RUnlock()
	if err := L.DoFile(path); err != nil {
		return fmt.Errorf("do lua file %s: %w", path, err)
	}
	return nil
}

// LState returns the current raw Lua state for advanced integrations. Direct
// users are responsible for gopher-lua's single-goroutine access requirement.
func (b *Bridge) LState() *lua.LState {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.state
}

type runtimeCandidate struct {
	L       *lua.LState
	reg     *Registry
	commit  func()
	cleanup func()
}

func (r *runtimeCandidate) discard() {
	if r.cleanup != nil {
		r.cleanup()
	}
	if r.L != nil {
		r.L.Close()
	}
}

func (b *Bridge) buildRuntime(ctx context.Context, dir string) (*runtimeCandidate, error) {
	ctx, span := tracer().Start(ctx, "lua.runtime.build")
	defer span.End()
	span.SetAttributes(attribute.String("lua.dir", dir))

	b.mu.RLock()
	bindings := make([]*binding, 0, len(b.bindings))
	for _, bind := range b.bindings {
		bindings = append(bindings, bind)
	}
	reg := b.reg.CloneDefaults()
	onSetup := b.OnSetup
	onRuntimeSetup := b.OnRuntimeSetup
	logger := b.logger
	b.mu.RUnlock()

	_, createSpan := tracer().Start(ctx, "lua.runtime.create")
	L := lua.NewState()
	createSpan.End()
	candidate := &runtimeCandidate{L: L, reg: reg}
	for _, bind := range bindings {
		bind.meta(L)
	}
	b.exposeRegistryTo(L, reg)
	if onSetup != nil {
		onSetup(L)
	}
	if onRuntimeSetup != nil {
		setup := onRuntimeSetup(L)
		candidate.commit = setup.Commit
		candidate.cleanup = setup.Cleanup
	}
	if err := loadDirInto(ctx, L, dir); err != nil {
		candidate.discard()
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		logger.ErrorContext(ctx, "lua runtime build failed", "dir", dir, "error", err)
		return nil, err
	}
	return candidate, nil
}

func loadDirInto(ctx context.Context, L *lua.LState, dir string) error {
	root, err := filepath.Abs(dir)
	if err != nil {
		root = dir
	}
	L.SetGlobal("feature_root", lua.LString(root))

	entries, err := os.ReadDir(dir)
	if err != nil {
		return &diagnostic.Error{
			Source:    diagnostic.SourceGo,
			Code:      diagnostic.CodeLuaLoad,
			Operation: "load_dir",
			File:      dir,
			Message:   "could not read Lua feature directory",
			Err:       err,
		}
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lua") {
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)

	// init.lua always first; every other .lua file runs alphabetically.
	sort.SliceStable(files, func(i, j int) bool {
		if files[i] == "init.lua" {
			return true
		}
		if files[j] == "init.lua" {
			return false
		}
		return i < j
	})

	for _, f := range files {
		path := filepath.Join(dir, f)
		fileCtx, span := tracer().Start(ctx, "lua.load_file")
		span.SetAttributes(attribute.String("lua.file", path))
		oldCtx := L.Context()
		L.SetContext(fileCtx)
		if err := L.DoFile(path); err != nil {
			restoreLuaContext(oldCtx, L)()
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			span.End()
			return &diagnostic.Error{
				Source:    diagnostic.SourceLua,
				Code:      diagnostic.CodeLuaLoad,
				Operation: "load_file",
				File:      path,
				Message:   "Lua feature file failed to load",
				Err:       err,
			}
		}
		restoreLuaContext(oldCtx, L)()
		span.End()
	}
	return nil
}

// LuaCall calls a method on a Lua table. It is the shared dispatch path from
// Go proxy types back into Lua.
//
// gopher-lua states are single-goroutine. If an app calls the same Lua-backed
// implementation concurrently, serialize those calls above this layer or use a
// future LState-pool runtime.
func LuaCall(L *lua.LState, method string, tbl *lua.LTable, nret int, args ...lua.LValue) ([]lua.LValue, error) {
	return LuaCallContext(luaContext(L), L, method, tbl, nret, args...)
}

// LuaCallContext calls a Lua table method with explicit trace context.
func LuaCallContext(ctx context.Context, L *lua.LState, method string, tbl *lua.LTable, nret int, args ...lua.LValue) ([]lua.LValue, error) {
	ctx, span := tracer().Start(ctx, "lua.call")
	defer span.End()
	span.SetAttributes(attribute.String("lua.method", method))

	oldCtx := L.Context()
	L.SetContext(ctx)
	defer restoreLuaContext(oldCtx, L)

	fn := L.GetField(tbl, method)
	if fn == lua.LNil {
		err := &diagnostic.Error{
			Source:    diagnostic.SourceLua,
			Code:      diagnostic.CodeMissingMethod,
			Operation: "lua_call",
			Method:    method,
			Message:   fmt.Sprintf("Lua table missing method %q", method),
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	L.Push(fn)
	L.Push(tbl) // self
	for _, a := range args {
		L.Push(a)
	}
	if err := L.PCall(len(args)+1, nret, nil); err != nil {
		diag := &diagnostic.Error{
			Source:    diagnostic.SourceLua,
			Code:      diagnostic.CodeLuaCall,
			Operation: "lua_call",
			Method:    method,
			Message:   "Lua method call failed",
			Err:       err,
		}
		span.RecordError(diag)
		span.SetStatus(codes.Error, diag.Error())
		return nil, diag
	}
	results := make([]lua.LValue, nret)
	for i := nret - 1; i >= 0; i-- {
		results[i] = L.Get(-(nret - i))
	}
	L.Pop(nret)
	return results, nil
}

type registryHandle struct {
	bridge *Bridge
	reg    *Registry
}

func (b *Bridge) exposeRegistryTo(L *lua.LState, reg *Registry) {
	mt := L.NewTypeMetatable("__bridge_registry")
	L.SetField(mt, "__index", L.SetFuncs(L.NewTable(), map[string]lua.LGFunction{
		"get":  luaRegistryGet,
		"set":  luaRegistrySet,
		"wrap": luaRegistryWrap,
		"list": luaRegistryList,
	}))

	ud := L.NewUserData()
	ud.Value = &registryHandle{bridge: b, reg: reg}
	L.SetMetatable(ud, mt)
	L.SetGlobal("registry", ud)

	L.SetGlobal("go_print", L.NewFunction(func(L *lua.LState) int {
		parts := make([]string, L.GetTop())
		for i := 1; i <= L.GetTop(); i++ {
			parts[i-1] = luaValueString(L.Get(i))
		}
		fmt.Print(strings.Join(parts, " ") + "\n")
		return 0
	}))
}

func registryFromLua(L *lua.LState) *registryHandle {
	return L.CheckUserData(1).Value.(*registryHandle)
}

func luaRegistryGet(L *lua.LState) int {
	h := registryFromLua(L)
	name := L.CheckString(2)
	impl := h.reg.Get(name)
	if impl == nil {
		L.Push(lua.LNil)
		return 1
	}

	ud := L.NewUserData()
	ud.Value = impl
	if bind, ok := h.bridge.binding(name); ok {
		L.SetMetatable(ud, L.GetTypeMetatable("__bridge_"+bind.name))
	}
	L.Push(ud)
	return 1
}

func luaRegistrySet(L *lua.LState) int {
	h := registryFromLua(L)
	name := L.CheckString(2)
	_, span := tracer().Start(luaContext(L), "lua.registry.set")
	defer span.End()
	span.SetAttributes(attribute.String("lua.slot", name))
	val := L.Get(3)

	bind, ok := h.bridge.binding(name)
	if !ok {
		L.RaiseError("diagnostic source=lua code=%s op=registry.set slot=%s msg=unknown registry slot", diagnostic.CodeUnknownSlot, name)
		return 0
	}

	impl, err := bind.valueFromLua(L, val)
	if err != nil {
		L.RaiseError("%s", err.Error())
		return 0
	}
	h.reg.entries[name] = impl
	return 0
}

func luaRegistryWrap(L *lua.LState) int {
	h := registryFromLua(L)
	name := L.CheckString(2)
	_, span := tracer().Start(luaContext(L), "lua.registry.wrap")
	defer span.End()
	span.SetAttributes(attribute.String("lua.slot", name))
	wrapper := L.CheckFunction(3)

	bind, ok := h.bridge.binding(name)
	if !ok {
		L.RaiseError("diagnostic source=lua code=%s op=registry.wrap slot=%s msg=unknown registry slot", diagnostic.CodeUnknownSlot, name)
		return 0
	}

	existing := h.reg.Get(name)
	if existing == nil {
		L.RaiseError("diagnostic source=lua code=%s op=registry.wrap slot=%s msg=slot has no implementation", diagnostic.CodeInvalidSlotValue, name)
		return 0
	}

	ud := L.NewUserData()
	ud.Value = existing
	L.SetMetatable(ud, L.GetTypeMetatable("__bridge_"+bind.name))

	L.Push(wrapper)
	L.Push(ud)
	if err := L.PCall(1, 1, nil); err != nil {
		L.RaiseError("diagnostic source=lua code=%s op=registry.wrap slot=%s msg=wrapper failed: %s", diagnostic.CodeLuaCall, name, err.Error())
		return 0
	}

	newVal := L.Get(-1)
	L.Pop(1)

	impl, err := bind.valueFromLua(L, newVal)
	if err != nil {
		L.RaiseError("%s", err.Error())
		return 0
	}
	h.reg.entries[name] = impl
	L.Push(lua.LBool(true))
	return 1
}

func luaRegistryList(L *lua.LState) int {
	h := registryFromLua(L)
	tbl := L.NewTable()
	for _, name := range h.reg.Names() {
		tbl.Append(lua.LString(name))
	}
	L.Push(tbl)
	return 1
}

func tracer() trace.Tracer {
	return otel.Tracer("extensible-go/bridge")
}

func luaContext(L *lua.LState) context.Context {
	if ctx := L.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

func restoreLuaContext(oldCtx context.Context, L *lua.LState) func() {
	return func() {
		if oldCtx != nil {
			L.SetContext(oldCtx)
		} else {
			L.RemoveContext()
		}
	}
}

func (b *Bridge) binding(name string) (*binding, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	bind, ok := b.bindings[name]
	return bind, ok
}

func (b *binding) valueFromLua(L *lua.LState, val lua.LValue) (interface{}, error) {
	var impl interface{}
	if tbl, isTable := val.(*lua.LTable); isTable {
		impl = b.proxy(L, tbl)
	} else if ud, isUD := val.(*lua.LUserData); isUD {
		impl = ud.Value
	} else {
		return nil, &diagnostic.Error{
			Source:    diagnostic.SourceLua,
			Code:      diagnostic.CodeInvalidSlotValue,
			Operation: "registry.set",
			Slot:      b.name,
			Message:   fmt.Sprintf("slot %q expects a Lua table or Go userdata, got %s", b.name, val.Type().String()),
		}
	}
	if b.validate != nil && !b.validate(impl) {
		return nil, &diagnostic.Error{
			Source:    diagnostic.SourceLua,
			Code:      diagnostic.CodeInvalidSlotValue,
			Operation: "registry.set",
			Slot:      b.name,
			Message:   fmt.Sprintf("replacement for slot %q does not satisfy the registered Go interface", b.name),
		}
	}
	return impl, nil
}

func luaValueString(v lua.LValue) string {
	switch val := v.(type) {
	case lua.LString:
		return string(val)
	case lua.LBool:
		return fmt.Sprintf("%v", bool(val))
	case lua.LNumber:
		return fmt.Sprintf("%v", float64(val))
	case *lua.LNilType:
		return "nil"
	case *lua.LUserData:
		return fmt.Sprintf("<userdata %T>", val.Value)
	case *lua.LTable:
		return "<table>"
	case *lua.LFunction:
		return "<function>"
	default:
		return v.String()
	}
}
