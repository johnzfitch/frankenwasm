package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/johanjanssens/frankenwasm/phpext" // registers PHP extension via init()
	"github.com/johanjanssens/frankenwasm/wasm"

	"github.com/dunglas/frankenphp"
	"github.com/joho/godotenv"
	"github.com/lmittmann/tint"
)

func main() {
	// Load .env if present
	_ = godotenv.Load()

	// Set up logger
	logger := slog.New(tint.NewHandler(os.Stdout, &tint.Options{
		Level:      slog.LevelDebug,
		TimeFormat: time.Kitchen,
	}))
	slog.SetDefault(logger)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Discover .wasm files in plugins/ directory
	pluginDir := "plugins"
	if dir := os.Getenv("FRANKENWASM_PLUGIN_DIR"); dir != "" {
		pluginDir = dir
	}

	wasmFiles, err := filepath.Glob(filepath.Join(pluginDir, "*.wasm"))
	if err != nil {
		logger.Error("Failed to discover plugins", "error", err)
		os.Exit(1)
	}

	// Resolve compilation cache path — persistent across restarts
	cachePath := filepath.Join(os.TempDir(), "frankenwasm-cache")
	if cp := os.Getenv("FRANKENWASM_CACHE_PATH"); cp != "" {
		cachePath = cp
	}

	// Create plugin manager with parallel compilation workers
	manager, err := wasm.NewManager(
		wasm.WithLogger(logger.Handler()),
		wasm.WithCachePath(cachePath),
		wasm.WithCompilationWorkers(runtime.NumCPU()),
		wasm.WithSharedRuntime(),
	)
	if err != nil {
		logger.Error("Failed to create plugin manager", "error", err)
		os.Exit(1)
	}
	defer manager.Close(ctx)

	// Build name→path map and load all plugins in parallel
	pluginPaths := make(map[string]string, len(wasmFiles))
	for _, wasmFile := range wasmFiles {
		name := strings.TrimSuffix(filepath.Base(wasmFile), ".wasm")
		absPath, err := filepath.Abs(wasmFile)
		if err != nil {
			logger.Error("Failed to resolve plugin path", "file", wasmFile, "error", err)
			continue
		}
		pluginPaths[name] = absPath
	}

	if err := manager.LoadAll(ctx, pluginPaths); err != nil {
		logger.Error("Failed to load plugins", "error", err)
		os.Exit(1)
	}

	if !manager.IsLoaded() {
		logger.Warn("No plugins loaded", "dir", pluginDir)
	}

	// Resolve thread count — pool size matches PHP threads for zero-contention
	numThreads := 2
	if n, err := strconv.Atoi(os.Getenv("FRANKENWASM_THREADS")); err == nil && n > 0 {
		numThreads = n
	}

	// Create thread-aligned pool with lazy instantiation
	pool, err := wasm.NewPool(ctx, manager, numThreads)
	if err != nil {
		logger.Error("Failed to create plugin pool", "error", err)
		os.Exit(1)
	}
	defer pool.Close(ctx)

	// Optional: warm up all pool slots (eagerly instantiate all plugins)
	warmUp := os.Getenv("FRANKENWASM_WARMUP") != "0"
	if warmUp {
		start := time.Now()
		if err := pool.WarmUp(ctx); err != nil {
			logger.Warn("Pool warm-up had errors", "error", err)
		} else {
			logger.Info("Pool warmed up", "slots", numThreads, "duration", time.Since(start))
		}
	}

	// Resolve document root
	docRootDir := "examples"
	if dir := os.Getenv("FRANKENWASM_DOC_ROOT"); dir != "" {
		docRootDir = dir
	}
	docRoot, err := filepath.Abs(docRootDir)
	if err != nil {
		logger.Error("Failed to resolve document root", "error", err)
		os.Exit(1)
	}

	// Init FrankenPHP
	initOptions := []frankenphp.Option{
		frankenphp.WithNumThreads(numThreads),
		frankenphp.WithLogger(logger),
		frankenphp.WithPhpIni(map[string]string{
			"include_path": docRoot,
		}),
	}

	if err := frankenphp.Init(initOptions...); err != nil {
		logger.Error("Failed to initialize FrankenPHP", "error", err)
		os.Exit(1)
	}
	defer frankenphp.Shutdown()

	// Set up HTTP handler
	addr := ":8080"
	if port := os.Getenv("FRANKENWASM_PORT"); port != "" {
		addr = ":" + port
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Rewrite directory requests to index.php
		if r.URL.Path == "/" || strings.HasSuffix(r.URL.Path, "/") {
			r.URL.Path = r.URL.Path + "index.php"
		}

		// Get a registry from the pool (blocks if none available)
		registry, err := pool.Get(r.Context())
		if err != nil {
			logger.Error("Failed to get registry from pool", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		defer pool.Put(registry)

		// Add registry to context
		ctx := wasm.WithContext(r.Context(), registry)
		r = r.WithContext(ctx)

		// Create FrankenPHP request
		req, err := frankenphp.NewRequestWithContext(r,
			frankenphp.WithRequestResolvedDocumentRoot(docRoot),
			frankenphp.WithRequestLogger(logger),
		)
		if err != nil {
			logger.Error("Failed to create FrankenPHP request", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		if err := frankenphp.ServeHTTP(w, req); err != nil {
			logger.Error("Failed to serve PHP", "error", err)
		}
	})

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	// Start server in goroutine
	go func() {
		logger.Info("Starting FrankenWASM server",
			"addr", addr,
			"docroot", docRoot,
			"plugins", len(wasmFiles),
			"threads", numThreads,
			"cache", cachePath,
			"workers", runtime.NumCPU(),
		)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("Server error", "error", err)
			os.Exit(1)
		}
	}()

	// Wait for shutdown signal
	<-ctx.Done()
	logger.Info("Shutting down...")

	if err := server.Shutdown(context.Background()); err != nil {
		logger.Error("Failed to shutdown server", "error", err)
	}
}
