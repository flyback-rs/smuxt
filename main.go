package main

import (
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	var configPath string
	var verbose bool

	flag.StringVar(&configPath, "config", "config.toml", "Path to configuration file")
	flag.BoolVar(&verbose, "verbose", false, "Enable verbose logging")
	flag.Parse()

	// Setup logging
	logLevel := slog.LevelInfo
	if verbose {
		logLevel = slog.LevelDebug
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))

	// Load configuration
	config, err := LoadConfig(configPath)
	if err != nil {
		logger.Error("Failed to load configuration", "error", err)
		os.Exit(1)
	}

	logger.Info("Configuration loaded",
		"title", config.Title,
		"groups", len(config.OutputGroups))

	// Create pipeline
	pipeline, err := NewPipeline(config, logger)
	if err != nil {
		logger.Error("Failed to create pipeline", "error", err)
		os.Exit(1)
	}
	defer pipeline.Cleanup()

	// Build pipeline
	if err := pipeline.Build(); err != nil {
		logger.Error("Failed to build pipeline", "error", err)
		os.Exit(1)
	}

	// Setup signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		logger.Info("Received signal, shutting down", "signal", sig)
		pipeline.Stop()
	}()

	// Run pipeline
	logger.Info("Starting streaming pipeline")
	if err := pipeline.Run(); err != nil {
		logger.Error("Pipeline error", "error", err)
		os.Exit(1)
	}

	logger.Info("Pipeline stopped gracefully")
}
