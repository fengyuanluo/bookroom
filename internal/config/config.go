// Package config loads BookRoom settings from BOOKROOM_* environment variables.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// Config is the complete runtime configuration. Every field maps to one environment variable; see
// docs/operations.md for the table.
type Config struct {
	Addr      string `env:"ADDR" envDefault:":8080"`
	DataDir   string `env:"DATA_DIR" envDefault:"./data"`
	PublicURL string `env:"PUBLIC_URL"` // external base URL for signed links; derived from the request when empty
	WebDir    string `env:"WEB_DIR"`    // optional static Readest web build served at /

	JWTSecret  string        `env:"JWT_SECRET"` // generated into DATA_DIR/jwt.key when empty
	AccessTTL  time.Duration `env:"ACCESS_TTL" envDefault:"1h"`
	RefreshTTL time.Duration `env:"REFRESH_TTL" envDefault:"720h"`

	AdminUser     string `env:"ADMIN_USER"`     // bootstrap admin created on first start when no user exists
	AdminPassword string `env:"ADMIN_PASSWORD"` // password of the bootstrap admin

	CORSOrigins []string `env:"CORS_ORIGINS" envDefault:"*" envSeparator:","`
	MaxUpload   int64    `env:"MAX_UPLOAD_BYTES" envDefault:"2147483648"`
	LogLevel    string   `env:"LOG_LEVEL" envDefault:"info"`
	LogFormat   string   `env:"LOG_FORMAT" envDefault:"text"`

	ProxyAllowPrivate bool     `env:"PROXY_ALLOW_PRIVATE"`
	ProxyTrustedHosts []string `env:"PROXY_TRUSTED_HOSTS" envSeparator:","`

	ScrapeAuto        bool          `env:"SCRAPE_AUTO" envDefault:"true"`
	ScrapeInterval    time.Duration `env:"SCRAPE_INTERVAL" envDefault:"2s"`
	GoogleBooksKey    string        `env:"GOOGLE_BOOKS_KEY"`
	HardcoverToken    string        `env:"HARDCOVER_TOKEN"`
	OpenLibraryEnable bool          `env:"OPENLIBRARY_ENABLED" envDefault:"true"`
	OpenLibraryUA     string        `env:"OPENLIBRARY_CONTACT"` // contact e-mail raises Open Library's limit to 3 req/s
	DoubanEnable      bool          `env:"DOUBAN_ENABLED"`
	XSHBaseURL        string        `env:"XIAOSHUOHUB_URL" envDefault:"https://api.xiaoshuohub.com"`
	XSHAppKey         string        `env:"XIAOSHUOHUB_APP_KEY"`
	XSHAppSecret      string        `env:"XIAOSHUOHUB_APP_SECRET"`
}

// Load reads the environment and fills derived defaults.
func Load() (*Config, error) {
	cfg, err := env.ParseAsWithOptions[Config](env.Options{Prefix: "BOOKROOM_"})
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &cfg, cfg.finish()
}

func (c *Config) finish() error {
	if c.DataDir == "" {
		return errors.New("config: BOOKROOM_DATA_DIR is empty")
	}
	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return err
	}
	c.DataDir = abs
	if err := os.MkdirAll(c.DataDir, 0o750); err != nil {
		return fmt.Errorf("config: data dir: %w", err)
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	if c.JWTSecret == "" {
		secret, err := loadOrCreateSecret(filepath.Join(c.DataDir, "jwt.key"))
		if err != nil {
			return err
		}
		c.JWTSecret = secret
	}
	if len(c.JWTSecret) < 32 {
		return errors.New("config: BOOKROOM_JWT_SECRET must be at least 32 characters")
	}
	return nil
}

// DBPath is the SQLite database file.
func (c *Config) DBPath() string { return filepath.Join(c.DataDir, "bookroom.db") }

func loadOrCreateSecret(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(b)), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	secret := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(secret+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("config: write %s: %w", path, err)
	}
	return secret, nil
}
