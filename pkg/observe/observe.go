// Package observe wires slog and OpenTelemetry for the demo host.
package observe

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otellog "go.opentelemetry.io/otel/log"
	logsdk "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

// Runtime owns process-wide OpenTelemetry providers and a slog logger.
type Runtime struct {
	Logger *slog.Logger
	traces *tracesdk.TracerProvider
	logs   *logsdk.LoggerProvider
}

// NewMotel configures JSON OTLP/HTTP traces and logs for a motel server.
// endpoint should be the motel base URL, e.g. http://127.0.0.1:27686.
func NewMotel(ctx context.Context, service, endpoint string) (*Runtime, error) {
	endpoint = strings.TrimRight(endpoint, "/")
	if _, err := url.ParseRequestURI(endpoint); err != nil {
		return nil, fmt.Errorf("parse motel endpoint %q: %w", endpoint, err)
	}
	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(service),
		semconv.ServiceVersion("dev"),
	)

	traceProvider := tracesdk.NewTracerProvider(
		tracesdk.WithBatcher(newMotelTraceExporter(endpoint+"/v1/traces"), tracesdk.WithBatchTimeout(200*time.Millisecond)),
		tracesdk.WithResource(res),
	)
	otel.SetTracerProvider(traceProvider)

	logProvider := logsdk.NewLoggerProvider(
		logsdk.WithProcessor(logsdk.NewBatchProcessor(newMotelLogExporter(endpoint+"/v1/logs"), logsdk.WithExportInterval(200*time.Millisecond))),
		logsdk.WithResource(res),
	)
	logger := otelslog.NewLogger(
		"extensible-go",
		otelslog.WithLoggerProvider(logProvider),
		otelslog.WithSource(true),
	)

	select {
	case <-ctx.Done():
		_ = traceProvider.Shutdown(context.Background())
		_ = logProvider.Shutdown(context.Background())
		return nil, fmt.Errorf("configure motel observability: %w", ctx.Err())
	default:
	}

	return &Runtime{Logger: logger, traces: traceProvider, logs: logProvider}, nil
}

// NewTextLogger returns a normal slog text logger for human-visible debug logs.
func NewTextLogger(level slog.Leveler) *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// Shutdown flushes traces and logs.
func (r *Runtime) Shutdown(ctx context.Context) error {
	var errs []error
	if r.traces != nil {
		if err := r.traces.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if r.logs != nil {
		if err := r.logs.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("shutdown observability: %v", errs)
	}
	return nil
}

type motelTraceExporter struct {
	endpoint string
	client   *http.Client
	stopped  atomic.Bool
}

func newMotelTraceExporter(endpoint string) *motelTraceExporter {
	return &motelTraceExporter{endpoint: endpoint, client: &http.Client{Timeout: 5 * time.Second}}
}

func (e *motelTraceExporter) ExportSpans(ctx context.Context, spans []tracesdk.ReadOnlySpan) error {
	if e.stopped.Load() || len(spans) == 0 {
		return nil
	}
	request := otlpTraceRequest{ResourceSpans: make([]otlpResourceSpans, 0, len(spans))}
	for _, span := range spans {
		request.ResourceSpans = append(request.ResourceSpans, otlpResourceSpans{
			Resource: otlpResource{Attributes: attributeKVs(span.Resource().Attributes())},
			ScopeSpans: []otlpScopeSpans{{
				Scope: otlpScope{Name: span.InstrumentationScope().Name},
				Spans: []otlpSpan{spanToOTLP(span)},
			}},
		})
	}
	if err := postJSON(ctx, e.client, e.endpoint, request); err != nil {
		return fmt.Errorf("export motel spans: %w", err)
	}
	return nil
}

func (e *motelTraceExporter) Shutdown(context.Context) error {
	e.stopped.Store(true)
	return nil
}

type motelLogExporter struct {
	endpoint string
	client   *http.Client
	stopped  atomic.Bool
}

func newMotelLogExporter(endpoint string) *motelLogExporter {
	return &motelLogExporter{endpoint: endpoint, client: &http.Client{Timeout: 5 * time.Second}}
}

func (e *motelLogExporter) Export(ctx context.Context, records []logsdk.Record) error {
	if e.stopped.Load() || len(records) == 0 {
		return nil
	}
	request := otlpLogRequest{ResourceLogs: make([]otlpResourceLogs, 0, len(records))}
	for _, record := range records {
		request.ResourceLogs = append(request.ResourceLogs, otlpResourceLogs{
			Resource: otlpResource{Attributes: attributeKVs(record.Resource().Attributes())},
			ScopeLogs: []otlpScopeLogs{{
				Scope:      otlpScope{Name: record.InstrumentationScope().Name},
				LogRecords: []otlpLogRecord{recordToOTLP(record)},
			}},
		})
	}
	if err := postJSON(ctx, e.client, e.endpoint, request); err != nil {
		return fmt.Errorf("export motel logs: %w", err)
	}
	return nil
}

func (e *motelLogExporter) ForceFlush(context.Context) error { return nil }

func (e *motelLogExporter) Shutdown(context.Context) error {
	e.stopped.Store(true)
	return nil
}

type otlpTraceRequest struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans,omitempty"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource,omitempty"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans,omitempty"`
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes,omitempty"`
}

