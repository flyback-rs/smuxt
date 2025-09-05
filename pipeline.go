package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/go-gst/go-glib/glib"
	"github.com/go-gst/go-gst/gst"
)

// Pipeline represents the main streaming pipeline.
type Pipeline struct {
	config          *Config
	logger          *slog.Logger
	pipeline        *gst.Pipeline
	failoverGroups  map[string]*FailoverGroup
	elementRegistry map[string]*FailoverGroup // Maps element names to their groups
	mainLoop        *glib.MainLoop
	mu              sync.RWMutex
	ctx             context.Context
	cancel          context.CancelFunc
	running         atomic.Bool
}

// NewPipeline creates a new streaming pipeline.
func NewPipeline(config *Config, logger *slog.Logger) (*Pipeline, error) {
	gst.Init(nil)

	ctx, cancel := context.WithCancel(context.Background())

	return &Pipeline{
		config:          config,
		logger:          logger,
		failoverGroups:  make(map[string]*FailoverGroup),
		elementRegistry: make(map[string]*FailoverGroup),
		ctx:             ctx,
		cancel:          cancel,
	}, nil
}

// Build constructs the GStreamer pipeline from configuration.
func (p *Pipeline) Build() error {
	var err error

	// Create main pipeline
	p.pipeline, err = gst.NewPipeline("streaming-pipeline")
	if err != nil {
		return fmt.Errorf("failed to create pipeline: %w", err)
	}

	// Build ingest components
	ingest, err := p.buildIngest()
	if err != nil {
		return fmt.Errorf("failed to build ingest: %w", err)
	}

	// Build output groups
	for groupName, groupConfig := range p.config.OutputGroups {
		group, err := p.buildOutputGroup(groupName, groupConfig)
		if err != nil {
			return fmt.Errorf("failed to build output group %q: %w", groupName, err)
		}

		// Connect ingest to group
		if err := p.connectIngestToGroup(ingest, group); err != nil {
			return fmt.Errorf("failed to connect ingest to group %q: %w", groupName, err)
		}

		p.failoverGroups[groupName] = group
	}

	// Set up dynamic pad handling
	p.setupDynamicPadHandling(ingest)

	// Set up error handling
	p.setupErrorHandling()

	return nil
}

// Run starts the pipeline and blocks until stopped.
func (p *Pipeline) Run() error {
	if !p.running.CompareAndSwap(false, true) {
		return errors.New("pipeline is already running")
	}
	defer p.running.Store(false)

	p.logger.Info("Starting pipeline",
		"source", p.config.Server.GetSRTListenURI(),
		"groups", len(p.config.OutputGroups))

	if err := p.pipeline.SetState(gst.StatePlaying); err != nil {
		return fmt.Errorf("failed to start pipeline: %w", err)
	}
	defer p.pipeline.SetState(gst.StateNull)

	p.mainLoop = glib.NewMainLoop(nil, false)

	// Run main loop in goroutine
	done := make(chan struct{})
	go func() {
		p.mainLoop.Run()
		close(done)
	}()

	// Wait for context cancellation or main loop exit
	select {
	case <-p.ctx.Done():
		p.logger.Info("Stopping pipeline due to context cancellation")
	case <-done:
		p.logger.Info("Pipeline main loop exited")
	}

	if p.mainLoop != nil {
		p.mainLoop.Quit()
	}

	return nil
}

// Stop gracefully stops the pipeline.
func (p *Pipeline) Stop() {
	p.cancel()
	if p.running.Load() && p.mainLoop != nil {
		p.mainLoop.Quit()
	}
}

// Cleanup releases all resources.
func (p *Pipeline) Cleanup() {
	if p.pipeline != nil {
		p.pipeline.SetState(gst.StateNull)
		p.pipeline = nil
	}
}

// GetStatus returns the current status of the pipeline.
func (p *Pipeline) GetStatus() PipelineStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()

	status := PipelineStatus{
		Running: p.running.Load(),
		Groups:  make(map[string]GroupStatus),
	}

	for name, group := range p.failoverGroups {
		status.Groups[name] = group.GetStatus()
	}

	return status
}

// PipelineStatus represents the current state of the pipeline.
type PipelineStatus struct {
	Running bool
	Groups  map[string]GroupStatus
}
