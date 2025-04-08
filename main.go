package main

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog/log"
)

var (
	reqCountProcessed = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_request_count",
			Help: "The total number of processed by handler",
		}, []string{"method", "endpoint"},
	)
)

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

	r := mux.NewRouter()
	r.Use(middleware)
	r.HandleFunc("/", handler)
	r.Handle("/metrics", promhttp.Handler())

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
		Handler: r,
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

			next.ServeHTTP(w, r.WithContext(ctx))
		},
	)
}

func handler(w http.ResponseWriter, r *http.Request) {
	reqCountProcessed.With(prometheus.Labels{"method": r.Method, "endpoint": r.URL.Path}).Inc()

	ctx := r.Context()
	log := log.Ctx(ctx).With().Str("func", "handler").Logger()
	name := r.URL.Query().Get("name")

	log.Debug().
		Str("name", name).
		Msg("processing handler")

	res, err := greeting(ctx, name)
	if err != nil {
		log.Error().Err(err).Msg("failed to process request")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Info().Msg("processed request successfully")

	w.Write([]byte(res))
}

func greeting(ctx context.Context, name string) (string, error) {
	log := log.Ctx(ctx).With().Str("func", "greeting").Logger()
	if len(name) < 5 {
		log.Warn().Msgf("name is too short %s", name)
		return fmt.Sprintf("Hello %s! Your name is to short\n", name), nil
	}

	log.Debug().Msgf("name is long enough: %s", name)
	return fmt.Sprintf("Hi %s", name), nil
}
