package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/time/rate"
)

const (
	defaultScytaleURL      = "http://scytale:6300/api/v2/device"
	defaultScytaleAuth     = "dXNlcjpwYXNz"
	defaultAddr            = ":4900"
	defaultTimeout         = 15
	defaultLogLevel        = "info"
	defaultRateLimit       = 100 // requests per second
	defaultRateLimitBurst  = 200
	defaultShutdownTimeout = 5 // seconds
)

var (
	// Prometheus metrics
	requestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "Histogram of request latencies",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"handler", "method", "status", "path"},
	)
	requestCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests",
		},
		[]string{"handler", "method", "status", "path"},
	)
)

func init() {
	prometheus.MustRegister(requestDuration, requestCounter)
}

func main() {
	// Setup configuration with viper
	viper.SetDefault("scytale_url", defaultScytaleURL)
	viper.SetDefault("address", defaultAddr)
	viper.SetDefault("timeout", defaultTimeout)
	viper.SetDefault("scytale_auth", defaultScytaleAuth)
	viper.SetDefault("log_level", defaultLogLevel)
	viper.SetDefault("rate_limit", defaultRateLimit)
	viper.SetDefault("rate_limit_burst", defaultRateLimitBurst)
	viper.SetDefault("shutdown_timeout", defaultShutdownTimeout)

	viper.SetEnvPrefix("SCYTALE2PARODUS")
	viper.AutomaticEnv()
	viper.SetConfigName("config")
	viper.AddConfigPath(".")
	viper.AddConfigPath("/etc/scytale2parodus")
	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			fmt.Fprintf(os.Stderr, "Failed to read config: %v\n", err)
			os.Exit(1)
		}
	}

	// Define flags
	scytaleURL := flag.String("scytale-url", viper.GetString("scytale_url"), "Scytale service URL")
	address := flag.String("address", viper.GetString("address"), "Address to bind the server")
	timeout := flag.Int("timeout", viper.GetInt("timeout"), "Server read and write timeout in seconds")
	scytaleAuth := flag.String("scytale-auth", viper.GetString("scytale_auth"), "Scytale basic auth (base64 encoded user:pass)")
	logLevel := flag.String("log-level", viper.GetString("log_level"), "Log level (debug, info, warn, error)")
	rateLimit := flag.Float64("rate-limit", viper.GetFloat64("rate_limit"), "Requests per second for rate limiting")
	rateLimitBurst := flag.Int("rate-limit-burst", viper.GetInt("rate_limit_burst"), "Burst size for rate limiting")

	flag.Parse()

	// Update viper with flag values
	viper.Set("scytale_url", *scytaleURL)
	viper.Set("address", *address)
	viper.Set("timeout", *timeout)
	viper.Set("scytale_auth", *scytaleAuth)
	viper.Set("log_level", *logLevel)
	viper.Set("rate_limit", *rateLimit)
	viper.Set("rate_limit_burst", *rateLimitBurst)

	// Setup logger
	zapLevel, err := zapcore.ParseLevel(viper.GetString("log_level"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid log level: %v\n", err)
		os.Exit(1)
	}
	config := zap.NewProductionConfig()
	config.Level = zap.NewAtomicLevelAt(zapLevel)
	config.EncoderConfig.TimeKey = "time"
	config.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	logger, err := config.Build()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	// Setup router
	router := mux.NewRouter()

	// Rate limiting middleware
	limiter := rate.NewLimiter(rate.Limit(viper.GetFloat64("rate_limit")), viper.GetInt("rate_limit_burst"))
	rateLimitMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := limiter.Wait(r.Context()); err != nil {
				if r.Context().Err() != nil {
					return // Client disconnected
				}
				logger.Error("Rate limit exceeded",
					zap.String("path", r.URL.Path),
					zap.Error(err))
				http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}

	// Metrics middleware
	metricsMiddleware := func(handlerName string, next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
			next.ServeHTTP(rw, r)
			if rand.Float64() < 0.1 { // 10% sampling
				duration := time.Since(start).Seconds()
				labels := []string{handlerName, r.Method, fmt.Sprint(rw.statusCode), r.URL.Path}
				requestDuration.WithLabelValues(labels...).Observe(duration)
				requestCounter.WithLabelValues(labels...).Inc()
			}
		})
	}

	// Initialize DeviceServer
	ubusServ, err := NewDeviceServer(logger, viper.GetString("scytale_url"), viper.GetString("scytale_auth"))
	if err != nil {
		logger.Error("Failed to initialize DeviceServer", zap.Error(err))
		os.Exit(1)
	}

	// Routes
	router.Handle("/api/v1/{deviceID}/send/{service}", metricsMiddleware("send", rateLimitMiddleware(http.HandlerFunc(ubusServ.PostCallHandler)))).Methods("POST")
	router.HandleFunc("/health", ubusServ.HealthCheckHandler).Methods("GET")
	router.Handle("/metrics", promhttp.Handler())

	// Setup server
	srv := &http.Server{
		Handler:      router,
		Addr:         viper.GetString("address"),
		WriteTimeout: time.Duration(viper.GetInt("timeout")) * time.Second,
		ReadTimeout:  time.Duration(viper.GetInt("timeout")) * time.Second,
	}

	// Graceful shutdown
	stop := make(chan os.Signal, 1)
	errCh := make(chan error, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		logger.Info("Starting server", zap.String("address", srv.Addr))
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-stop:
		logger.Info("Shutting down server...")
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(viper.GetInt("shutdown_timeout"))*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logger.Error("Server shutdown failed", zap.Error(err))
			os.Exit(1)
		}
		logger.Info("Server stopped")
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			logger.Error("Server failed", zap.Error(err))
			os.Exit(1)
		}
	}
}

// responseWriter wraps http.ResponseWriter to capture status code for metrics
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if rw.statusCode == 0 {
		rw.WriteHeader(http.StatusOK)
	}
	return rw.ResponseWriter.Write(b)
}
