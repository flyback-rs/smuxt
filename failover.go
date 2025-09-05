package main

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/go-gst/go-gst/gst"
)

// OutputStream represents a single output stream.
type OutputStream struct {
	Name      string
	Config    OutputConfig
	Sink      *gst.Element
	Queue     *gst.Element
	IsHealthy bool
	LastError time.Time
	mu        sync.RWMutex
}

// FailoverGroup manages a group of output streams with failover capability.
type FailoverGroup struct {
	Name         string
	Type         OutputType
	Selector     *gst.Element
	Muxer        *gst.Element
	AudioMixer   *gst.Element
	Streams      []*OutputStream
	ActiveStream *OutputStream
	AudioTracks  []int
	logger       *slog.Logger
	mu           sync.RWMutex
}

// buildOutputGroup creates a failover group from configuration.
func (p *Pipeline) buildOutputGroup(name string, config GroupConfig) (*FailoverGroup, error) {
	if len(config.Outputs) == 0 {
		return nil, errors.New("no outputs defined")
	}

	group := &FailoverGroup{
		Name:        name,
		Type:        config.Outputs[0].Type,
		Streams:     make([]*OutputStream, 0, len(config.Outputs)),
		AudioTracks: config.Outputs[0].AudioTracks,
		logger:      p.logger.With("group", name),
	}

	// Create muxer based on output type
	muxer, err := p.createMuxer(group.Type)
	if err != nil {
		return nil, fmt.Errorf("failed to create muxer: %w", err)
	}
	group.Muxer = muxer

	// Create audio mixer if multiple audio tracks are needed
	if len(group.AudioTracks) > 1 {
		mixer, err := p.createAudioMixer()
		if err != nil {
			return nil, fmt.Errorf("failed to create audio mixer: %w", err)
		}
		group.AudioMixer = mixer

		// Link mixer to muxer
		if err := mixer.Link(muxer); err != nil {
			return nil, fmt.Errorf("failed to link audio mixer to muxer: %w", err)
		}
	}

	// Create output selector for failover (if more than one output)
	if len(config.Outputs) > 1 {
		selector, err := createElement("output-selector", fmt.Sprintf("selector-%s", name))
		if err != nil {
			return nil, err
		}

		if err := p.pipeline.Add(selector); err != nil {
			return nil, fmt.Errorf("failed to add selector: %w", err)
		}

		if err := muxer.Link(selector); err != nil {
			return nil, fmt.Errorf("failed to link muxer to selector: %w", err)
		}

		group.Selector = selector
	}

	// Create output streams
	for i, outputConfig := range config.Outputs {
		stream, err := p.createOutputStream(outputConfig, i)
		if err != nil {
			return nil, fmt.Errorf("failed to create output stream %d: %w", i, err)
		}

		// Connect stream to selector or directly to muxer
		if group.Selector != nil {
			if err := p.connectStreamToSelector(stream, group.Selector); err != nil {
				return nil, fmt.Errorf("failed to connect stream to selector: %w", err)
			}
		} else {
			if err := muxer.Link(stream.Queue); err != nil {
				return nil, fmt.Errorf("failed to link muxer to stream: %w", err)
			}
		}

		group.Streams = append(group.Streams, stream)

		// Register sink in element registry for error handling
		p.elementRegistry[stream.Sink.GetName()] = group
	}

	// Set first stream as active
	if len(group.Streams) > 0 {
		group.SetActiveStream(group.Streams[0])
		p.logger.Info("Set initial active stream",
			"group", group.Name,
			"stream", group.Streams[0].Name,
			"index", 0)
	}

	return group, nil
}

// createMuxer creates the appropriate muxer for the output type.
func (p *Pipeline) createMuxer(outputType OutputType) (*gst.Element, error) {
	var muxer *gst.Element
	var err error

	switch outputType {
	case OutputTypeRTMP:
		muxer, err = createElement("flvmux", "")
		if err != nil {
			return nil, err
		}
		muxer.Set("streamable", true)
		muxer.Set("latency", uint64(100*time.Millisecond))

	case OutputTypeSRT:
		muxer, err = createElement("mpegtsmux", "")
		if err != nil {
			return nil, err
		}
		muxer.Set("alignment", 7) // Align on packet boundaries

	default:
		return nil, fmt.Errorf("unsupported output type: %s", outputType)
	}

	if err := p.pipeline.Add(muxer); err != nil {
		return nil, fmt.Errorf("failed to add muxer to pipeline: %w", err)
	}

	return muxer, nil
}

