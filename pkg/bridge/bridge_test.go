package bridge_test

import (
	"extensible-go/pkg/bridge"
	"os"
	"path/filepath"
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

type Greeter interface {
	Greet(name string) string
}

type defaultGreeter struct{}

func (defaultGreeter) Greet(name string) string { return "go:" + name }

type greeterProxy struct {
	L   *lua.LState
	tbl *lua.LTable
}

func (p *greeterProxy) Greet(name string) string {
	results, err := bridge.LuaCall(p.L, "Greet", p.tbl, 1, lua.LString(name))
	if err != nil {
		return "ERROR: " + err.Error()
	}
	return string(results[0].(lua.LString))
}

func greeterMeta(L *lua.LState) {
	mt := L.NewTypeMetatable("__bridge_greeter")
	L.SetField(mt, "__index", L.SetFuncs(L.NewTable(), map[string]lua.LGFunction{
		"Greet": func(L *lua.LState) int {
			g := L.CheckUserData(1).Value.(Greeter)
			L.Push(lua.LString(g.Greet(L.CheckString(2))))
			return 1
		},
	}))
}

func greeterFactory(L *lua.LState, tbl *lua.LTable) interface{} {
	return &greeterProxy{L: L, tbl: tbl}
}

func newTestBridge(t *testing.T) *bridge.Bridge {
	t.Helper()
	b := bridge.New()
	t.Cleanup(b.Close)
	b.BindInterface("greeter", greeterMeta, greeterFactory, func(v interface{}) bool {
		_, ok := v.(Greeter)
		return ok
	})
	b.Register("greeter", defaultGreeter{})
	return b
}

func writeScript(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func requireGreeting(t *testing.T, b *bridge.Bridge, name string) string {
	t.Helper()
	return b.Require("greeter").(Greeter).Greet(name)
}

func TestLoadDirReplacesAndWrapsImplementation(t *testing.T) {
	b := newTestBridge(t)
	dir := t.TempDir()
	writeScript(t, dir, "init.lua", `
registry:set("greeter", {
    Greet = function(self, name)
        return "lua:" .. name
    end
})
registry:wrap("greeter", function(existing)
    return {
        Greet = function(self, name)
            return "[" .. existing:Greet(name) .. "]"
        end
    }
end)
`)

	if err := b.LoadDir(dir); err != nil {
		t.Fatal(err)
	}
	if got, want := requireGreeting(t, b, "ben"), "[lua:ben]"; got != want {
		t.Fatalf("greeting = %q, want %q", got, want)
	}
}

func TestValidateDirDoesNotMutateLiveRegistry(t *testing.T) {
	b := newTestBridge(t)
	live := t.TempDir()
	writeScript(t, live, "init.lua", `
registry:set("greeter", { Greet = function(self, name) return "live:" .. name end })
`)
	if err := b.LoadDir(live); err != nil {
		t.Fatal(err)
	}
	if got, want := requireGreeting(t, b, "a"), "live:a"; got != want {
		t.Fatalf("before validate = %q, want %q", got, want)
	}

	candidate := t.TempDir()
	writeScript(t, candidate, "init.lua", `
registry:set("greeter", { Greet = function(self, name) return "candidate:" .. name end })
`)
	if err := b.ValidateDir(candidate); err != nil {
		t.Fatal(err)
	}
	if got, want := requireGreeting(t, b, "a"), "live:a"; got != want {
		t.Fatalf("after validate = %q, want live state unchanged %q", got, want)
	}
}

func TestReloadPicksUpChangesAndRollsBackOnError(t *testing.T) {
	b := newTestBridge(t)
	dir := t.TempDir()
	writeScript(t, dir, "init.lua", `
registry:set("greeter", { Greet = function(self, name) return "v1:" .. name end })
`)
	if err := b.LoadDir(dir); err != nil {
		t.Fatal(err)
	}
	if got, want := requireGreeting(t, b, "x"), "v1:x"; got != want {
		t.Fatalf("initial = %q, want %q", got, want)
	}

	writeScript(t, dir, "init.lua", `
registry:set("greeter", { Greet = function(self, name) return "v2:" .. name end })
`)
	if err := b.Reload(dir); err != nil {
		t.Fatal(err)
	}
	if got, want := requireGreeting(t, b, "x"), "v2:x"; got != want {
		t.Fatalf("after reload = %q, want %q", got, want)
	}

	writeScript(t, dir, "init.lua", `this is not valid lua`)
	if err := b.Reload(dir); err == nil {
		t.Fatal("expected reload error")
	}
	if got, want := requireGreeting(t, b, "x"), "v2:x"; got != want {
		t.Fatalf("after failed reload = %q, want previous state %q", got, want)
	}
}

func TestMissingLuaMethodReturnsUsefulError(t *testing.T) {
	b := newTestBridge(t)
	dir := t.TempDir()
	writeScript(t, dir, "init.lua", `registry:set("greeter", {})`)
	if err := b.LoadDir(dir); err != nil {
		t.Fatal(err)
	}

	got := requireGreeting(t, b, "x")
	if !strings.Contains(got, `missing method "Greet"`) {
		t.Fatalf("expected missing method error, got %q", got)
	}
}

func TestInvalidBoundSlotValueFailsLoad(t *testing.T) {
	b := newTestBridge(t)
	dir := t.TempDir()
	writeScript(t, dir, "init.lua", `registry:set("greeter", 123)`)

	if err := b.LoadDir(dir); err == nil {
		t.Fatal("expected invalid slot value error")
	}
	if got, want := requireGreeting(t, b, "x"), "go:x"; got != want {
		t.Fatalf("failed load mutated default: got %q want %q", got, want)
	}
}

func TestUnknownSlotFailsLoad(t *testing.T) {
	b := newTestBridge(t)
	dir := t.TempDir()
	writeScript(t, dir, "init.lua", `registry:set("greteer", { Greet = function(self, name) return name end })`)

	if err := b.LoadDir(dir); err == nil {
		t.Fatal("expected unknown slot error")
	}
	if got, want := requireGreeting(t, b, "x"), "go:x"; got != want {
		t.Fatalf("failed load mutated default: got %q want %q", got, want)
	}
}
