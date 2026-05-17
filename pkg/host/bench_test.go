package host_test

import (
	"extensible-go/pkg/host"
	"io"
	"os"
	"path/filepath"
	"testing"
)

const luaReplacePolicy = `
registry:set("core.policy", {
    Check = function(self, action)
        if action == "blocked" then
            return { allow = false, reason = "blocked by Lua" }
        end
        return { allow = true, reason = "allowed by Lua" }
    end
})
`

const luaWrapPolicy = `
registry:wrap("core.policy", function(existing)
    return {
        Check = function(self, action)
            if action == "blocked" then
                return { allow = false, reason = "blocked by Lua wrapper" }
            end
            return existing:Check(action)
        end
    }
end)
`

const luaNoopCommand = `
app:register_command("noop", {
    description = "Do nothing",
    handler = function(args, ctx) end,
})
`

const luaNoopEvent = `
app:on("input", function(event, ctx) end)
`

const luaEventPolicyCheck = `
registry:wrap("core.policy", function(existing)
    return {
        Check = function(self, action)
            return existing:Check(action)
        end
    }
end)
app:on("input", function(event, ctx)
    ctx.check(event.text)
end)
`

var (
	benchDecision host.Decision
	benchErr      error
)

func writeBenchFeature(b *testing.B, body string) string {
	b.Helper()
	dir := b.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "init.lua"), []byte(body), 0o644); err != nil {
		b.Fatal(err)
	}
	return dir
}

func newBenchApp(b *testing.B, feature string) *host.App {
	b.Helper()
	app := host.New(io.Discard)
	b.Cleanup(app.Close)
	if feature != "" {
		if err := app.LoadDir(writeBenchFeature(b, feature)); err != nil {
			b.Fatal(err)
		}
	}
	return app
}

func BenchmarkPolicyCheck(b *testing.B) {
	b.Run("GoDirect", func(b *testing.B) {
		policy := host.AllowPolicy{}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			benchDecision = policy.Check("harmless")
		}
	})

	b.Run("GoThroughApp", func(b *testing.B) {
		app := newBenchApp(b, "")
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			benchDecision = app.CheckPolicy("harmless")
		}
	})

	b.Run("LuaReplace", func(b *testing.B) {
		app := newBenchApp(b, luaReplacePolicy)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			benchDecision = app.CheckPolicy("harmless")
		}
	})

	b.Run("LuaWrapGo", func(b *testing.B) {
		app := newBenchApp(b, luaWrapPolicy)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			benchDecision = app.CheckPolicy("harmless")
		}
	})
}

func BenchmarkLuaDispatch(b *testing.B) {
	b.Run("RunCommandNoop", func(b *testing.B) {
		app := newBenchApp(b, luaNoopCommand)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			benchErr = app.RunCommand("noop", "payload")
			if benchErr != nil {
				b.Fatal(benchErr)
			}
		}
	})

	b.Run("EmitNoopEvent", func(b *testing.B) {
		app := newBenchApp(b, luaNoopEvent)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			benchErr = app.Emit("input", "payload")
			if benchErr != nil {
				b.Fatal(benchErr)
			}
		}
	})

	b.Run("EmitEventWithPolicyCheck", func(b *testing.B) {
		app := newBenchApp(b, luaEventPolicyCheck)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			benchErr = app.Emit("input", "payload")
			if benchErr != nil {
				b.Fatal(benchErr)
			}
		}
	})
}

func BenchmarkLuaLifecycle(b *testing.B) {
	feature := luaWrapPolicy + luaNoopCommand + luaNoopEvent

	b.Run("ValidateDir", func(b *testing.B) {
		app := newBenchApp(b, feature)
		dir := writeBenchFeature(b, feature)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			benchErr = app.ValidateDir(dir)
			if benchErr != nil {
				b.Fatal(benchErr)
			}
		}
	})

	b.Run("Reload", func(b *testing.B) {
		app := newBenchApp(b, feature)
		dir := writeBenchFeature(b, feature)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			benchErr = app.Reload(dir)
			if benchErr != nil {
				b.Fatal(benchErr)
			}
		}
	})
}
