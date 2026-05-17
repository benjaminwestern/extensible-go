// Package diagnostic provides structured errors for Go/Lua host failures.
package diagnostic

import (
	"fmt"
	"strings"
)

// Source identifies where a failure originated.
type Source string

const (
	// SourceGo marks failures originating in Go host code.
	SourceGo Source = "go"
	// SourceLua marks failures originating in Lua feature code.
	SourceLua Source = "lua"
)

// Code is a stable machine-readable diagnostic code.
type Code string

const (
	// CodeUnknownSlot means Lua referenced a registry slot that is not registered.
	CodeUnknownSlot Code = "unknown_slot"
	// CodeInvalidSlotValue means Lua provided a value that cannot satisfy the slot.
	CodeInvalidSlotValue Code = "invalid_slot_value"
	// CodeMissingMethod means a Lua table lacks a required method.
	CodeMissingMethod Code = "lua_missing_method"
	// CodeLuaCall means a Lua function call failed.
	CodeLuaCall Code = "lua_call_failed"
	// CodeLuaLoad means Lua feature loading failed.
	CodeLuaLoad Code = "lua_load_failed"
	// CodeReload means transactional reload failed and the old runtime stayed active.
	CodeReload Code = "reload_failed"
	// CodeCommandNotFound means a requested command is not registered.
	CodeCommandNotFound Code = "command_not_found"
	// CodeCommandFailed means a Lua command handler failed.
	CodeCommandFailed Code = "command_failed"
	// CodeEventFailed means a Lua event handler failed.
	CodeEventFailed Code = "event_failed"
	// CodeInvalidDefinition means Lua registered an invalid app definition.
	CodeInvalidDefinition Code = "invalid_definition"
)

// Error carries enough context for users to fix bad Lua or host integration
// failures without reading Go stack traces.
type Error struct {
	Source    Source
	Code      Code
	Operation string
	File      string
	Slot      string
	Method    string
	Command   string
	Event     string
	Message   string
	Err       error
}

func (e *Error) Error() string {
	parts := []string{"diagnostic"}
	if e.Source != "" {
		parts = append(parts, "source="+string(e.Source))
	}
	if e.Code != "" {
		parts = append(parts, "code="+string(e.Code))
	}
	if e.Operation != "" {
		parts = append(parts, "op="+e.Operation)
	}
	if e.File != "" {
		parts = append(parts, "file="+e.File)
	}
	if e.Slot != "" {
		parts = append(parts, "slot="+e.Slot)
	}
	if e.Method != "" {
		parts = append(parts, "method="+e.Method)
	}
	if e.Command != "" {
		parts = append(parts, "command="+e.Command)
	}
	if e.Event != "" {
		parts = append(parts, "event="+e.Event)
	}
	msg := e.Message
	if msg == "" && e.Err != nil {
		msg = e.Err.Error()
	}
	if msg != "" {
		parts = append(parts, "msg="+msg)
	}
	if e.Err != nil {
		parts = append(parts, "cause="+e.Err.Error())
	}
	return strings.Join(parts, " ")
}

func (e *Error) Unwrap() error { return e.Err }

// Lua constructs a Lua-origin diagnostic error.
func Lua(code Code, operation, message string, err error) *Error {
	return &Error{Source: SourceLua, Code: code, Operation: operation, Message: message, Err: err}
}

// Go constructs a Go-origin diagnostic error.
func Go(code Code, operation, message string, err error) *Error {
	return &Error{Source: SourceGo, Code: code, Operation: operation, Message: message, Err: err}
}

// Fields returns slog-compatible key/value pairs.
func (e *Error) Fields() []any {
	fields := []any{"source", e.Source, "code", e.Code}
	if e.Operation != "" {
		fields = append(fields, "operation", e.Operation)
	}
	if e.File != "" {
		fields = append(fields, "file", e.File)
	}
	if e.Slot != "" {
		fields = append(fields, "slot", e.Slot)
	}
	if e.Method != "" {
		fields = append(fields, "method", e.Method)
	}
	if e.Command != "" {
		fields = append(fields, "command", e.Command)
	}
	if e.Event != "" {
		fields = append(fields, "event", e.Event)
	}
	if e.Message != "" {
		fields = append(fields, "message", e.Message)
	}
	if e.Err != nil {
		fields = append(fields, "error", e.Err)
	}
	return fields
}

// UserMessage returns a concise human-facing message.
func (e *Error) UserMessage() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("%s failed", e.Operation)
}
