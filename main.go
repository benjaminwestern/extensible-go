// Package main provides a CLI for the extensible Go host-kernel demo.
package main

import (
	"bufio"
	"context"
	"errors"
	"extensible-go/pkg/diagnostic"
	"extensible-go/pkg/host"
	"extensible-go/pkg/observe"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strings"
	"time"
)

func main() {
	luaDir := flag.String("lua-dir", "features", "directory containing Lua feature files")
	noLua := flag.Bool("no-lua", false, "debug mode: ignore Lua files and run only Go defaults")
	debugLog := flag.Bool("debug-log", false, "write structured bridge/host diagnostics to stderr")
	otelEnabled := flag.Bool("otel", false, "export OTEL traces/logs over OTLP/HTTP")
	otelURL := flag.String("otel-url", "http://127.0.0.1:27686", "OTLP/HTTP base URL, e.g. motel")
	service := flag.String("service", "extensible-go", "OTEL service.name")
	flag.Parse()

	ctx := context.Background()
	app := host.New(os.Stdout)
	defer app.Close()
	var otelRuntime *observe.Runtime
	if *otelEnabled {
		runtime, err := observe.NewMotel(ctx, *service, *otelURL)
		if err != nil {
			log.Fatalf("otel setup: %v", err)
		}
		otelRuntime = runtime
		app.SetLogger(runtime.Logger)
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := otelRuntime.Shutdown(shutdownCtx); err != nil {
				log.Printf("otel shutdown: %v", err)
			}
		}()
	} else if *debugLog {
		app.SetLogger(observe.NewTextLogger(slog.LevelDebug))
	}

	fmt.Println("=== Extensible Go Host Kernel ===")
	luaEnabled := !*noLua
	if luaEnabled {
		if err := app.LoadDirContext(ctx, *luaDir); err != nil {
			printDiagnostic("startup failed", err)
			log.Fatal("refusing to start with invalid Lua; use -no-lua to debug Go defaults")
		}
	} else {
		fmt.Println("Lua disabled: running Go defaults only")
	}

	printHelp(luaEnabled)
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !handleCommand(ctx, app, *luaDir, luaEnabled, line) {
			return
		}
	}
}

func printHelp(luaEnabled bool) {
	commands := "help | slots | commands | check <action> | run <command> [args] | emit <event> [text] | quit"
	if luaEnabled {
		commands = "help | slots | commands | check <action> | run <command> [args] | emit <event> [text] | validate | reload | quit"
	}
	fmt.Println("Commands:", commands)
}

func handleCommand(ctx context.Context, app *host.App, luaDir string, luaEnabled bool, line string) bool {
	cmd, rest, _ := strings.Cut(line, " ")
	rest = strings.TrimSpace(rest)

	switch cmd {
	case "help":
		printHelp(luaEnabled)
	case "slots":
		for _, slot := range app.Slots() {
			fmt.Println(slot)
		}
	case "commands":
		for _, command := range app.Commands() {
			if command.Description == "" {
				fmt.Println(command.Name)
			} else {
				fmt.Printf("%s - %s\n", command.Name, command.Description)
			}
		}
	case "check":
		if rest == "" {
			fmt.Println("usage: check <action>")
			break
		}
		decision := app.CheckPolicyContext(ctx, rest)
		fmt.Printf("allow=%v reason=%q\n", decision.Allow, decision.Reason)
	case "run":
		name, args, _ := strings.Cut(rest, " ")
		if name == "" {
			fmt.Println("usage: run <command> [args]")
			break
		}
		if err := app.RunCommandContext(ctx, name, strings.TrimSpace(args)); err != nil {
			printDiagnostic("command failed", err)
		}
	case "emit":
		event, text, _ := strings.Cut(rest, " ")
		if event == "" {
			fmt.Println("usage: emit <event> [text]")
			break
		}
		if err := app.EmitContext(ctx, event, strings.TrimSpace(text)); err != nil {
			printDiagnostic("event failed", err)
		}
	case "validate":
		if !luaEnabled {
			fmt.Println("validate unavailable: Lua is disabled with -no-lua")
			break
		}
		if err := app.ValidateDirContext(ctx, luaDir); err != nil {
			printDiagnostic("validate failed", err)
		} else {
			fmt.Println("validate ok")
		}
	case "reload":
		if !luaEnabled {
			fmt.Println("reload unavailable: Lua is disabled with -no-lua")
			break
		}
		if err := app.ReloadContext(ctx, luaDir); err != nil {
			printDiagnostic("reload failed", err)
		} else {
			fmt.Println("reload ok")
		}
	case "quit", "exit":
		fmt.Println("bye")
		return false
	default:
		fmt.Println("unknown command:", cmd)
	}
	return true
}

func printDiagnostic(prefix string, err error) {
	printed := false
	for current := err; current != nil; current = errors.Unwrap(current) {
		var diag *diagnostic.Error
		if !errors.As(current, &diag) {
			continue
		}
		label := prefix
		if printed {
			label = "caused by"
		}
		printDiagnosticLine(label, diag)
		printed = true
	}
	if printed {
		fmt.Printf("detail: %v\n", err)
		return
	}
	fmt.Printf("%s: %v\n", prefix, err)
}

func printDiagnosticLine(label string, diag *diagnostic.Error) {
	fmt.Printf("%s: code=%s source=%s", label, diag.Code, diag.Source)
	if diag.File != "" {
		fmt.Printf(" file=%s", diag.File)
	}
	if diag.Slot != "" {
		fmt.Printf(" slot=%s", diag.Slot)
	}
	if diag.Command != "" {
		fmt.Printf(" command=%s", diag.Command)
	}
	if diag.Event != "" {
		fmt.Printf(" event=%s", diag.Event)
	}
	fmt.Printf(" message=%q\n", diag.UserMessage())
}
