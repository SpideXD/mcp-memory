package main

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
)

// Set via -ldflags: go build -ldflags "-X main.Version=v2.1.0 -X main.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
var (
	Version   = "dev"
	BuildTime = "unknown"
)

func main() {
	// Ensure we're in the correct working directory
	// go run . compiles to a temp dir, so we need to detect the actual cwd
	if wd, err := os.Getwd(); err == nil {
		os.MkdirAll(filepath.Join(wd, "logs"), 0755)
	}
	loadEnv()
	cleanupOrphans()  // Kill orphans from previous crash
	config := LoadConfig()
	if err := config.Validate(); err != nil {
		println("mcp-memory: invalid config:", err.Error())
		os.Exit(1)
	}

	// Check alert endpoint if configured
	alerts := NewAlertClient(config.AlertURL, config.AlertMode)
	if alerts != nil {
		if err := alerts.CheckHealth(); err != nil {
			if alerts.IsRequired() {
				println("mcp-memory: alert endpoint required but not reachable:", err.Error())
				os.Exit(1)
			}
			println("mcp-memory: alert endpoint not reachable (optional mode, continuing):", err.Error())
		}
	}
	srv := NewServer(config)

	// Phase 1: Start internal services (llama.cpp, Hindsight, workers, health monitor)
	println("mcp-memory: starting services...")
	if err := srv.Start(); err != nil {
		// Log to stderr before dying — logger isn't ready yet
		println("mcp-memory: failed to start services:", err.Error())
		os.Exit(1)
	}
	srv.log.Info("server started")

	// Log build info so we can verify the running binary matches the source
	buildInfo, _ := debug.ReadBuildInfo()
	srv.log.Info("build info",
		"version", Version,
		"built", BuildTime,
		"go", buildInfo.GoVersion,
		"path", buildInfo.Path,
		"main", buildInfo.Main.Version,
	)

	// Phase 2: Start HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/health", srv.handleHealth)
	mux.HandleFunc("/start", srv.handleStart)
	mux.HandleFunc("/stop", srv.handleStop)
	mux.HandleFunc("/mcp/sse", srv.handleMCPSSE)
	mux.HandleFunc("/mcp/message", srv.handleMCPMessage)

	addr := config.Host + ":" + config.Port
	httpSrv := &http.Server{
		Addr:           addr,
		Handler:        mux,
		ReadTimeout:    srv.config.HTTPReadTimeout,
		WriteTimeout:   0, // SSE
		IdleTimeout:    srv.config.HTTPIdleTimeout,
		MaxHeaderBytes: int(srv.config.MaxBodyBytes),
	}

	// Phase 3: Handle shutdown — stop HTTP first, then drain services
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		println("[MCP] shutting down...")
		srv.log.Info("shutdown signal received")

		// Stop HTTP server first (stop accepting new connections)
		ctx, cancel := context.WithTimeout(context.Background(), srv.config.ShutdownTimeout)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			srv.log.Error("http shutdown error", "error", err.Error())
		}

		// Drain workers, close sessions, stop services
		srv.Stop()

		srv.log.Info("shutdown complete")
		os.Exit(0)
	}()

	srv.log.Info("http server started", "addr", addr)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		srv.log.Error("http server failed", "error", err.Error())
		os.Exit(1)
	}
}

// loadEnv reads .env file and sets environment variables.
// Does NOT overwrite already-set environment variables.
func loadEnv() {
	envFile := ".env"
	if _, err := os.Stat(envFile); os.IsNotExist(err) {
		return
	}
	f, err := os.Open(envFile)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		// Strip surrounding quotes
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[0] == value[len(value)-1] {
			value = value[1 : len(value)-1]
		}
		// Handle common escape sequences
		value = strings.ReplaceAll(value, "\\n", "\n")
		value = strings.ReplaceAll(value, "\\t", "\t")
		value = strings.ReplaceAll(value, "\\\\", "\\")
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, value)
		}
	}
}
