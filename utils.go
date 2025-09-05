// utils.go
package main

import (
	"errors"
	"fmt"

	"github.com/go-gst/go-gst/gst"
)

// createElement creates a new GStreamer element with an optional name.
func createElement(factoryName, elementName string) (*gst.Element, error) {
	var elem *gst.Element
	var err error

	if elementName != "" {
		elem, err = gst.NewElementWithName(factoryName, elementName)
	} else {
		elem, err = gst.NewElement(factoryName)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to create %s element: %w", factoryName, err)
	}

	if elem == nil {
		return nil, fmt.Errorf("%s element is nil", factoryName)
	}

	return elem, nil
}

// connectIngestToGroup connects ingest components to an output group.
func (p *Pipeline) connectIngestToGroup(ingest *IngestComponents, group *FailoverGroup) error {
	// Connect video tee to group muxer
	videoQueue, err := createElement("queue", fmt.Sprintf("video-queue-%s", group.Name))
	if err != nil {
		return err
	}

	if err := p.pipeline.Add(videoQueue); err != nil {
		return fmt.Errorf("failed to add video queue: %w", err)
	}

	// Get tee src pad
	teeSrcPad := ingest.VideoTee.GetRequestPad("src_%u")
	if teeSrcPad == nil {
		return errors.New("failed to get tee src pad")
	}

	// Link tee to queue
	queueSinkPad := videoQueue.GetStaticPad("sink")
	if result := teeSrcPad.Link(queueSinkPad); result != gst.PadLinkOK {
		return fmt.Errorf("failed to link tee to queue: %v", result)
	}

	// Link queue to muxer
	if err := videoQueue.Link(group.Muxer); err != nil {
		return fmt.Errorf("failed to link video queue to muxer: %w", err)
	}

	return nil
}

// connectAudioToGroup connects an audio tee to a group that needs it.
func (p *Pipeline) connectAudioToGroup(audioTee *gst.Element, trackIdx int, group *FailoverGroup) {
	audioQueue, err := createElement("queue", fmt.Sprintf("audio-queue-%s-%d", group.Name, trackIdx))
	if err != nil {
		p.logger.Error("Failed to create audio queue", "error", err)
		return
	}

	if err := p.pipeline.Add(audioQueue); err != nil {
		p.logger.Error("Failed to add audio queue", "error", err)
		return
	}

	// Get tee src pad
	teeSrcPad := audioTee.GetRequestPad("src_%u")
	if teeSrcPad == nil {
		p.logger.Error("Failed to get audio tee src pad")
		return
	}

	// Link tee to queue
	queueSinkPad := audioQueue.GetStaticPad("sink")
	if result := teeSrcPad.Link(queueSinkPad); result != gst.PadLinkOK {
		p.logger.Error("Failed to link audio tee to queue", "result", result)
		return
	}

	// Link to mixer or directly to muxer
	var target *gst.Element
	if group.AudioMixer != nil {
		target = group.AudioMixer
	} else {
		target = group.Muxer
	}

	if err := audioQueue.Link(target); err != nil {
		p.logger.Error("Failed to link audio queue", "error", err)
		return
	}

	p.logger.Debug("Connected audio track to group", "track", trackIdx, "group", group.Name)
}

// setupErrorHandling configures the pipeline's error handling.
func (p *Pipeline) setupErrorHandling() {
	bus := p.pipeline.GetBus()
	bus.AddSignalWatch()

	// Handle errors
	bus.Connect("message::error", func(_ *gst.Bus, msg *gst.Message) {
		err := msg.ParseError()
		elementName := msg.Source()

		p.logger.Error("Pipeline error",
			"element", elementName,
			"error", err.Error(),
			"debug", err.DebugString())

		// Check if this is from a registered output sink
		p.mu.RLock()
		group, exists := p.elementRegistry[elementName]
		p.mu.RUnlock()

		if exists {
			// Find the specific stream
			for _, stream := range group.Streams {
				if stream.Sink.GetName() == elementName {
					group.HandleStreamError(stream)
					break
				}
			}
		}
	})

	// Handle warnings
	bus.Connect("message::warning", func(_ *gst.Bus, msg *gst.Message) {
		err := msg.ParseError()
		p.logger.Warn("Pipeline warning",
			"element", msg.Source(),
			"warning", err.Error())
	})

	// Handle state changes
	bus.Connect("message::state-changed", func(_ *gst.Bus, msg *gst.Message) {
		oldState, newState := msg.ParseStateChanged()
		elementName := msg.Source()

		if elementName == p.pipeline.GetName() {
			p.logger.Debug("Pipeline state changed",
				"old", oldState,
				"new", newState)
		} else if elementName == "srt-source" {
			p.logger.Info("SRT source state changed",
				"old", oldState,
				"new", newState)
		}
	})

	// Handle SRT-specific messages
	bus.Connect("message::element", func(_ *gst.Bus, msg *gst.Message) {
		if msg.Source() == "srt-source" {
			structure := msg.GetStructure()
			if structure != nil {
				p.logger.Debug("SRT source message", "structure", structure.Name())
			}
		}
	})
}
