package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
)

type S3 struct {
	Endpoint        string
	Region          string
	Bucket          string
	AccessKey       string
	SecretKey       string
	UseSSL          bool
	ForcePathStyle  bool
	StoragePresign  bool
}

type Config struct {
	Port        string
	LogLevel    string
	BaseURL     string
	DatabaseURL string
	// BootstrappedToken is the sole bearer token; it authenticates as the
	// built-in admin user. The token is never persisted.
	BootstrappedToken string
	S3                S3
}

func Load() (*Config, error) {
	c := &Config{
		Port:        getenv("PORT", "8080"),
		LogLevel:    getenv("LOG_LEVEL", "info"),
		BaseURL:     getenv("BASE_URL", "http://localhost:8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		BootstrappedToken: os.Getenv("GOORBIT_BOOTSTRAP_TOKEN"),
		S3: S3{
			Endpoint:        os.Getenv("S3_ENDPOINT"),
			Region:          getenv("S3_REGION", "us-east-1"),
			Bucket:          os.Getenv("S3_BUCKET"),
			AccessKey:       os.Getenv("S3_ACCESS_KEY"),
			SecretKey:       os.Getenv("S3_SECRET_KEY"),
			UseSSL:          getenv("S3_USE_SSL", "true") == "true",
			ForcePathStyle:  getenv("S3_FORCE_PATH_STYLE", "true") == "true",
			StoragePresign:  getenv("STORAGE_PRESIGN", "false") == "true",
		},
	}
	for _, req := range []struct {
		name, val string
	}{
		{"DATABASE_URL", c.DatabaseURL},
		{"GOORBIT_BOOTSTRAP_TOKEN", c.BootstrappedToken},
		{"S3_ENDPOINT", c.S3.Endpoint},
		{"S3_BUCKET", c.S3.Bucket},
		{"S3_ACCESS_KEY", c.S3.AccessKey},
		{"S3_SECRET_KEY", c.S3.SecretKey},
	} {
		if req.val == "" {
			return nil, fmt.Errorf("missing required env %s", req.name)
		}
	}
	return c, nil
}

func Logger(level string) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (c *Config) PortInt() int {
	p, err := strconv.Atoi(c.Port)
	if err != nil {
		return 8080
	}
	return p
}