type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope,omitempty"`
	Spans []otlpSpan `json:"spans,omitempty"`
}

type otlpScope struct {
	Name string `json:"name,omitempty"`
}

type otlpSpan struct {
	TraceID           string          `json:"traceId"`
	SpanID            string          `json:"spanId"`
	ParentSpanID      string          `json:"parentSpanId,omitempty"`
	Name              string          `json:"name,omitempty"`
	Kind              int             `json:"kind,omitempty"`
	StartTimeUnixNano string          `json:"startTimeUnixNano,omitempty"`
	EndTimeUnixNano   string          `json:"endTimeUnixNano,omitempty"`
	Attributes        []otlpKeyValue  `json:"attributes,omitempty"`
	Status            otlpSpanStatus  `json:"status,omitempty"`
	Events            []otlpSpanEvent `json:"events,omitempty"`
}

type otlpSpanStatus struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type otlpSpanEvent struct {
	TimeUnixNano string         `json:"timeUnixNano,omitempty"`
	Name         string         `json:"name,omitempty"`
	Attributes   []otlpKeyValue `json:"attributes,omitempty"`
}

type otlpLogRequest struct {
	ResourceLogs []otlpResourceLogs `json:"resourceLogs,omitempty"`
}

type otlpResourceLogs struct {
	Resource  otlpResource    `json:"resource,omitempty"`
	ScopeLogs []otlpScopeLogs `json:"scopeLogs,omitempty"`
}

type otlpScopeLogs struct {
	Scope      otlpScope       `json:"scope,omitempty"`
	LogRecords []otlpLogRecord `json:"logRecords,omitempty"`
}

type otlpLogRecord struct {
	TimeUnixNano         string         `json:"timeUnixNano,omitempty"`
	ObservedTimeUnixNano string         `json:"observedTimeUnixNano,omitempty"`
	SeverityText         string         `json:"severityText,omitempty"`
	Body                 otlpAnyValue   `json:"body,omitempty"`
	Attributes           []otlpKeyValue `json:"attributes,omitempty"`
	TraceID              string         `json:"traceId,omitempty"`
	SpanID               string         `json:"spanId,omitempty"`
}

type otlpKeyValue struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value,omitempty"`
}

type otlpAnyValue struct {
	StringValue *string         `json:"stringValue,omitempty"`
	BoolValue   *bool           `json:"boolValue,omitempty"`
	IntValue    *string         `json:"intValue,omitempty"`
	DoubleValue *float64        `json:"doubleValue,omitempty"`
	BytesValue  *string         `json:"bytesValue,omitempty"`
	ArrayValue  *otlpArrayValue `json:"arrayValue,omitempty"`
	KVListValue *otlpKVList     `json:"kvlistValue,omitempty"`
}

