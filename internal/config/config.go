// Package config manages persistent cloudedits configuration, platform data
// directory defaults, and Docker Compose bundle generation.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// FolderConfig defines one configured photo root folder.
type FolderConfig struct {
	ID    string `json:"id"`
	Path  string `json:"path"`
	Label string `json:"label"`
}

// Config is persisted in <dataDir>/config.json.
type Config struct {
	LibraryID   string `json:"libraryId"`
	LibraryName string `json:"libraryName"`
	Hostname    string `json:"hostname"`
	DataDir     string `json:"dataDir"`
	SidecarDir  string `json:"sidecarDir,omitempty"`
	AuthMode    string `json:"authMode"` // "tailnet" | "token" | "loopback"
	BearerToken string `json:"bearerToken,omitempty"`
	ListenAddr  string `json:"listenAddr,omitempty"`
	// PublicURL and TailscaleIP are the tailnet address of the enrolled node,
	// recorded when it last came up so commands that do not start the node
	// (qr, status, the setup wizard) can show where the library is reachable.
	PublicURL    string         `json:"publicUrl,omitempty"`
	TailscaleIP  string         `json:"tailscaleIp,omitempty"`
	Folders      []FolderConfig `json:"folders"`
	AllowedHosts []string       `json:"allowedOrigins,omitempty"`
}

// DefaultDataDir returns the OS-standard directory outside photo folders where
// the rebuildable SQLite index, thumbnail cache, and tsnet state are stored.
func DefaultDataDir() string {
	if env := strings.TrimSpace(os.Getenv("CLOUDEDITS_DATA_DIR")); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "cloudedits")
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "cloudedits")
	case "windows":
		if appData := os.Getenv("LOCALAPPDATA"); appData != "" {
			return filepath.Join(appData, "cloudedits")
		}
		return filepath.Join(home, "AppData", "Local", "cloudedits")
	default:
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			return filepath.Join(xdg, "cloudedits")
		}
		return filepath.Join(home, ".local", "share", "cloudedits")
	}
}

// Default returns a populated default configuration.
func Default(dataDir string) *Config {
	if dataDir == "" {
		dataDir = DefaultDataDir()
	}
	hostname := strings.TrimSpace(os.Getenv("CLOUDEDITS_HOSTNAME"))
	if hostname == "" {
		hostname = "library"
	}
	return &Config{
		LibraryID:   "lib-" + randomHex(6),
		LibraryName: "Cloud Library",
		Hostname:    hostname,
		DataDir:     dataDir,
		SidecarDir:  strings.TrimSpace(os.Getenv("CLOUDEDITS_SIDECAR_DIR")),
		AuthMode:    "tailnet",
		ListenAddr:  "127.0.0.1:8787",
		Folders:     []FolderConfig{},
	}
}

// ConfigPath returns the config.json file path under dataDir.
func ConfigPath(dataDir string) string {
	return filepath.Join(dataDir, "config.json")
}

// Load reads config.json from dataDir, or returns Default if it does not exist.
func Load(dataDir string) (*Config, error) {
	if dataDir == "" {
		dataDir = DefaultDataDir()
	}
	raw, err := os.ReadFile(ConfigPath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return Default(dataDir), nil
	}
	if err != nil {
		return nil, err
	}
	cfg := Default(dataDir)
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse config.json: %w", err)
	}
	cfg.DataDir = dataDir
	if cfg.LibraryID == "" {
		cfg.LibraryID = "lib-" + randomHex(6)
	}
	if cfg.Hostname == "" {
		cfg.Hostname = "library"
	}
	if cfg.LibraryName == "" {
		cfg.LibraryName = "Cloud Library"
	}
	if cfg.AuthMode == "" {
		cfg.AuthMode = "tailnet"
	}
	return cfg, nil
}

// Save atomically persists cfg to <cfg.DataDir>/config.json.
func Save(cfg *Config) error {
	if cfg == nil {
		return errors.New("nil config")
	}
	if cfg.DataDir == "" {
		cfg.DataDir = DefaultDataDir()
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := filepath.Join(cfg.DataDir, ".config."+randomHex(6)+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ConfigPath(cfg.DataDir))
}

// AddFolder adds a photo folder cleanly (deduplicated by absolute path).
func (c *Config) AddFolder(folderPath, label string) (FolderConfig, error) {
	abs, err := filepath.Abs(folderPath)
	if err != nil {
		return FolderConfig{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return FolderConfig{}, err
	}
	if !info.IsDir() {
		return FolderConfig{}, fmt.Errorf("%s is not a directory", abs)
	}
	for _, existing := range c.Folders {
		if existing.Path == abs {
			return existing, nil
		}
	}
	if strings.TrimSpace(label) == "" {
		label = filepath.Base(abs)
	}
	fc := FolderConfig{
		ID:    MakeFolderID(label, len(c.Folders)),
		Path:  abs,
		Label: label,
	}
	c.Folders = append(c.Folders, fc)
	return fc, nil
}

// RemoveFolder removes a folder by path, ID, or label.
func (c *Config) RemoveFolder(target string) bool {
	abs, _ := filepath.Abs(target)
	kept := make([]FolderConfig, 0, len(c.Folders))
	removed := false
	for _, f := range c.Folders {
		if f.Path == target || f.Path == abs || f.ID == target || f.Label == target {
			removed = true
			continue
		}
		kept = append(kept, f)
	}
	c.Folders = kept
	return removed
}

// MakeFolderID produces a deterministic URL-safe folder identifier.
func MakeFolderID(label string, idx int) string {
	var b strings.Builder
	for _, r := range strings.ToLower(label) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == ' ':
			b.WriteByte('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		slug = "folder"
	}
	if idx == 0 {
		return slug
	}
	return fmt.Sprintf("%s-%d", slug, idx+1)
}

// WriteDockerBundle creates docker-compose.yml and .env.example in outDir.
func WriteDockerBundle(outDir, folderPath, hostname string) (composePath, envPath string, err error) {
	if hostname == "" {
		hostname = "library"
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", "", err
	}
	compose := fmt.Sprintf(`services:
  cloudedits:
    image: ghcr.io/thebigsasha/cloudedits:latest
    restart: unless-stopped
    environment:
      CLOUDEDITS_HOSTNAME: %s
      TS_AUTHKEY: ${TS_AUTHKEY}
    volumes:
      - %s:/library/photos
      - cloudedits_data:/data
    command: ["serve", "--tsnet", "--folder", "/library/photos"]

volumes:
  cloudedits_data:
`, hostname, folderPath)

	envExample := `# Generate a one-time Tailscale auth key at:
# https://login.tailscale.com/admin/settings/keys
TS_AUTHKEY=tskey-auth-replace-me
`
	composePath = filepath.Join(outDir, "docker-compose.yml")
	envPath = filepath.Join(outDir, ".env.example")
	if err := os.WriteFile(composePath, []byte(compose), 0o644); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(envPath, []byte(envExample), 0o644); err != nil {
		return "", "", err
	}
	return composePath, envPath, nil
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
