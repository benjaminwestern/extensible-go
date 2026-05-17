// Package host demonstrates the small application kernel that sits above the
// Lua bridge: named core seams, commands, events, validation, and hot reload.
package host

import (
	"context"
	"extensible-go/pkg/bridge"
	"extensible-go/pkg/diagnostic"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"sync"

	lua "github.com/yuin/gopher-lua"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Decision is returned by policy checks.
type Decision struct {
	Allow  bool
	Reason string
}

// Policy is a core seam. Lua can wrap or replace it via registry slot
// "core.policy".
type Policy interface {
	Check(action string) Decision
}

// AllowPolicy is the Go default: allow everything.
type AllowPolicy struct{}

// Check allows the action and records that the Go default made the decision.
func (AllowPolicy) Check(action string) Decision {
	return Decision{Allow: true, Reason: "allowed by Go default: " + action}
}

// CommandInfo is the inspectable command surface registered by Lua feature
// packs.
type CommandInfo struct {
	Name        string
	Description string
}

type luaCallback struct {
	L  *lua.LState
	fn *lua.LFunction
}

type command struct {
	CommandInfo
	handler luaCallback
}

type eventHandler struct {
	name    string
	handler luaCallback
}

type runtimeState struct {
	commands map[string]command
	events   map[string][]eventHandler
}

// App is the tiny host kernel. It intentionally exposes only a few surfaces:
// policy checks, commands, events, reload, validation, and inspection.
type App struct {
	mu       sync.RWMutex
	luaMu    sync.Mutex // serializes external access to live Lua states
	bridge   *bridge.Bridge
	out      io.Writer
	logger   *slog.Logger
	commands map[string]command
	events   map[string][]eventHandler
}

// New creates an App with the core policy seam registered.
func New(out io.Writer) *App {
	if out == nil {
		out = os.Stdout
	}
	a := &App{
		bridge:   bridge.New(),
		out:      out,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		commands: make(map[string]command),
		events:   make(map[string][]eventHandler),
	}
	a.bridge.BindInterface("core.policy", policyMeta, policyFactory, func(v interface{}) bool {
		_, ok := v.(Policy)
		return ok
	})
	a.bridge.Register("core.policy", AllowPolicy{})
	a.bridge.OnRuntimeSetup = func(L *lua.LState) bridge.RuntimeSetupResult {
		runtime := &runtimeState{
			commands: make(map[string]command),
			events:   make(map[string][]eventHandler),
		}
		a.exposeApp(L, runtime)
		return bridge.RuntimeSetupResult{
			Commit: func() {
				a.mu.Lock()
				defer a.mu.Unlock()
				a.commands = runtime.commands
				a.events = runtime.events
			},
		}
	}
	return a
}

// Close releases the current Lua bridge state.
func (a *App) Close() { a.bridge.Close() }

// LoadDir transactionally loads Lua features from dir.
func (a *App) LoadDir(dir string) error {
	return a.LoadDirContext(context.Background(), dir)
}

// LoadDirContext transactionally loads Lua features from dir with context propagation.
func (a *App) LoadDirContext(ctx context.Context, dir string) error {
	if err := a.bridge.LoadDirContext(ctx, dir); err != nil {
		return fmt.Errorf("host load dir: %w", err)
	}
	return nil
}

// Reload transactionally reloads Lua features from dir.
func (a *App) Reload(dir string) error { return a.ReloadContext(context.Background(), dir) }

// ReloadContext transactionally reloads Lua features from dir with context propagation.
func (a *App) ReloadContext(ctx context.Context, dir string) error {
	if err := a.bridge.ReloadContext(ctx, dir); err != nil {
		return fmt.Errorf("host reload: %w", err)
	}
	return nil
}

// ValidateDir checks Lua features from dir without mutating live app state.
func (a *App) ValidateDir(dir string) error { return a.ValidateDirContext(context.Background(), dir) }

// ValidateDirContext checks Lua features from dir without mutating live app state.
func (a *App) ValidateDirContext(ctx context.Context, dir string) error {
	if err := a.bridge.ValidateDirContext(ctx, dir); err != nil {
		return fmt.Errorf("host validate dir: %w", err)
	}
	return nil
}

// SetLogger sets the app and bridge logger. Passing nil disables logs.
func (a *App) SetLogger(logger *slog.Logger) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	a.mu.Lock()
	a.logger = logger
	a.mu.Unlock()
	a.bridge.SetLogger(logger)
}

