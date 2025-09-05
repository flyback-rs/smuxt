package main

import (
	"github.com/BurntSushi/toml"
)

// Config is the top-level configuration structure.
type Config struct {
	Title        string
	Server       ServerConfig
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

// OutputType defines the protocol used by the output stream (RTMP or SRT).
type OutputType string

const (
	RTMP OutputType = "rtmp"
	SRT  OutputType = "srt"
)

// OutputConfig defines a single output stream.
type OutputConfig struct {
	Name        string
	Type        OutputType
	URL         string
	AudioTracks []int `toml:"audio_tracks"`
}

// LoadConfig parses the specified TOML file into a Config struct.
func LoadConfig(path string) (*Config, error) {
	var config Config
	if _, err := toml.DecodeFile(path, &config); err != nil {
		return nil, err
	}
	return &config, nil
}