type otlpArrayValue struct {
	Values []otlpAnyValue `json:"values,omitempty"`
}

type otlpKVList struct {
	Values []otlpKeyValue `json:"values,omitempty"`
}

func spanToOTLP(span tracesdk.ReadOnlySpan) otlpSpan {
	ctx := span.SpanContext()
	parent := span.Parent()
	result := otlpSpan{
		TraceID:           ctx.TraceID().String(),
		SpanID:            ctx.SpanID().String(),
		Name:              span.Name(),
		Kind:              int(span.SpanKind()),
		StartTimeUnixNano: unixNanoString(span.StartTime()),
		EndTimeUnixNano:   unixNanoString(span.EndTime()),
		Attributes:        attributeKVs(span.Attributes()),
		Status:            statusToOTLP(span.Status()),
		Events:            spanEventsToOTLP(span.Events()),
	}
	if parent.IsValid() {
		result.ParentSpanID = parent.SpanID().String()
	}
	return result
}

func statusToOTLP(status tracesdk.Status) otlpSpanStatus {
	result := otlpSpanStatus{Message: status.Description}
	switch status.Code {
	case codes.Unset:
		result.Code = 0
	case codes.Ok:
		result.Code = 1
	case codes.Error:
		result.Code = 2
	default:
		result.Code = 0
	}
	return result
}

func spanEventsToOTLP(events []tracesdk.Event) []otlpSpanEvent {
	result := make([]otlpSpanEvent, 0, len(events))
	for _, event := range events {
		result = append(result, otlpSpanEvent{
			TimeUnixNano: unixNanoString(event.Time),
			Name:         event.Name,
			Attributes:   attributeKVs(event.Attributes),
		})
	}
	return result
}

func recordToOTLP(record logsdk.Record) otlpLogRecord {
	attrs := make([]otlpKeyValue, 0, record.AttributesLen())
	record.WalkAttributes(func(kv otellog.KeyValue) bool {
		attrs = append(attrs, logKV(kv))
		return true
	})
	result := otlpLogRecord{
		TimeUnixNano:         unixNanoString(record.Timestamp()),
		ObservedTimeUnixNano: unixNanoString(record.ObservedTimestamp()),
		SeverityText:         severityText(record),
		Body:                 logValue(record.Body()),
		Attributes:           attrs,
	}
	if record.TraceID().IsValid() {
		result.TraceID = record.TraceID().String()
	}
	if record.SpanID().IsValid() {
		result.SpanID = record.SpanID().String()
	}
	return result
}

func severityText(record logsdk.Record) string {
	if text := record.SeverityText(); text != "" {
		return text
	}
	severity := record.Severity()
	if severity == otellog.SeverityUndefined {
		return "INFO"
	}
	return severity.String()
}

func attributeKVs(attrs []attribute.KeyValue) []otlpKeyValue {
	result := make([]otlpKeyValue, 0, len(attrs))
	for _, attr := range attrs {
		result = append(result, otlpKeyValue{Key: string(attr.Key), Value: attributeValue(attr.Value)})
	}
	return result
}

func logKV(kv otellog.KeyValue) otlpKeyValue {
	return otlpKeyValue{Key: kv.Key, Value: logValue(kv.Value)}
}

func attributeValue(value attribute.Value) otlpAnyValue {
	switch value.Type() {
	case attribute.BOOL:
		return boolValue(value.AsBool())
	case attribute.INT64:
		return intValue(value.AsInt64())
	case attribute.FLOAT64:
		return doubleValue(value.AsFloat64())
	case attribute.STRING:
		return stringValue(value.AsString())
	case attribute.BOOLSLICE:
		return arrayValue(boolSlice(value.AsBoolSlice()))
	case attribute.INT64SLICE:
		return arrayValue(intSlice(value.AsInt64Slice()))
	case attribute.FLOAT64SLICE:
		return arrayValue(floatSlice(value.AsFloat64Slice()))
	case attribute.STRINGSLICE:
		return arrayValue(stringSlice(value.AsStringSlice()))
	case attribute.EMPTY:
		return stringValue("")
	default:
		return stringValue(value.Emit())
	}
}

