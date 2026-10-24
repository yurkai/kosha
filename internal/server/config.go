package server

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Address         string
	Database        string
	MaxRooms        int
	MaxPeers        int
	MaxMessageBytes int64
	Metrics         bool
	RoomTTL         time.Duration
}

func DefaultConfig() Config {
	return Config{
		Address: "127.0.0.1:8080", Database: "data/kosha.db",
		MaxRooms: 5, MaxPeers: 5, MaxMessageBytes: 1 << 20,
		RoomTTL: 14 * 24 * time.Hour,
	}
}

func (c Config) Validate() error {
	if c.RoomTTL <= 0 {
		return errors.New("room TTL must be positive (KOSHA_ROOM_TTL or -room-ttl)")
	}
	host, port, err := net.SplitHostPort(c.Address)
	if err != nil || (strings.Contains(host, ":") && net.ParseIP(host) == nil) {
		return errors.New("listen address must contain a valid host and numeric port (KOSHA_HOST/KOSHA_PORT or -addr)")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return errors.New("listen port must be 0..65535 (KOSHA_PORT or -addr)")
	}
	if c.Database == "" || c.Database == ":memory:" || strings.HasPrefix(c.Database, "file:") {
		return errors.New("database must be a persistent filesystem path")
	}
	if c.MaxRooms < 1 || c.MaxPeers < 1 || c.MaxRooms > 100 || c.MaxPeers > 100 || c.MaxMessageBytes < 1 {
		return errors.New("room/peer limits must be 1..100 and message limit must be positive")
	}
	return nil
}
