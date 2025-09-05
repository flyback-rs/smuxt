package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/BurntSushi/toml"
)

// OutputType defines the protocol used by the output stream.
type OutputType string

const (
	OutputTypeRTMP OutputType = "rtmp"
	OutputTypeSRT  OutputType = "srt"
)

// Config is the top-level configuration structure.
type Config struct {
	Title        string                 `toml:"title"`
	Server       ServerConfig           `toml:"server"`
	OutputGroups map[string]GroupConfig `toml:"output_groups"`
}

// ServerConfig holds the SRT listener configuration.
type ServerConfig struct {
	ListenAddress string `toml:"listen_address"`
	ListenPort    int    `toml:"listen_port"`
	Passphrase    string `toml:"passphrase"`
}

// GroupConfig holds a list of outputs that belong to a failover group.
type GroupConfig struct {
	Outputs []OutputConfig `toml:"outputs"`
}

// OutputConfig defines a single output stream.
type OutputConfig struct {
	Name        string     `toml:"name"`
	Type        OutputType `toml:"type"`
	URL         string     `toml:"url"`
	AudioTracks []int      `toml:"audio_tracks"`
}

// LoadConfig parses the specified TOML file into a Config struct.
func LoadConfig(path string) (*Config, error) {
	var config Config
	if _, err := toml.DecodeFile(path, &config); err != nil {
		return nil, fmt.Errorf("failed to decode config file: %w", err)
	}

	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return &config, nil
}

// Validate checks if the configuration is valid.
func (c *Config) Validate() error {
	if err := c.Server.Validate(); err != nil {
		return fmt.Errorf("server config: %w", err)
	}

	if len(c.OutputGroups) == 0 {
		return errors.New("at least one output group is required")
	}

	for name, group := range c.OutputGroups {
		if err := group.Validate(); err != nil {
			return fmt.Errorf("output group %q: %w", name, err)
		}
	}

	return nil
}

// Validate checks if the server configuration is valid.
func (sc *ServerConfig) Validate() error {
	if sc.ListenAddress == "" {
		sc.ListenAddress = "0.0.0.0"
	}

	if sc.ListenPort <= 0 || sc.ListenPort > 65535 {
		return errors.New("listen_port must be between 1 and 65535")
	}

	return nil
}

// Validate checks if the group configuration is valid.
func (gc *GroupConfig) Validate() error {
	if len(gc.Outputs) == 0 {
		return errors.New("at least one output is required")
	}

	// All outputs in a group should have the same type
	firstType := gc.Outputs[0].Type
	for i, output := range gc.Outputs {
		if err := output.Validate(); err != nil {
			return fmt.Errorf("output %d: %w", i, err)
		}
		if output.Type != firstType {
			return errors.New("all outputs in a group must have the same type")
		}
	}

	return nil
}

// Validate checks if the output configuration is valid.
func (oc *OutputConfig) Validate() error {
	if oc.Name == "" {
		return errors.New("output name is required")
	}

	if oc.Type != OutputTypeRTMP && oc.Type != OutputTypeSRT {
		return errors.New("output type must be 'rtmp' or 'srt'")
	}

	if oc.URL == "" {
		return errors.New("output URL is required")
	}

	// Validate URL format
	u, err := url.Parse(oc.URL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	switch oc.Type {
	case OutputTypeRTMP:
		if !strings.HasPrefix(u.Scheme, "rtmp") {
			return errors.New("RTMP output requires rtmp:// or rtmps:// URL scheme")
		}
	case OutputTypeSRT:
		if u.Scheme != "srt" {
			return errors.New("SRT output requires srt:// URL scheme")
		}
	}

	return nil
}

// GetSRTListenURI returns the formatted SRT listen URI.
func (sc *ServerConfig) GetSRTListenURI() string {
	uri := fmt.Sprintf("srt://%s:%d?mode=listener", sc.ListenAddress, sc.ListenPort)
	if sc.Passphrase != "" {
		uri += "&passphrase=" + url.QueryEscape(sc.Passphrase)
	}
	return uri
}
