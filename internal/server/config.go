package server

import (
	"errors"
	"strings"
)

type Config struct {
	Address         string
	Database        string
	MaxRooms        int
	MaxPeers        int
	MaxMessageBytes int64
	Metrics         bool
}

func DefaultConfig() Config {
	return Config{
		Address: "127.0.0.1:8080", Database: "data/kosha.db",
		MaxRooms: 5, MaxPeers: 5, MaxMessageBytes: 1 << 20,
	}
}

func (c Config) Validate() error {
	if c.Database == "" || c.Database == ":memory:" || strings.HasPrefix(c.Database, "file:") {
		return errors.New("database must be a persistent filesystem path")
	}
	if c.MaxRooms < 1 || c.MaxPeers < 1 || c.MaxRooms > 100 || c.MaxPeers > 100 || c.MaxMessageBytes < 1 {
		return errors.New("room/peer limits must be 1..100 and message limit must be positive")
	}
	return nil
}