// CheckPolicy executes the current core.policy implementation.
func (a *App) CheckPolicy(action string) Decision {
	return a.CheckPolicyContext(context.Background(), action)
}

// CheckPolicyContext executes the current core.policy implementation with trace context.
func (a *App) CheckPolicyContext(ctx context.Context, action string) Decision {
	ctx, span := tracer().Start(ctx, "host.check_policy")
	defer span.End()
	span.SetAttributes(attribute.String("policy.action", action))
	a.luaMu.Lock()
	defer a.luaMu.Unlock()
	decision := a.checkPolicyNoLock(ctx, action)
	span.SetAttributes(attribute.Bool("policy.allow", decision.Allow))
	if !decision.Allow {
		span.SetAttributes(attribute.String("policy.reason", decision.Reason))
	}
	a.currentLogger().DebugContext(ctx, "policy checked", "action", action, "allow", decision.Allow, "reason", decision.Reason)
	return decision
}

func (a *App) checkPolicyNoLock(ctx context.Context, action string) Decision {
	policy, ok := a.bridge.Require("core.policy").(Policy)
	if !ok {
		return Decision{Allow: false, Reason: "core.policy is not available"}
	}
	if proxy, ok := policy.(*policyProxy); ok {
		return proxy.CheckContext(ctx, action)
	}
	return policy.Check(action)
}