func logValue(value otellog.Value) otlpAnyValue {
	switch value.Kind() {
	case otellog.KindEmpty:
		return stringValue("")
	case otellog.KindBool:
		return boolValue(value.AsBool())
	case otellog.KindFloat64:
		return doubleValue(value.AsFloat64())
	case otellog.KindInt64:
		return intValue(value.AsInt64())
	case otellog.KindString:
		return stringValue(value.AsString())
	case otellog.KindBytes:
		encoded := base64.StdEncoding.EncodeToString(value.AsBytes())
		return otlpAnyValue{BytesValue: &encoded}
	case otellog.KindSlice:
		return arrayValue(logSlice(value.AsSlice()))
	case otellog.KindMap:
		return kvListValue(logMap(value.AsMap()))
	default:
		return stringValue(value.String())
	}
}

func boolSlice(values []bool) []otlpAnyValue {
	result := make([]otlpAnyValue, 0, len(values))
	for _, value := range values {
		result = append(result, boolValue(value))
	}
	return result
}

func intSlice(values []int64) []otlpAnyValue {
	result := make([]otlpAnyValue, 0, len(values))
	for _, value := range values {
		result = append(result, intValue(value))
	}
	return result
}

func floatSlice(values []float64) []otlpAnyValue {
	result := make([]otlpAnyValue, 0, len(values))
	for _, value := range values {
		result = append(result, doubleValue(value))
	}
	return result
}

func stringSlice(values []string) []otlpAnyValue {
	result := make([]otlpAnyValue, 0, len(values))
	for _, value := range values {
		result = append(result, stringValue(value))
	}
	return result
}

func logSlice(values []otellog.Value) []otlpAnyValue {
	result := make([]otlpAnyValue, 0, len(values))
	for _, value := range values {
		result = append(result, logValue(value))
	}
	return result
}

func logMap(values []otellog.KeyValue) []otlpKeyValue {
	result := make([]otlpKeyValue, 0, len(values))
	for _, value := range values {
		result = append(result, logKV(value))
	}
	return result
}

func stringValue(value string) otlpAnyValue {
	return otlpAnyValue{StringValue: &value}
}

func boolValue(value bool) otlpAnyValue {
	return otlpAnyValue{BoolValue: &value}
}

func intValue(value int64) otlpAnyValue {
	text := fmt.Sprintf("%d", value)
	return otlpAnyValue{IntValue: &text}
}

func doubleValue(value float64) otlpAnyValue {
	return otlpAnyValue{DoubleValue: &value}
}

func arrayValue(values []otlpAnyValue) otlpAnyValue {
	return otlpAnyValue{ArrayValue: &otlpArrayValue{Values: values}}
}

func kvListValue(values []otlpKeyValue) otlpAnyValue {
	return otlpAnyValue{KVListValue: &otlpKVList{Values: values}}
}

func unixNanoString(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return fmt.Sprintf("%d", t.UnixNano())
}

func postJSON(ctx context.Context, client *http.Client, endpoint string, payload interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal OTLP JSON: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create OTLP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post OTLP request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusBadRequest {
		return nil
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return fmt.Errorf("read OTLP error response: %w", err)
	}
	return fmt.Errorf("OTLP request failed: status=%s body=%s", resp.Status, strings.TrimSpace(string(responseBody)))
}

var (
	_ tracesdk.SpanExporter = (*motelTraceExporter)(nil)
	_ logsdk.Exporter       = (*motelLogExporter)(nil)
)
