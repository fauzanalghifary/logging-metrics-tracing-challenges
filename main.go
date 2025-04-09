package main

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/rs/zerolog/log"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/prometheus"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.10.0"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
)

var meter otelmetric.Meter
var tracer oteltrace.Tracer
var reqHistogram otelmetric.Float64Histogram

func main() {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	signal.Notify(ch, syscall.SIGTERM)
	go func() {
		oscall := <-ch
		log.Warn().Msgf("system call:%+v", oscall)
		cancel()
	}()

	res, err := resource.New(
		ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String("logging-challenge"),
		),
	)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create resource")
	}

	// set up the OpenTelemetry Prometheus exporter
	exporter, err := prometheus.New()
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create prometheus exporter")
	}

	// set up the OpenTelemetry meter provider
	meterProvider := metric.NewMeterProvider(
		metric.WithResource(res),
		metric.WithReader(exporter),
	)
	otel.SetMeterProvider(meterProvider)

	meter = otel.Meter("logging-challenge")
	reqHistogram, err = meter.Float64Histogram(
		"http_request_duration_milliseconds",
		otelmetric.WithUnit("milliseconds"),
		otelmetric.WithDescription("Histogram of HTTP request duration in milliseconds"),
		otelmetric.WithExplicitBucketBoundaries(
			[]float64{
				0,
				100,
				200,
				300,
				400,
				500,
				600,
				700,
				800,
				900,
				1000,
			}...,
		),
	)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create histogram")
	}

	tc := otlptracegrpc.NewClient(
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithEndpoint("localhost:4317"),
		otlptracegrpc.WithDialOption(grpc.WithBlock()),
	)

	traceExporter, err := otlptrace.New(ctx, tc)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create trace exporter")
	}

	bsp := trace.NewBatchSpanProcessor(traceExporter)
	tracerProvider := trace.NewTracerProvider(
		trace.WithResource(res),
		trace.WithSpanProcessor(bsp),
	)
	otel.SetTracerProvider(tracerProvider)

	tracer = otel.Tracer("logging-challenge")

	r := mux.NewRouter()
	r.Use(middleware)
	r.Handle("/", otelhttp.WithRouteTag("/", http.HandlerFunc(handler)))
	r.Handle("/metrics", otelhttp.WithRouteTag("/metrics", promhttp.Handler()))

	// start: set up any of your logger configuration here if necessary
	zerolog.SetGlobalLevel(zerolog.DebugLevel)

	f, err := os.OpenFile("logs/app.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to open log file")
	}
	defer f.Close()

	multi := zerolog.MultiLevelWriter(os.Stdout, f)
	log.Logger = zerolog.New(multi).With().Timestamp().Logger()

	log.Info().Msg("starting http server")

	// end: set up any of your logger configuration here

	server := &http.Server{
		Addr:    ":8080",
		Handler: otelhttp.NewHandler(r, "/"),
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("failed to listen and serve http server")
		}
	}()
	<-ctx.Done()

	if err := server.Shutdown(context.Background()); err != nil {
		log.Error().Err(err).Msg("failed to shutdown http server gracefully")
	}
}

func middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			log := log.Logger.With().
				Str("request_id", uuid.New().String()).
				Str("method", r.Method).
				Str("url", r.URL.String()).
				Logger()
			ctx := log.WithContext(r.Context())

			// calculate time elapsed
			start := time.Now()
			next.ServeHTTP(w, r.WithContext(ctx))
			elapsed := time.Since(start)
			elapsed_ms := float64(elapsed.Nanoseconds()) / 1e6

			log.Info().
				Float64("elapsed", float64(elapsed.Nanoseconds())/1e6).
				Msg("request completed")
			reqHistogram.Record(
				ctx,
				elapsed_ms,
				otelmetric.WithAttributes(
					attribute.String("url", r.URL.String()),
					attribute.String("method", r.Method),
				),
			)

		},
	)
}

func handler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	ctx, span := tracer.Start(
		ctx, "handler", oteltrace.WithAttributes(
			attribute.String("user_id", "foo"),
		),
	)
	defer span.End()

	log := log.Ctx(ctx).With().Str("func", "handler").Logger()
	name := r.URL.Query().Get("name")

	span.SetAttributes(
		attribute.String("name", name),
	)

	log.Debug().
		Str("name", name).
		Msg("processing handler")

	res, err := greeting(ctx, name)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		log.Error().Err(err).Msg("failed to process request")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	span.AddEvent(
		"greeting function is executed",
		oteltrace.WithAttributes(
			attribute.String("result", "ok"),
		),
	)

	log.Info().Msg("processed request successfully")

	w.Write([]byte(res))
}

func greeting(ctx context.Context, name string) (string, error) {
	_, span := tracer.Start(ctx, "greeting")
	defer span.End()

	log := log.Ctx(ctx).With().Str("func", "greeting").Logger()
	if len(name) < 5 {
		log.Warn().Msgf("name is too short %s", name)
		return "", fmt.Errorf("Hello %s! Your name is to short\n", name)
	}

	log.Debug().Msgf("name is long enough: %s", name)
	return fmt.Sprintf("Hi %s", name), nil
}