// Commands returns sorted command metadata.
func (a *App) Commands() []CommandInfo {
	a.mu.RLock()
	defer a.mu.RUnlock()
	infos := make([]CommandInfo, 0, len(a.commands))
	for _, cmd := range a.commands {
		infos = append(infos, cmd.CommandInfo)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos
}

// Slots returns the core slots exposed through the bridge.
func (a *App) Slots() []string {
	return a.bridge.Names()
}

// RunCommand invokes a Lua-registered command.
func (a *App) RunCommand(name, args string) error {
	return a.RunCommandContext(context.Background(), name, args)
}

// RunCommandContext invokes a Lua-registered command with trace context.
func (a *App) RunCommandContext(ctx context.Context, name, args string) error {
	ctx, span := tracer().Start(ctx, "host.run_command")
	defer span.End()
	span.SetAttributes(attribute.String("command.name", name))
	a.luaMu.Lock()
	defer a.luaMu.Unlock()
	if err := a.runCommandNoLock(ctx, name, args); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	a.currentLogger().DebugContext(ctx, "command completed", "command", name)
	return nil
}

func (a *App) runCommandNoLock(ctx context.Context, name, args string) error {
	a.mu.RLock()
	cmd, ok := a.commands[name]
	a.mu.RUnlock()
	if !ok {
		diag := &diagnostic.Error{
			Source:    diagnostic.SourceGo,
			Code:      diagnostic.CodeCommandNotFound,
			Operation: "run_command",
			Command:   name,
			Message:   fmt.Sprintf("unknown command %q", name),
		}
		a.logDiagnostic(diag)
		return diag
	}
	L := cmd.handler.L
	ctx, span := tracer().Start(ctx, "lua.command")
	defer span.End()
	span.SetAttributes(attribute.String("command.name", name))
	oldCtx := L.Context()
	L.SetContext(ctx)
	defer restoreLuaContext(oldCtx, L)

	L.Push(cmd.handler.fn)
	L.Push(lua.LString(args))
	L.Push(a.contextTable(L))
	if err := L.PCall(2, 0, nil); err != nil {
		diag := &diagnostic.Error{
			Source:    diagnostic.SourceLua,
			Code:      diagnostic.CodeCommandFailed,
			Operation: "run_command",
			Command:   name,
			Message:   "Lua command failed",
			Err:       err,
		}
		span.RecordError(diag)
		span.SetStatus(codes.Error, diag.Error())
		a.logDiagnostic(diag)
		return diag
	}
	return nil
}

// Emit invokes Lua handlers for an event name.
func (a *App) Emit(name, text string) error {
	return a.EmitContext(context.Background(), name, text)
}

// EmitContext invokes Lua handlers for an event name with trace context.
func (a *App) EmitContext(ctx context.Context, name, text string) error {
	ctx, span := tracer().Start(ctx, "host.emit_event")
	defer span.End()
	span.SetAttributes(attribute.String("event.name", name))
	a.luaMu.Lock()
	defer a.luaMu.Unlock()
	if err := a.emitNoLock(ctx, name, text); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}

func (a *App) emitNoLock(ctx context.Context, name, text string) error {
	a.mu.RLock()
	handlers := append([]eventHandler(nil), a.events[name]...)
	a.mu.RUnlock()
	for i, h := range handlers {
		L := h.handler.L
		handlerCtx, span := tracer().Start(ctx, "lua.event_handler")
		span.SetAttributes(attribute.String("event.name", name), attribute.Int("event.handler_index", i))
		oldCtx := L.Context()
		L.SetContext(handlerCtx)

		event := L.NewTable()
		L.SetField(event, "name", lua.LString(name))
		L.SetField(event, "text", lua.LString(text))
		L.Push(h.handler.fn)
		L.Push(event)
		L.Push(a.contextTable(L))
		if err := L.PCall(2, 0, nil); err != nil {
			restoreLuaContext(oldCtx, L)()
			diag := &diagnostic.Error{
				Source:    diagnostic.SourceLua,
				Code:      diagnostic.CodeEventFailed,
				Operation: "emit_event",
				Event:     name,
				Message:   "Lua event handler failed",
				Err:       err,
			}
			span.RecordError(diag)
			span.SetStatus(codes.Error, diag.Error())
			a.logDiagnostic(diag)
			span.End()
			return diag
		}
		restoreLuaContext(oldCtx, L)()
		span.End()
	}
	a.currentLogger().DebugContext(ctx, "event emitted", "event", name, "handler_count", len(handlers))
	return nil
}

func (a *App) currentLogger() *slog.Logger {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.logger
}

func (a *App) logDiagnostic(err *diagnostic.Error) {
	a.currentLogger().Error("host diagnostic", err.Fields()...)
}

func (a *App) exposeApp(L *lua.LState, runtime *runtimeState) {
	mt := L.NewTypeMetatable("__host_app")
	L.SetField(mt, "__index", L.SetFuncs(L.NewTable(), map[string]lua.LGFunction{
		"log": func(L *lua.LState) int {
			parts := make([]any, 0, L.GetTop()-1)
			for i := 2; i <= L.GetTop(); i++ {
				parts = append(parts, luaValueString(L.Get(i)))
			}
			_, _ = fmt.Fprintln(a.out, parts...)
			return 0
		},
		"on": func(L *lua.LState) int {
			name := L.CheckString(2)
			fn := L.CheckFunction(3)
			runtime.events[name] = append(runtime.events[name], eventHandler{
				name:    name,
				handler: luaCallback{L: L, fn: fn},
			})
			return 0
		},
		"register_command": func(L *lua.LState) int {
			name := L.CheckString(2)
			def := L.CheckTable(3)
			desc := ""
			if v := L.GetField(def, "description"); v != lua.LNil {
				desc = luaValueString(v)
			}
			handler := L.GetField(def, "handler")
			fn, ok := handler.(*lua.LFunction)
			if !ok {
				L.ArgError(3, "handler must be a function")
			}
			runtime.commands[name] = command{
				CommandInfo: CommandInfo{Name: name, Description: desc},
				handler:     luaCallback{L: L, fn: fn},
			}
			return 0
		},
	}))
	ud := L.NewUserData()
	ud.Value = a
	L.SetMetatable(ud, mt)
	L.SetGlobal("app", ud)
}

func (a *App) contextTable(L *lua.LState) *lua.LTable {
	ctx := L.NewTable()
	L.SetField(ctx, "print", L.NewFunction(func(L *lua.LState) int {
		parts := make([]any, 0, L.GetTop())
		for i := 1; i <= L.GetTop(); i++ {
			parts = append(parts, luaValueString(L.Get(i)))
		}
		_, _ = fmt.Fprintln(a.out, parts...)
		return 0
	}))
	L.SetField(ctx, "check", L.NewFunction(func(L *lua.LState) int {
		decision := a.checkPolicyNoLock(luaContext(L), L.CheckString(1))
		L.Push(decisionTable(L, decision))
		return 1
	}))
	L.SetField(ctx, "emit", L.NewFunction(func(L *lua.LState) int {
		if err := a.emitNoLock(luaContext(L), L.CheckString(1), L.OptString(2, "")); err != nil {
			L.RaiseError("%s", err.Error())
		}
		return 0
	}))
	return ctx
}

func decisionTable(L *lua.LState, d Decision) *lua.LTable {
	t := L.NewTable()
	L.SetField(t, "allow", lua.LBool(d.Allow))
	L.SetField(t, "reason", lua.LString(d.Reason))
	return t
}

func parseDecision(v lua.LValue) Decision {
	if b, ok := v.(lua.LBool); ok {
		return Decision{Allow: bool(b)}
	}
	tbl, ok := v.(*lua.LTable)
	if !ok {
		return Decision{Allow: false, Reason: "policy returned invalid decision"}
	}
	allow := false
	if allowValue := tbl.RawGetString("allow"); allowValue != lua.LNil {
		allow = lua.LVAsBool(allowValue)
	}
	reason := ""
	if reasonValue := tbl.RawGetString("reason"); reasonValue != lua.LNil {
		reason = luaValueString(reasonValue)
	}
	return Decision{Allow: allow, Reason: reason}
}

type policyProxy struct {
	L   *lua.LState
	tbl *lua.LTable
}

func (p *policyProxy) Check(action string) Decision {
	return p.CheckContext(context.Background(), action)
}

func (p *policyProxy) CheckContext(ctx context.Context, action string) Decision {
	results, err := bridge.LuaCallContext(ctx, p.L, "Check", p.tbl, 1, lua.LString(action))
	if err != nil {
		return Decision{Allow: false, Reason: err.Error()}
	}
	return parseDecision(results[0])
}

func policyMeta(L *lua.LState) {
	mt := L.NewTypeMetatable("__bridge_core.policy")
	L.SetField(mt, "__index", L.SetFuncs(L.NewTable(), map[string]lua.LGFunction{
		"Check": func(L *lua.LState) int {
			policy := L.CheckUserData(1).Value.(Policy)
			action := L.CheckString(2)
			if contextual, ok := policy.(interface {
				CheckContext(context.Context, string) Decision
			}); ok {
				L.Push(decisionTable(L, contextual.CheckContext(luaContext(L), action)))
				return 1
			}
			L.Push(decisionTable(L, policy.Check(action)))
			return 1
		},
	}))
}

func policyFactory(L *lua.LState, tbl *lua.LTable) interface{} {
	return &policyProxy{L: L, tbl: tbl}
}

func tracer() trace.Tracer {
	return otel.Tracer("extensible-go/host")
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
	default:
		return v.String()
	}
}
