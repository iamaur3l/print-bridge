package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultIsUsable(t *testing.T) {
	cfg := Default()
	if cfg.Server.Port != 9567 {
		t.Errorf("expected the default port 9567, got %d", cfg.Server.Port)
	}
	if cfg.Queue.MaxAttempts != 5 || cfg.Queue.MaxDepth != 5000 || cfg.Queue.MaxConcurrentJobs != 4 {
		t.Errorf("unexpected queue defaults: %+v", cfg.Queue)
	}
	if cfg.Updater.AutoUpdate {
		t.Error("auto-update must be opt-in")
	}
}

func TestLoadCreatesFileWithDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultFilename)

	cfg, result := Load(path)
	if result.Source != SourceCreated {
		t.Errorf("expected the file to be created, got %q", result.Source)
	}
	if cfg.Server.Port != 9567 {
		t.Errorf("unexpected config: %+v", cfg)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected the config file to exist: %v", err)
	}

	// Reading it back now reports a real file rather than a creation.
	_, second := Load(path)
	if second.Source != SourceFile {
		t.Errorf("expected the second load to read the file, got %q", second.Source)
	}
}

func TestLoadToleratesNotepadBOM(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultFilename)
	content := "\uFEFF{\n  \"server\": { \"port\": 9600 },\n  \"security\": { \"allowed_origins\": [\"https://pos.example.com\"] }\n}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, result := Load(path)
	if result.Source != SourceFile {
		t.Fatalf("expected a BOM-prefixed file to load, got %q (%s)", result.Source, result.Warning)
	}
	if cfg.Server.Port != 9600 {
		t.Errorf("expected the configured port, got %d", cfg.Server.Port)
	}
	if len(cfg.Security.AllowedOrigins) != 1 || cfg.Security.AllowedOrigins[0] != "https://pos.example.com" {
		t.Errorf("expected the configured origin, got %v", cfg.Security.AllowedOrigins)
	}
	// Unset sections keep their defaults.
	if cfg.Queue.MaxConcurrentJobs != 4 {
		t.Errorf("expected queue defaults for unset sections, got %+v", cfg.Queue)
	}
}

// TestLoadRecoversFromUnreadableFile is the behaviour that keeps a till printing: a
// typo in the JSON must never stop the agent from starting.
func TestLoadRecoversFromUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultFilename)

	broken := []byte("{ \"server\": { \"port\": 9567,, }")
	if err := os.WriteFile(path, broken, 0o644); err != nil {
		t.Fatalf("failed to write broken config: %v", err)
	}

	cfg, result := Load(path)
	if result.Source != SourceRecovered {
		t.Errorf("expected the load to report recovery, got %q", result.Source)
	}
	if cfg.Server.Port != 9567 {
		t.Errorf("expected defaults after recovery, got %+v", cfg)
	}
	if result.Warning == "" {
		t.Error("expected a warning explaining the recovery")
	}

	if result.BackupPath == "" {
		t.Fatal("expected the broken file to be backed up")
	}
	backed, err := os.ReadFile(result.BackupPath)
	if err != nil {
		t.Fatalf("expected the backup to exist: %v", err)
	}
	if string(backed) != string(broken) {
		t.Error("the backup must contain the original bytes, unmodified")
	}

	// The original file is left alone so an operator can fix and restore it.
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the original file should still exist: %v", err)
	}
	if string(original) != string(broken) {
		t.Error("recovery must not overwrite the unreadable file")
	}

	matches, _ := filepath.Glob(filepath.Join(dir, "config.corrupt.*"+filepath.Ext(DefaultFilename)))
	if len(matches) != 1 {
		t.Errorf("expected exactly one corrupt backup, found %v", matches)
	}
}

func TestLoadEmptyPathUsesDefaults(t *testing.T) {
	cfg, result := Load("")
	if result.Source != SourceDefault {
		t.Errorf("expected built-in defaults, got %q", result.Source)
	}
	if cfg.Server.Port != 9567 {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

func TestApplyDefaultsClampsBadValues(t *testing.T) {
	cfg := Config{
		Server: ServerConfig{Port: 70000, TLSCertFile: "cert.pem"},
		Queue:  QueueConfig{MaxAttempts: 500, MaxConcurrentJobs: 90},
		Security: SecurityConfig{
			AllowedOrigins: []string{" https://pos.example.com ", "", "   "},
		},
	}

	notes := cfg.ApplyDefaults()

	if cfg.Server.Port != 9567 {
		t.Errorf("expected the port to be clamped, got %d", cfg.Server.Port)
	}
	if cfg.Queue.MaxAttempts != 100 || cfg.Queue.MaxConcurrentJobs != 32 {
		t.Errorf("expected the queue limits to be clamped, got %+v", cfg.Queue)
	}
	if cfg.Queue.MaxDepth != 5000 {
		t.Errorf("expected a default depth, got %d", cfg.Queue.MaxDepth)
	}
	if len(cfg.Security.AllowedOrigins) != 1 || cfg.Security.AllowedOrigins[0] != "https://pos.example.com" {
		t.Errorf("expected blank origins to be dropped and whitespace trimmed, got %v", cfg.Security.AllowedOrigins)
	}
	if cfg.Server.TLSCertFile != "" || cfg.Server.TLSKeyFile != "" {
		t.Error("expected half-configured TLS to be disabled")
	}
	if len(notes) < 4 {
		t.Errorf("expected a note per clamped value, got %v", notes)
	}
}

func TestSaveRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", DefaultFilename)

	cfg := Default()
	cfg.Server.Port = 9700
	cfg.Updater.AutoUpdate = true
	cfg.Security.AllowedOrigins = []string{"https://pos.example.com"}

	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, result := Load(path)
	if result.Source != SourceFile {
		t.Fatalf("expected the saved file to load, got %q", result.Source)
	}
	if loaded.Server.Port != 9700 || !loaded.Updater.AutoUpdate {
		t.Errorf("values did not round trip: %+v", loaded)
	}
	if len(loaded.Security.AllowedOrigins) != 1 {
		t.Errorf("origins did not round trip: %v", loaded.Security.AllowedOrigins)
	}

	// No temporary file should be left behind by the atomic write.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("expected the temporary file to be renamed away")
	}
}

func TestDefaultPath(t *testing.T) {
	if got := DefaultPath(""); got != DefaultFilename {
		t.Errorf("expected the bare filename, got %q", got)
	}
	got := DefaultPath(filepath.Join("C:", "data"))
	if !strings.HasSuffix(got, DefaultFilename) {
		t.Errorf("expected a path ending in %s, got %q", DefaultFilename, got)
	}
}