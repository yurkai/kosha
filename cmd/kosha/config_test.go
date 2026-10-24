package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"kosha/internal/server"
)

func lookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) { value, ok := values[key]; return value, ok }
}

func writeEnv(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigDefaultsWithoutEnvFile(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg, err := parseConfig(nil, lookup(nil), io.Discard)
	if err != nil || !reflect.DeepEqual(cfg, server.DefaultConfig()) {
		t.Fatalf("config = %+v, error = %v", cfg, err)
	}
}

func TestConfigLoadsDefaultEnvFile(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".env", []byte("KOSHA_PORT=3107\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseConfig(nil, lookup(nil), io.Discard)
	if err != nil || cfg.Address != "127.0.0.1:3107" {
		t.Fatalf("address = %q, error = %v", cfg.Address, err)
	}
}

func TestConfigSourcesAndPriority(t *testing.T) {
	path := writeEnv(t, `# Local settings
KOSHA_HOST=::1
KOSHA_PORT=3107
KOSHA_DATABASE="data/local room.db"
KOSHA_MAX_ROOMS=7
KOSHA_MAX_PEERS=8
KOSHA_MAX_MESSAGE_BYTES=65536
KOSHA_METRICS=true
`)
	want := server.Config{
		Address: "[::1]:3107", Database: "data/local room.db", MaxRooms: 7,
		MaxPeers: 8, MaxMessageBytes: 65536, Metrics: true,
	}
	for _, tc := range []struct {
		name string
		env  map[string]string
		args []string
		want server.Config
	}{
		{name: "file", want: want},
		{name: "process", env: map[string]string{
			"KOSHA_HOST": "127.0.0.2", "KOSHA_PORT": "4107", "KOSHA_DATABASE": "process.db",
			"KOSHA_MAX_ROOMS": "9", "KOSHA_MAX_PEERS": "10", "KOSHA_MAX_MESSAGE_BYTES": "32768", "KOSHA_METRICS": "false",
		}, want: server.Config{Address: "127.0.0.2:4107", Database: "process.db", MaxRooms: 9, MaxPeers: 10, MaxMessageBytes: 32768}},
		{name: "flags", env: map[string]string{
			"KOSHA_PORT": "invalid", "KOSHA_DATABASE": "", "KOSHA_MAX_ROOMS": "invalid",
			"KOSHA_MAX_PEERS": "invalid", "KOSHA_MAX_MESSAGE_BYTES": "invalid", "KOSHA_METRICS": "invalid",
		}, args: []string{"-addr", "127.0.0.1:0", "-db", "flags.db", "-max-rooms", "3", "-max-peers", "4", "-max-message-bytes", "8192", "-metrics=false"},
			want: server.Config{Address: "127.0.0.1:0", Database: "flags.db", MaxRooms: 3, MaxPeers: 4, MaxMessageBytes: 8192}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"-env-file", path}, tc.args...)
			cfg, err := parseConfig(args, lookup(tc.env), io.Discard)
			if err != nil || !reflect.DeepEqual(cfg, tc.want) {
				t.Fatalf("config = %+v, want %+v, error = %v", cfg, tc.want, err)
			}
		})
	}
}

func TestConfigEnvFileControl(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".env", []byte("broken file ?? secret-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := parseConfig(nil, lookup(nil), io.Discard)
	if err == nil || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("expected syntax error without contents, got %v", err)
	}
	cfg, err := parseConfig([]string{"-env-file", ""}, lookup(map[string]string{"KOSHA_PORT": "4107"}), io.Discard)
	if err != nil || cfg.Address != "127.0.0.1:4107" {
		t.Fatalf("disabled env file: address = %q, error = %v", cfg.Address, err)
	}
	if _, err := parseConfig([]string{"-env-file", "missing.env"}, lookup(nil), io.Discard); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected missing explicitly selected file, got %v", err)
	}
	if _, err := parseConfig([]string{"-env-file", ".env"}, lookup(nil), io.Discard); err == nil {
		t.Fatal("explicit file must still be parsed")
	}
	if err := os.Remove(".env"); err != nil {
		t.Fatal(err)
	}
	if _, err := parseConfig([]string{"-env-file", ".env"}, lookup(nil), io.Discard); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("explicit .env must exist, got %v", err)
	}
}

func TestConfigRejectsInvalidSettings(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"KOSHA_PORT", ""}, {"KOSHA_PORT", "-1"}, {"KOSHA_PORT", "65536"}, {"KOSHA_PORT", "http"},
		{"KOSHA_HOST", "host:1234"}, {"KOSHA_DATABASE", ""}, {"KOSHA_DATABASE", ":memory:"},
		{"KOSHA_MAX_ROOMS", "abc"}, {"KOSHA_MAX_ROOMS", "0"}, {"KOSHA_MAX_PEERS", "101"},
		{"KOSHA_MAX_MESSAGE_BYTES", "-1"}, {"KOSHA_MAX_MESSAGE_BYTES", "1MiB"}, {"KOSHA_METRICS", "yes"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			_, err := parseConfig([]string{"-env-file", ""}, lookup(map[string]string{tc.key: tc.value}), io.Discard)
			if err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	for _, args := range [][]string{{"-addr", "127.0.0.1:70000"}, {"-addr", "missing-port"}, {"-max-rooms", "0"}, {"extra-argument"}, {"-unknown"}} {
		if _, err := parseConfig(append([]string{"-env-file", ""}, args...), lookup(nil), io.Discard); err == nil {
			t.Fatalf("expected error for args %v", args)
		}
	}
}

func TestConfigHelpDoesNotReadEnvFile(t *testing.T) {
	var output bytes.Buffer
	_, err := parseConfig([]string{"-env-file", "missing.env", "-help"}, lookup(nil), &output)
	if !errors.Is(err, flag.ErrHelp) || !strings.Contains(output.String(), "-env-file") || !strings.Contains(output.String(), "-addr") {
		t.Fatalf("error = %v, help = %s", err, output.String())
	}
}