// createAudioMixer creates an audio mixing pipeline.
func (p *Pipeline) createAudioMixer() (*gst.Element, error) {
	// Create bin for audio processing
	bin := gst.NewBin("audio-mixer-bin")

	// Create elements
	mixer, err := createElement("audiomixer", "")
	if err != nil {
		return nil, err
	}

	convert, err := createElement("audioconvert", "")
	if err != nil {
		return nil, err
	}

	resample, err := createElement("audioresample", "")
	if err != nil {
		return nil, err
	}

	encoder, err := createElement("fdkaacenc", "")
	if err != nil {
		return nil, err
	}
	encoder.Set("bitrate", 128000)

	parser, err := createElement("aacparse", "")
	if err != nil {
		return nil, err
	}

	// Add to bin and link
	if err := bin.AddMany(mixer, convert, resample, encoder, parser); err != nil {
		return nil, fmt.Errorf("failed to add audio elements: %w", err)
	}

	if err := gst.ElementLinkMany(mixer, convert, resample, encoder, parser); err != nil {
		return nil, fmt.Errorf("failed to link audio elements: %w", err)
	}

	// Create ghost pads
	mixerPad := mixer.GetRequestPad("sink_%u")
	bin.AddPad(gst.NewGhostPad("sink", mixerPad).Pad)

	parserPad := parser.GetStaticPad("src")
	bin.AddPad(gst.NewGhostPad("src", parserPad).Pad)

	if err := p.pipeline.Add(bin.Element); err != nil {
		return nil, fmt.Errorf("failed to add audio bin: %w", err)
	}

	return bin.Element, nil
}

// createOutputStream creates a single output stream.
func (p *Pipeline) createOutputStream(config OutputConfig, index int) (*OutputStream, error) {
	streamName := fmt.Sprintf("%s-%d", config.Name, index)

	stream := &OutputStream{
		Name:      streamName,
		Config:    config,
		IsHealthy: true,
	}

	// Create queue for buffering
	queue, err := createElement("queue2", fmt.Sprintf("queue-%s", config.Name))
	if err != nil {
		return nil, err
	}
	queue.Set("max-size-bytes", uint(10*1024*1024))   // 10MB buffer
	queue.Set("max-size-time", uint64(2*time.Second)) // 2 second buffer

	// Create sink based on type
	switch config.Type {
	case OutputTypeRTMP:
		sink, err := createElement("rtmp2sink", fmt.Sprintf("rtmp-%s", config.Name))
		if err != nil {
			return nil, err
		}
		sink.Set("location", config.URL)
		sink.Set("sync", false)
		stream.Sink = sink

	case OutputTypeSRT:
		sink, err := createElement("srtsink", fmt.Sprintf("srt-%s", config.Name))
		if err != nil {
			return nil, err
		}
		sink.Set("uri", config.URL)
		sink.Set("sync", false)
		sink.Set("latency", 125) // ms
		stream.Sink = sink

	default:
		return nil, fmt.Errorf("unsupported output type: %s", config.Type)
	}

	// Add elements to pipeline
	if err := p.pipeline.AddMany(queue, stream.Sink); err != nil {
		return nil, fmt.Errorf("failed to add stream elements: %w", err)
	}

	// Link queue to sink
	if err := queue.Link(stream.Sink); err != nil {
		return nil, fmt.Errorf("failed to link queue to sink: %w", err)
	}

	stream.Queue = queue

	return stream, nil
}

