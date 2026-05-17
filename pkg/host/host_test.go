package host_test

import (
	"bytes"
	"errors"
	"extensible-go/pkg/diagnostic"
	"extensible-go/pkg/host"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFeaturePackRegistersCommandsEventsAndWrapsPolicy(t *testing.T) {
	var out bytes.Buffer
	app := host.New(&out)
	defer app.Close()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "init.lua"), `
registry:wrap("core.policy", function(existing)
    return {
        Check = function(self, action)
            if action == "blocked" then
                return { allow = false, reason = "blocked by test feature" }
            end
            return existing:Check(action)
        end
    }
end)
app:register_command("hello", {
    description = "hello command",
    handler = function(args, ctx) ctx.print("hello", args) end,
})
app:on("input", function(event, ctx) ctx.print("event", event.text) end)
`)

	if err := app.LoadDir(dir); err != nil {
		t.Fatal(err)
	}
	if got := app.CheckPolicy("blocked"); got.Allow || got.Reason != "blocked by test feature" {
		t.Fatalf("policy = %#v", got)
	}
	if err := app.RunCommand("hello", "ben"); err != nil {
		t.Fatal(err)
	}
	if err := app.Emit("input", "abc"); err != nil {
		t.Fatal(err)
	}
	output := out.String()
	if !strings.Contains(output, "hello ben") || !strings.Contains(output, "event abc") {
		t.Fatalf("missing output in %q", output)
	}
}

func TestValidateDoesNotMutateCommandsOrPolicy(t *testing.T) {
	var out bytes.Buffer
	app := host.New(&out)
	defer app.Close()
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "init.lua"), `
app:register_command("live", { handler = function(args, ctx) ctx.print("live") end })
`)
	if err := app.LoadDir(live); err != nil {
		t.Fatal(err)
	}

	candidate := t.TempDir()
	writeFile(t, filepath.Join(candidate, "init.lua"), `
registry:set("core.policy", { Check = function(self, action) return { allow = false, reason = "candidate" } end })
app:register_command("candidate", { handler = function(args, ctx) ctx.print("candidate") end })
`)
	if err := app.ValidateDir(candidate); err != nil {
		t.Fatal(err)
	}
	if got := app.CheckPolicy("anything"); !got.Allow {
		t.Fatalf("validate mutated policy: %#v", got)
	}
	if err := app.RunCommand("live", ""); err != nil {
		t.Fatalf("live command missing: %v", err)
	}
	if err := app.RunCommand("candidate", ""); err == nil {
		t.Fatal("candidate command should not be live after validate")
	}
}

func TestReloadRollbackKeepsPreviousRuntime(t *testing.T) {
	var out bytes.Buffer
	app := host.New(&out)
	defer app.Close()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "init.lua"), `
registry:set("core.policy", { Check = function(self, action) return { allow = false, reason = "v1" } end })
app:register_command("version", { handler = function(args, ctx) ctx.print("v1") end })
`)
	if err := app.LoadDir(dir); err != nil {
		t.Fatal(err)
	}
	if got := app.CheckPolicy("x"); got.Reason != "v1" {
		t.Fatalf("initial policy = %#v", got)
	}

	writeFile(t, filepath.Join(dir, "init.lua"), `this is not valid lua`)
	if err := app.Reload(dir); err == nil {
		t.Fatal("expected reload failure")
	}
	if got := app.CheckPolicy("x"); got.Reason != "v1" {
		t.Fatalf("failed reload changed policy = %#v", got)
	}
	if err := app.RunCommand("version", ""); err != nil {
		t.Fatalf("failed reload removed command: %v", err)
	}
}

func TestInvalidPolicyReplacementFailsSafely(t *testing.T) {
	var out bytes.Buffer
	app := host.New(&out)
	defer app.Close()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "init.lua"), `registry:set("core.policy", 123)`)

	if err := app.LoadDir(dir); err == nil {
		t.Fatal("expected invalid replacement error")
	}
	if got := app.CheckPolicy("safe"); !got.Allow {
		t.Fatalf("failed load mutated Go default: %#v", got)
	}
}

func TestLuaRuntimeFailuresReturnDiagnosticsWithoutPanics(t *testing.T) {
	var out bytes.Buffer
	app := host.New(&out)
	defer app.Close()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "init.lua"), `
registry:set("core.policy", { Check = function(self, action) error("policy boom") end })
app:register_command("boom", { handler = function(args, ctx) error("command boom") end })
app:on("input", function(event, ctx) error("event boom") end)
`)
	if err := app.LoadDir(dir); err != nil {
		t.Fatal(err)
	}

	decision := app.CheckPolicy("dangerous")
	if decision.Allow || !strings.Contains(decision.Reason, "policy boom") {
		t.Fatalf("policy failure should deny with reason, got %#v", decision)
	}

	err := app.RunCommand("boom", "")
	var diag *diagnostic.Error
	if !errors.As(err, &diag) || diag.Code != diagnostic.CodeCommandFailed || diag.Command != "boom" {
		t.Fatalf("command error = %#v, want command diagnostic", err)
	}

	err = app.Emit("input", "hello")
	diag = nil
	if !errors.As(err, &diag) || diag.Code != diagnostic.CodeEventFailed || diag.Event != "input" {
		t.Fatalf("event error = %#v, want event diagnostic", err)
	}
}

func TestConcurrentLuaBackedPublicCallsAreSerialized(t *testing.T) {
	var out bytes.Buffer
	app := host.New(&out)
	defer app.Close()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "init.lua"), `
registry:wrap("core.policy", function(existing)
    return { Check = function(self, action) return existing:Check(action) end }
end)
app:register_command("check", { handler = function(args, ctx) ctx.check(args) end })
app:on("input", function(event, ctx) ctx.check(event.text) end)
`)
	if err := app.LoadDir(dir); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); _ = app.CheckPolicy("safe") }()
		go func() { defer wg.Done(); _ = app.RunCommand("check", "safe") }()
		go func() { defer wg.Done(); _ = app.Emit("input", "safe") }()
	}
	wg.Wait()
}
