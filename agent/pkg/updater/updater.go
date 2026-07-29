package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

var Version = "0.1.0"

const DefaultManifestURL = "https://releases.printbridge.dev/manifest.json"

type PlatformRelease struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type Manifest struct {
	Version     string                     `json:"version"`
	ReleaseDate string                     `json:"release_date"`
	Platforms   map[string]PlatformRelease `json:"platforms"`
}

type Updater struct {
	manifestURL    string
	checkInterval  time.Duration
	httpClient     *http.Client
	lastCheck      time.Time
	availableVers  string
	availablePath  string
	availableSHA   string
	backupDir      string
	stopChan       chan struct{}
}

func NewUpdater(manifestURL string, checkInterval time.Duration) *Updater {
	if manifestURL == "" {
		manifestURL = DefaultManifestURL
	}
	if checkInterval <= 0 {
		checkInterval = 24 * time.Hour
	}
	return &Updater{
		manifestURL:   manifestURL,
		checkInterval: checkInterval,
		httpClient:    &http.Client{Timeout: 30 * time.Second},
		stopChan:      make(chan struct{}),
	}
}

func (u *Updater) platformKey() string {
	return fmt.Sprintf("%s_%s", runtime.GOOS, runtime.GOARCH)
}

func (u *Updater) FetchManifest() (*Manifest, error) {
	resp, err := u.httpClient.Get(u.manifestURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest returned HTTP %d", resp.StatusCode)
	}

	var m Manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("failed to decode manifest: %w", err)
	}
	return &m, nil
}

func (u *Updater) CheckForUpdate() (string, bool, error) {
	manifest, err := u.FetchManifest()
	if err != nil {
		return "", false, err
	}

	if manifest.Version <= Version {
		return manifest.Version, false, nil
	}

	key := u.platformKey()
	rel, ok := manifest.Platforms[key]
	if !ok {
		return manifest.Version, false, fmt.Errorf("no release for platform %s", key)
	}

	u.availableVers = manifest.Version
	u.availablePath = rel.URL
	u.availableSHA = rel.SHA256
	u.lastCheck = time.Now()

	return manifest.Version, true, nil
}

func (u *Updater) DownloadUpdate(destDir string) (string, error) {
	if u.availablePath == "" {
		return "", fmt.Errorf("no update available; call CheckForUpdate first")
	}

	if err := os.MkdirAll(destDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create dest dir: %w", err)
	}

	tmpFile := filepath.Join(destDir, fmt.Sprintf("printbridge_update_%s.tmp", u.availableVers))
	out, err := os.Create(tmpFile)
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}

	log.Printf("[Updater] Downloading update %s from %s", u.availableVers, u.availablePath)
	resp, err := u.httpClient.Get(u.availablePath)
	if err != nil {
		out.Close()
		os.Remove(tmpFile)
		return "", fmt.Errorf("failed to download update: %w", err)
	}
	defer resp.Body.Close()

	hasher := sha256.New()
	writer := io.MultiWriter(out, hasher)

	if _, err := io.Copy(writer, resp.Body); err != nil {
		out.Close()
		os.Remove(tmpFile)
		return "", fmt.Errorf("failed to write update: %w", err)
	}
	out.Close()

	computedSHA := hex.EncodeToString(hasher.Sum(nil))
	if u.availableSHA != "" && computedSHA != u.availableSHA {
		os.Remove(tmpFile)
		return "", fmt.Errorf("SHA256 mismatch: expected %s, got %s", u.availableSHA, computedSHA)
	}

	log.Printf("[Updater] Update downloaded and verified (%s)", tmpFile)
	return tmpFile, nil
}

func (u *Updater) ApplyUpdate(downloadedPath string) error {
	currentExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get current executable path: %w", err)
	}
	currentExe, err = filepath.Abs(currentExe)
	if err != nil {
		return fmt.Errorf("failed to resolve absolute path: %w", err)
	}

	backupPath := filepath.Join(u.backupDir, filepath.Base(currentExe)+".bak")
	if u.backupDir == "" {
		backupDir := filepath.Join(filepath.Dir(currentExe), ".printbridge_backup")
		if err := os.MkdirAll(backupDir, 0755); err == nil {
			backupPath = filepath.Join(backupDir, filepath.Base(currentExe)+".bak")
		}
	}

	if currentExe != "" {
		if err := copyFile(currentExe, backupPath); err != nil {
			log.Printf("[Updater] Warning: failed to create backup at %s: %v", backupPath, err)
		} else {
			log.Printf("[Updater] Backed up current binary to %s", backupPath)
		}
	}

	if err := copyFile(downloadedPath, currentExe); err != nil {
		return fmt.Errorf("failed to replace binary: %w", err)
	}

	if err := os.Chmod(currentExe, 0755); err != nil {
		log.Printf("[Updater] Warning: failed to set executable permissions: %v", err)
	}

	log.Printf("[Updater] Update applied successfully. New version: %s", u.availableVers)
	return nil
}

func (u *Updater) Rollback(lastGoodPath string) error {
	currentExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get current executable path: %w", err)
	}

	backupPath := filepath.Join(filepath.Dir(currentExe), ".printbridge_backup", filepath.Base(currentExe)+".bak")
	if lastGoodPath != "" {
		backupPath = lastGoodPath
	}

	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		return fmt.Errorf("no backup binary found at %s", backupPath)
	}

	if err := copyFile(backupPath, currentExe); err != nil {
		return fmt.Errorf("failed to restore backup: %w", err)
	}

	if err := os.Chmod(currentExe, 0755); err != nil {
		log.Printf("[Updater] Warning: failed to set executable permissions on rollback: %v", err)
	}

	log.Printf("[Updater] Rolled back to previous version from %s", backupPath)
	return nil
}

func (u *Updater) StartPeriodicCheck(queueEmptyFn func() bool) {
	go func() {
		ticker := time.NewTicker(u.checkInterval)
		defer ticker.Stop()

		for {
			select {
			case <-u.stopChan:
				return
			case <-ticker.C:
				vers, avail, err := u.CheckForUpdate()
				if err != nil {
					log.Printf("[Updater] Periodic check failed: %v", err)
					continue
				}
				if avail {
					log.Printf("[Updater] Update available: %s (current: %s)", vers, Version)
					if !queueEmptyFn() {
						log.Printf("[Updater] Queue not empty, deferring update")
						continue
					}
					log.Printf("[Updater] Applying update %s", vers)
					if err := u.ApplyAvailableUpdate(); err != nil {
						log.Printf("[Updater] Auto-update failed: %v", err)
					}
				}
			}
		}
	}()
}

func (u *Updater) StopPeriodicCheck() {
	close(u.stopChan)
}

func (u *Updater) ApplyAvailableUpdate() error {
	tmpDir, err := os.MkdirTemp("", "printbridge-update-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	downloaded, err := u.DownloadUpdate(tmpDir)
	if err != nil {
		return err
	}

	if err := u.ApplyUpdate(downloaded); err != nil {
		return err
	}

	return nil
}

func (u *Updater) LastCheckTime() time.Time {
	return u.lastCheck
}

func (u *Updater) AvailableVersion() string {
	return u.availableVers
}

func copyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open source: %w", err)
	}
	defer sourceFile.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}

	destFile, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("failed to create destination: %w", err)
	}
	defer destFile.Close()

	if _, err := io.Copy(destFile, sourceFile); err != nil {
		return fmt.Errorf("failed to copy: %w", err)
	}
	return nil
}
