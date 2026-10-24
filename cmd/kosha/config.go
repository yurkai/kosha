package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"

	"github.com/joho/godotenv"

	"kosha/internal/server"
)

// parseConfig does not modify the process environment. Explicit flags win over
// process variables, which win over the env file, which wins over defaults.
func parseConfig(args []string, lookupEnv func(string) (string, bool), output io.Writer) (server.Config, error) {
	cfg := server.DefaultConfig()
	flags := flag.NewFlagSet("kosha", flag.ContinueOnError)
	flags.SetOutput(output)
	envFile := flags.String("env-file", ".env", "dotenv file (missing default is OK; empty disables loading)")
	flags.StringVar(&cfg.Address, "addr", cfg.Address, "HTTP listen address (overrides KOSHA_HOST/KOSHA_PORT)")
	flags.StringVar(&cfg.Database, "db", cfg.Database, "persistent SQLite file")
	flags.IntVar(&cfg.MaxRooms, "max-rooms", cfg.MaxRooms, "maximum resident rooms")
	flags.IntVar(&cfg.MaxPeers, "max-peers", cfg.MaxPeers, "maximum participants per room")
	flags.Int64Var(&cfg.MaxMessageBytes, "max-message-bytes", cfg.MaxMessageBytes, "maximum incoming WebSocket message size")
	flags.BoolVar(&cfg.Metrics, "metrics", cfg.Metrics, "enable diagnostic /debug/metrics (keep private)")
	flags.DurationVar(&cfg.RoomTTL, "room-ttl", cfg.RoomTTL, "inactive room retention (e.g. 336h for 14 days)")
	if err := flags.Parse(args); err != nil {
		return cfg, err
	}
	if flags.NArg() != 0 {
		return cfg, errors.New("unexpected positional arguments; use -help for available flags")
	}
	explicit := make(map[string]bool)
	flags.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	values := map[string]string{}
	if *envFile != "" {
		var err error
		values, err = godotenv.Read(*envFile)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) || explicit["env-file"] {
				var pathErr *os.PathError
				if errors.As(err, &pathErr) {
					return cfg, fmt.Errorf("read env file: %w", err)
				}
				// A parser error may include the file contents. Keep those out of logs.
				return cfg, fmt.Errorf("invalid dotenv syntax in %q", *envFile)
			}
		}
	}
	value := func(key string) (string, bool) {
		if v, ok := lookupEnv(key); ok {
			return v, true
		}
		v, ok := values[key]
		return v, ok
	}
	if !explicit["addr"] {
		host, port, _ := net.SplitHostPort(cfg.Address) // The built-in default is valid.
		if v, ok := value("KOSHA_HOST"); ok {
			host = v
		}
		if v, ok := value("KOSHA_PORT"); ok {
			port = v
		}
		cfg.Address = net.JoinHostPort(host, port)
	}
	for _, setting := range []struct{ env, flag string }{
		{"KOSHA_DATABASE", "db"}, {"KOSHA_MAX_ROOMS", "max-rooms"},
		{"KOSHA_MAX_PEERS", "max-peers"}, {"KOSHA_MAX_MESSAGE_BYTES", "max-message-bytes"},
		{"KOSHA_METRICS", "metrics"},
		{"KOSHA_ROOM_TTL", "room-ttl"},
	} {
		if explicit[setting.flag] {
			continue
		}
		if v, ok := value(setting.env); ok {
			if err := flags.Set(setting.flag, v); err != nil {
				return cfg, fmt.Errorf("invalid %s (see -help)", setting.env)
			}
		}
	}
	return cfg, cfg.Validate()
}