// connectStreamToSelector connects a stream to the output selector.
func (p *Pipeline) connectStreamToSelector(stream *OutputStream, selector *gst.Element) error {
	srcPad := selector.GetRequestPad("src_%u")
	if srcPad == nil {
		return errors.New("failed to get selector src pad")
	}

	sinkPad := stream.Queue.GetStaticPad("sink")
	if result := srcPad.Link(sinkPad); result != gst.PadLinkOK {
		return fmt.Errorf("failed to link selector to stream: %v", result)
	}

	return nil
}

// SetActiveStream switches the active stream in a failover group.
func (g *FailoverGroup) SetActiveStream(stream *OutputStream) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.Selector != nil && stream != nil {
		sinkPad := stream.Queue.GetStaticPad("sink")
		if sinkPad != nil {
			peerPad := sinkPad.GetPeer()
			if peerPad != nil {
				g.Selector.Set("active-pad", peerPad)
				g.logger.Info("Switched active stream", "stream", stream.Name)
			}
		}
	}

	g.ActiveStream = stream
}

// HandleStreamError handles errors from a stream and performs failover if needed.
func (g *FailoverGroup) HandleStreamError(erroredStream *OutputStream) {
	g.mu.Lock()
	defer g.mu.Unlock()

	erroredStream.mu.Lock()
	erroredStream.IsHealthy = false
	erroredStream.LastError = time.Now()
	erroredStream.mu.Unlock()

	// If this is not the active stream, no action needed
	if g.ActiveStream != erroredStream {
		g.logger.Debug("Error on backup stream", "stream", erroredStream.Name)
		return
	}

	g.logger.Warn("Active stream failed, attempting failover", "failed_stream", erroredStream.Name)

	// Find next healthy stream
	for _, stream := range g.Streams {
		stream.mu.RLock()
		isHealthy := stream.IsHealthy
		stream.mu.RUnlock()

		if stream != erroredStream && isHealthy {
			g.SetActiveStream(stream)
			g.logger.Info("Failover successful", "new_stream", stream.Name)

			// Schedule recovery check for failed stream
			go g.scheduleRecoveryCheck(erroredStream)
			return
		}
	}

	g.logger.Error("No healthy backup streams available")
	g.ActiveStream = nil
}

// scheduleRecoveryCheck attempts to recover a failed stream after a delay.
func (g *FailoverGroup) scheduleRecoveryCheck(stream *OutputStream) {
	time.Sleep(30 * time.Second)

	// Try to recover the stream
	stream.mu.Lock()
	stream.IsHealthy = true
	stream.mu.Unlock()

	g.logger.Info("Marked stream as recovered", "stream", stream.Name)
}

// NeedsAudioTrack checks if this group needs a specific audio track.
func (g *FailoverGroup) NeedsAudioTrack(trackNum int) bool {
	for _, track := range g.AudioTracks {
		if track == trackNum {
			return true
		}
	}
	return false
}

// GetStatus returns the current status of the failover group.
func (g *FailoverGroup) GetStatus() GroupStatus {
	g.mu.RLock()
	defer g.mu.RUnlock()

	status := GroupStatus{
		Name:         g.Name,
		Type:         string(g.Type),
		StreamCount:  len(g.Streams),
		ActiveStream: "",
		Streams:      make([]StreamStatus, 0, len(g.Streams)),
	}

	if g.ActiveStream != nil {
		status.ActiveStream = g.ActiveStream.Name
	}

	for _, stream := range g.Streams {
		stream.mu.RLock()
		streamStatus := StreamStatus{
			Name:      stream.Name,
			URL:       stream.Config.URL,
			IsHealthy: stream.IsHealthy,
			IsActive:  stream == g.ActiveStream,
		}
		if !stream.IsHealthy {
			streamStatus.LastError = stream.LastError
		}
		stream.mu.RUnlock()

		status.Streams = append(status.Streams, streamStatus)
	}

	return status
}

// GroupStatus represents the status of a failover group.
type GroupStatus struct {
	Name         string
	Type         string
	StreamCount  int
	ActiveStream string
	Streams      []StreamStatus
}

// StreamStatus represents the status of an individual stream.
type StreamStatus struct {
	Name      string
	URL       string
	IsHealthy bool
	IsActive  bool
	LastError time.Time
}
