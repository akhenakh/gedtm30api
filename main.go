// main.go
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/caarlos0/env/v11"
	grpcprom "github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"

	pb "github.com/akhenakh/gedtm30api/gen/go"
	"github.com/akhenakh/gedtm30api/geotiff"
)

const appName = "elevation-service"

//go:embed static
var staticFS embed.FS

var (
	grpcAPIServer     *grpc.Server
	grpcHealthServer  *grpc.Server
	httpMetricsServer *http.Server
	httpRestUIServer  *http.Server
	grpcMetrics       = grpcprom.NewServerMetrics(grpcprom.WithServerHandlingTimeHistogram(
		grpcprom.WithHistogramBuckets([]float64{0.01, 0.1, 0.3, 0.6, 1, 3, 6, 9}),
	))
)

// Config holds all configuration for the application, loaded from environment variables.
type Config struct {
	LogLevel          string `env:"LOG_LEVEL" envDefault:"INFO"`
	HTTPPort          int    `env:"HTTP_PORT" envDefault:"8080"`
	APIPort           int    `env:"API_PORT" envDefault:"9200"`
	HealthPort        int    `env:"HEALTH_PORT" envDefault:"6666"`
	HTTPMetricsPort   int    `env:"METRICS_PORT" envDefault:"8888"`
	CogSource         string `env:"COG_SOURCE" envDefault:"https://s3.opengeohub.org/global/edtm/gedtm_rf_m_30m_s_20060101_20151231_go_epsg.4326.3855_v20250611.tif"`
	CacheMaxSize      int64  `env:"CACHE_MAX_SIZE" envDefault:"1024"`
	CacheItemsToPrune uint32 `env:"CACHE_ITEMS_TO_PRUNE" envDefault:"100"`
}

type Server struct {
	pb.UnimplementedElevationServiceServer
	geo          *geotiff.GeoTIFF
	healthServer *health.Server
}

func main() {
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		fmt.Printf("failed to parse config: %+v\n", err)
		os.Exit(1)
	}

	logger := createLogger(cfg, appName)
	slog.SetDefault(logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(interrupt)

	g, ctx := errgroup.WithContext(ctx)

	geo, err := setupTIFFReader(cfg, logger)
	if err != nil {
		logger.Error("failed to initialize GeoTIFF reader, shutting down", "error", err)
		os.Exit(1)
	}

	healthServer := health.NewServer()

	// gRPC Health Server
	g.Go(func() error {
		return startHealthServer(logger, cfg, healthServer)
	})

	// HTTP Metrics Server (Prometheus)
	g.Go(func() error {
		return startMetricsServer(logger, cfg)
	})

	// gRPC API Server
	g.Go(func() error {
		return startGRPCAPIServer(logger, cfg, healthServer, geo)
	})

	// HTTP REST & Web UI Server
	g.Go(func() error {
		return startHTTPRestUIServer(logger, cfg, geo)
	})

	// Wait for termination signal or an error from one of the services
	select {
	case <-interrupt:
		slog.Warn("received termination signal, starting graceful shutdown")
		cancel()
	case <-ctx.Done():
		slog.Warn("context cancelled, starting graceful shutdown")
	}

	// Graceful Shutdown
	healthServer.Shutdown()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if httpMetricsServer != nil {
		if err := httpMetricsServer.Shutdown(shutdownCtx); err != nil {
			slog.Error("HTTP metrics server shutdown error", "error", err)
		}
	}
	if httpRestUIServer != nil {
		if err := httpRestUIServer.Shutdown(shutdownCtx); err != nil {
			slog.Error("HTTP REST/UI server shutdown error", "error", err)
		}
	}
	if grpcHealthServer != nil {
		grpcHealthServer.GracefulStop()
	}
	if grpcAPIServer != nil {
		grpcAPIServer.GracefulStop()
	}

	// Wait for all services in the errgroup to finish
	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("server group returned an error", "error", err)
		os.Exit(2)
	}
}

func startHealthServer(logger *slog.Logger, cfg Config, healthServer *health.Server) error {
	addr := fmt.Sprintf(":%d", cfg.HealthPort)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("gRPC Health server failed to listen: %w", err)
	}

	grpcHealthServer = grpc.NewServer()
	healthpb.RegisterHealthServer(grpcHealthServer, healthServer)
	logger.Info("gRPC health server listening", "address", addr)
	return grpcHealthServer.Serve(lis)
}

func startMetricsServer(logger *slog.Logger, cfg Config) error {
	addr := fmt.Sprintf(":%d", cfg.HTTPMetricsPort)
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	prometheus.MustRegister(grpcMetrics)

	httpMetricsServer = &http.Server{Addr: addr, Handler: mux}
	logger.Info("HTTP metrics server listening", "address", addr)

	if err := httpMetricsServer.ListenAndServe(); err != http.ErrServerClosed {
		return fmt.Errorf("HTTP metrics server failed: %w", err)
	}
	return nil
}

func startGRPCAPIServer(logger *slog.Logger, cfg Config, healthServer *health.Server, geo *geotiff.GeoTIFF) error {
	addr := fmt.Sprintf(":%d", cfg.APIPort)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("gRPC API server failed to listen: %w", err)
	}

	lopts := []logging.Option{logging.WithLogOnEvents(logging.StartCall, logging.FinishCall)}
	grpcAPIServer = grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			logging.UnaryServerInterceptor(
				InterceptorLogger(logger),
				lopts...),
			grpcMetrics.UnaryServerInterceptor(),
		),
	)

	s := &Server{
		geo:          geo,
		healthServer: healthServer,
	}

	pb.RegisterElevationServiceServer(grpcAPIServer, s)
	reflection.Register(grpcAPIServer) // Enable reflection for tools like grpcurl

	// Set initial health status
	healthServer.SetServingStatus(pb.ElevationService_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_SERVING)
	logger.Info("gRPC API server listening", "address", addr)
	return grpcAPIServer.Serve(lis)
}

