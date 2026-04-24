package middleware

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// InitTracing wires the OTLP/HTTP exporter against `otlpEndpoint`
// and registers the W3C traceparent propagator so incoming
// `traceparent` headers are extracted and outgoing HTTP calls
// inject them. The returned shutdown function drains the batch
// span processor; call it during graceful shutdown.
//
// When `otlpEndpoint` is empty the function is a no-op and returns
// a shutdown that does nothing, so local dev doesn't require an
// OTLP collector.
func InitTracing(ctx context.Context, serviceName, otlpEndpoint string) (func(context.Context) error, error) {
	if otlpEndpoint == "" {
		// Still register the propagator so upstream
		// `traceparent` headers flow through even without an
		// exporter.
		otel.SetTextMapPropagator(propagation.TraceContext{})
		return func(context.Context) error { return nil }, nil
	}
	// Accept full URLs or host:port.
	endpoint := otlpEndpoint
	var opts []otlptracehttp.Option
	if u, err := url.Parse(otlpEndpoint); err == nil && u.Scheme != "" {
		endpoint = u.Host
		if u.Path != "" && u.Path != "/" {
			opts = append(opts, otlptracehttp.WithURLPath(u.Path))
		}
		if u.Scheme == "http" {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
	} else if strings.HasPrefix(otlpEndpoint, "http://") {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	opts = append(opts, otlptracehttp.WithEndpoint(endpoint))
	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.1))),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	_ = serviceName
	return tp.Shutdown, nil
}

// TracingMiddleware extracts W3C traceparent headers, creates a
// span for the request, and adds tenant/user attributes when
// available on the request context.
func TracingMiddleware(next http.Handler) http.Handler {
	tracer := otel.Tracer("kmail.http")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := tracer.Start(ctx, r.Method+" "+r.URL.Path,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.target", r.URL.Path),
			),
		)
		defer span.End()
		if tid := TenantIDFrom(ctx); tid != "" {
			span.SetAttributes(attribute.String("kmail.tenant_id", tid))
		}
		if uid := KChatUserIDFrom(ctx); uid != "" {
			span.SetAttributes(attribute.String("kmail.user_id", uid))
		}
		// Record trace id on the response so downstream log
		// aggregators can correlate without re-deriving from
		// headers.
		if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
			w.Header().Set("X-Trace-Id", sc.TraceID().String())
		}
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sr, r.WithContext(ctx))
		span.SetAttributes(
			attribute.Int("http.status_code", sr.status),
			attribute.Int64("http.response_time_ms", time.Since(start).Milliseconds()),
		)
	})
}

// TraceIDFrom returns the W3C trace id on the context or empty
// string. Exported so the structured JSON logger can include it.
func TraceIDFrom(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}
