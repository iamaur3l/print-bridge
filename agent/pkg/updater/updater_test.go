package updater

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFetchManifest(t *testing.T) {
	manifest := Manifest{
		Version:     "0.2.0",
		ReleaseDate: "2026-07-29",
		Platforms: map[string]PlatformRelease{
			"windows_amd64": {
				URL:    "https://releases.printbridge.dev/agent/v0.2.0/printbridge-agent-windows-amd64.exe",
				SHA256: "abcdef1234567890",
			},
		},
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(manifest)
	}))
	defer ts.Close()

	up := NewUpdater(ts.URL, 0)
	m, err := up.FetchManifest()
	if err != nil {
		t.Fatalf("FetchManifest failed: %v", err)
	}

	if m.Version != "0.2.0" {
		t.Errorf("expected version 0.2.0, got %s", m.Version)
	}

	rel, ok := m.Platforms["windows_amd64"]
	if !ok {
		t.Fatal("expected windows_amd64 platform entry")
	}
	if rel.URL != "https://releases.printbridge.dev/agent/v0.2.0/printbridge-agent-windows-amd64.exe" {
		t.Errorf("unexpected URL: %s", rel.URL)
	}
}

func TestCheckForUpdate(t *testing.T) {
	origVersion := Version
	Version = "0.1.0"
	defer func() { Version = origVersion }()

	manifest := Manifest{
		Version: "0.2.0",
		Platforms: map[string]PlatformRelease{
			"windows_amd64": {URL: "http://example.com/new.exe", SHA256: ""},
		},
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(manifest)
	}))
	defer ts.Close()

	up := NewUpdater(ts.URL, 0)
	vers, avail, err := up.CheckForUpdate()
	if err != nil {
		t.Fatalf("CheckForUpdate failed: %v", err)
	}
	if !avail {
		t.Fatal("expected update to be available")
	}
	if vers != "0.2.0" {
		t.Errorf("expected version 0.2.0, got %s", vers)
	}

	// Test no update when same version
	manifest.Version = "0.1.0"
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(manifest)
	}))
	defer ts2.Close()

	up2 := NewUpdater(ts2.URL, 0)
	_, avail2, err := up2.CheckForUpdate()
	if err != nil {
		t.Fatalf("CheckForUpdate failed: %v", err)
	}
	if avail2 {
		t.Fatal("expected no update when version same")
	}
}

func TestDownloadUpdate(t *testing.T) {
	origVersion := Version
	Version = "0.1.0"
	defer func() { Version = origVersion }()

	content := []byte("mock binary content")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer ts.Close()

	up := NewUpdater("", 0)
	up.availableVers = "0.2.0"
	up.availablePath = ts.URL
	up.availableSHA = ""

	tmpDir := t.TempDir()
	path, err := up.DownloadUpdate(tmpDir)
	if err != nil {
		t.Fatalf("DownloadUpdate failed: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read downloaded file: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("downloaded content mismatch")
	}
}

func TestDownloadUpdateSHA256Mismatch(t *testing.T) {
	origVersion := Version
	Version = "0.1.0"
	defer func() { Version = origVersion }()

	content := []byte("mock binary")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer ts.Close()

	up := NewUpdater("", 0)
	up.availableVers = "0.2.0"
	up.availablePath = ts.URL
	up.availableSHA = "0000000000000000000000000000000000000000000000000000000000000000"

	tmpDir := t.TempDir()
	_, err := up.DownloadUpdate(tmpDir)
	if err == nil {
		t.Fatal("expected SHA256 mismatch error")
	}
}

func TestCopyFile(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.txt")
	dst := filepath.Join(t.TempDir(), "dst.txt")

	orig := []byte("hello world")
	if err := os.WriteFile(src, orig, 0644); err != nil {
		t.Fatal(err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile failed: %v", err)
	}

	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("failed to read dest: %v", err)
	}
	if string(data) != string(orig) {
		t.Errorf("content mismatch: got %s, expected %s", data, orig)
	}
}