func startHTTPRestUIServer(logger *slog.Logger, cfg Config, geo *geotiff.GeoTIFF) error {
	addr := fmt.Sprintf(":%d", cfg.HTTPPort)
	mux := http.NewServeMux()

	// Handle REST API endpoints
	mux.HandleFunc("/getElevation/", getElevationHandler(geo))
	mux.HandleFunc("/getProfile/", getProfileHandler(geo))

	// Handle embedded Web UI
	contentFS, err := fs.Sub(staticFS, "static")
	if err != nil {
		return fmt.Errorf("failed to create sub-filesystem for web UI: %w", err)
	}
	mux.Handle("/", http.FileServer(http.FS(contentFS)))

	httpRestUIServer = &http.Server{Addr: addr, Handler: mux}
	logger.Info("HTTP REST/UI server listening", "address", addr)

	if err := httpRestUIServer.ListenAndServe(); err != http.ErrServerClosed {
		return fmt.Errorf("HTTP REST/UI server failed: %w", err)
	}
	return nil
}

func (s *Server) GetElevation(ctx context.Context, req *pb.ElevationRequest) (*pb.ElevationResponse, error) {
	value, err := s.geo.AtCoord(req.Longitude, req.Latitude)
	if err != nil {
		if strings.Contains(err.Error(), "does not fall inside") {
			return nil, status.Errorf(codes.NotFound, "coordinates are outside the dataset bounds: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "failed to retrieve elevation: %v", err)
	}
	return &pb.ElevationResponse{Elevation: value}, nil
}

func (s *Server) GetProfile(ctx context.Context, req *pb.ProfileRequest) (*pb.ProfileResponse, error) {
	if len(req.Points) < 2 {
		return nil, status.Error(codes.InvalidArgument, "at least two points are required for a profile")
	}
	coords := make([][]float64, len(req.Points))
	for i, p := range req.Points {
		coords[i] = []float64{p.Latitude, p.Longitude}
	}
	profileResult, err := s.geo.Profile(coords)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to generate profile: %v", err)
	}
	responsePoints := make([]*pb.ElevationPoint, len(profileResult))
	for i, p := range profileResult {
		responsePoints[i] = &pb.ElevationPoint{Latitude: p[0], Longitude: p[1], Elevation: float32(p[2])}
	}
	return &pb.ProfileResponse{Points: responsePoints}, nil
}

func getElevationHandler(geo *geotiff.GeoTIFF) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pathParts := strings.Split(strings.TrimPrefix(r.URL.Path, "/getElevation/"), "/")
		if len(pathParts) != 2 {
			http.Error(w, "Invalid URL format", http.StatusBadRequest)
			return
		}
		lat, err := strconv.ParseFloat(pathParts[0], 64)
		if err != nil {
			http.Error(w, "Invalid latitude", http.StatusBadRequest)
			return
		}
		lng, err := strconv.ParseFloat(pathParts[1], 64)
		if err != nil {
			http.Error(w, "Invalid longitude", http.StatusBadRequest)
			return
		}
		value, err := geo.AtCoord(lng, lat)
		if err != nil {
			http.Error(w, fmt.Sprintf("Could not retrieve elevation: %v", err), http.StatusInternalServerError)
			return
		}
		response := map[string]interface{}{"latitude": lat, "longitude": lng, "elevation": value}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}

func getProfileHandler(geo *geotiff.GeoTIFF) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req [][]float64
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
		profile, err := geo.Profile(req)
		if err != nil {
			http.Error(w, fmt.Sprintf("Could not generate profile: %v", err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(profile)
	}
}

func setupTIFFReader(cfg Config, logger *slog.Logger) (*geotiff.GeoTIFF, error) {
	logger.Info("initializing GeoTIFF reader", "source", cfg.CogSource)
	var reader io.ReadSeeker
	if strings.HasPrefix(cfg.CogSource, "http") {
		r, err := geotiff.NewHTTPRangeReader(cfg.CogSource, nil) // Using default client
		if err != nil {
			return nil, fmt.Errorf("failed to create HTTP reader for COG: %w", err)
		}
		reader = r
	} else {
		file, err := os.Open(cfg.CogSource)
		if err != nil {
			return nil, fmt.Errorf("failed to open local COG file: %w", err)
		}
		reader = file
	}
	logger.Info("configuring tile cache", "max_size", cfg.CacheMaxSize, "items_to_prune", cfg.CacheItemsToPrune)
	return geotiff.Open(reader, cfg.CacheMaxSize, cfg.CacheItemsToPrune)
}

func createLogger(cfg Config, appName string) *slog.Logger {
	var programLevel slog.Level
	switch strings.ToUpper(cfg.LogLevel) {
	case "DEBUG":
		programLevel = slog.LevelDebug
	case "INFO":
		programLevel = slog.LevelInfo
	case "WARN":
		programLevel = slog.LevelWarn
	case "ERROR":
		programLevel = slog.LevelError
	default:
		programLevel = slog.LevelInfo
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level:     programLevel,
		AddSource: programLevel <= slog.LevelDebug,
	}).WithAttrs([]slog.Attr{slog.String("app", appName)})
	return slog.New(handler)
}

func InterceptorLogger(l *slog.Logger) logging.Logger {
	return logging.LoggerFunc(func(ctx context.Context, lvl logging.Level, msg string, fields ...any) {
		l.Log(ctx, slog.Level(lvl), msg, fields...)
	})
}
