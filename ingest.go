package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/go-gst/go-gst/gst"
)

// IngestComponents holds the common ingest elements.
type IngestComponents struct {
	Source    *gst.Element
	Demux     *gst.Element
	VideoTee  *gst.Element
	AudioTees map[int]*gst.Element // Audio tees per track
}

// buildIngest creates the ingest components of the pipeline.
func (p *Pipeline) buildIngest() (*IngestComponents, error) {
	ingest := &IngestComponents{
		AudioTees: make(map[int]*gst.Element),
	}

	// Create SRT source
	source, err := createElement("srtsrc", "srt-source")
	if err != nil {
		return nil, err
	}

	uri := p.config.Server.GetSRTListenURI()
	if err := source.Set("uri", uri); err != nil {
		return nil, fmt.Errorf("failed to set SRT URI: %w", err)
	}

	// Optional: Set additional SRT parameters
	if err := source.Set("latency", 200); err != nil {
		p.logger.Warn("Failed to set SRT latency", "error", err)
	}

	// Create demuxer
	demux, err := createElement("tsdemux", "ts-demuxer")
	if err != nil {
		return nil, err
	}

	// Create video tee for distribution
	videoTee, err := createElement("tee", "video-tee")
	if err != nil {
		return nil, err
	}

	// Add elements to pipeline
	if err := p.pipeline.AddMany(source, demux, videoTee); err != nil {
		return nil, fmt.Errorf("failed to add ingest elements: %w", err)
	}

	// Link source to demux
	if err := source.Link(demux); err != nil {
		return nil, fmt.Errorf("failed to link source to demux: %w", err)
	}

	ingest.Source = source
	ingest.Demux = demux
	ingest.VideoTee = videoTee

	return ingest, nil
}

// setupDynamicPadHandling configures handlers for dynamically created pads.
func (p *Pipeline) setupDynamicPadHandling(ingest *IngestComponents) {
	ingest.Demux.Connect("pad-added", func(_ *gst.Element, pad *gst.Pad) {
		p.handleDemuxPadAdded(ingest, pad)
	})
}

// handleDemuxPadAdded handles new pads from the demuxer.
func (p *Pipeline) handleDemuxPadAdded(ingest *IngestComponents, pad *gst.Pad) {
	caps := pad.GetCurrentCaps()
	if caps == nil {
		p.logger.Warn("Pad has no caps", "pad", pad.GetName())
		return
	}

	capsStr := caps.String()
	p.logger.Debug("New demuxer pad", "pad", pad.GetName(), "caps", capsStr)

	switch {
	case strings.HasPrefix(capsStr, "video"):
		p.handleVideoPad(ingest, pad)

	case strings.HasPrefix(capsStr, "audio"):
		p.handleAudioPad(ingest, pad)

	default:
		p.logger.Debug("Ignoring pad with unsupported caps", "caps", capsStr)
	}
}

// handleVideoPad connects a video pad from the demuxer.
func (p *Pipeline) handleVideoPad(ingest *IngestComponents, pad *gst.Pad) {
	sinkPad := ingest.VideoTee.GetStaticPad("sink")
	if sinkPad.IsLinked() {
		p.logger.Warn("Video tee sink already linked")
		return
	}

	if result := pad.Link(sinkPad); result != gst.PadLinkOK {
		p.logger.Error("Failed to link video pad", "result", result)
		return
	}

	p.logger.Info("Connected video stream")
}

// handleAudioPad connects an audio pad from the demuxer.
func (p *Pipeline) handleAudioPad(ingest *IngestComponents, pad *gst.Pad) {
	padName := pad.GetName()

	// Extract track index from pad name (e.g., "audio_0101" -> 1)
	trackIdx := extractAudioTrackIndex(padName)
	if trackIdx < 0 {
		p.logger.Warn("Could not extract audio track index", "pad", padName)
		return
	}

	p.logger.Debug("Processing audio pad", "pad", padName, "extracted_track", trackIdx)

	// Create audio tee if needed
	audioTee, exists := ingest.AudioTees[trackIdx]
	if !exists {
		var err error
		audioTee, err = createElement("tee", fmt.Sprintf("audio-tee-%d", trackIdx))
		if err != nil {
			p.logger.Error("Failed to create audio tee", "track", trackIdx, "error", err)
			return
		}

		if err := p.pipeline.Add(audioTee); err != nil {
			p.logger.Error("Failed to add audio tee", "error", err)
			return
		}

		ingest.AudioTees[trackIdx] = audioTee
	}

	// Link pad to audio tee
	sinkPad := audioTee.GetStaticPad("sink")
	if !sinkPad.IsLinked() {
		if result := pad.Link(sinkPad); result != gst.PadLinkOK {
			p.logger.Error("Failed to link audio pad", "track", trackIdx, "result", result)
			return
		}
		p.logger.Info("Connected audio stream", "track", trackIdx)
	}

	// Connect audio tee to groups that need this track
	for name, group := range p.failoverGroups {
		if group.NeedsAudioTrack(trackIdx + 1) { // Convert to 1-based index
			p.logger.Debug("Connecting audio track to group", "track", trackIdx, "group", name)
			p.connectAudioToGroup(audioTee, trackIdx, group)
		}
	}
}

// extractAudioTrackIndex extracts the track index from a pad name.
// Handles various pad naming patterns like "audio_0", "audio_01", "audio_0101"
func extractAudioTrackIndex(padName string) int {
	// Try different common patterns for tsdemux audio pad names
	patterns := []string{
		"audio_", // Most common: audio_0, audio_1
		"audio-", // Alternative: audio-0, audio-1
	}

	for _, pattern := range patterns {
		if strings.HasPrefix(padName, pattern) {
			indexStr := strings.TrimPrefix(padName, pattern)

			// Handle hex format like "0101" -> convert to decimal track number
			if len(indexStr) >= 2 {
				// Try parsing as hex first (common in MPEG-TS)
				if hexVal, err := strconv.ParseInt(indexStr, 16, 32); err == nil {
					// Extract actual track number from MPEG-TS PID
					trackIdx := int(hexVal & 0xFF)      // Get lower byte as track index
					if trackIdx >= 0 && trackIdx < 64 { // Reasonable track limit
						return trackIdx
					}
				}
			}

			// Fallback to decimal parsing
			if idx, err := strconv.Atoi(indexStr); err == nil && idx >= 0 {
				return idx
			}
		}
	}

	return -1 // Unable to extract valid index
}
