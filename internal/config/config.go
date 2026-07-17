package config

import (
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration loaded from environment variables.
type Config struct {
	Port            string
	APIKey          string
	WebhookURL      string
	WebhookEvents   []string
	StoreDir        string
	DatabaseURL     string
	DownloadMedia   bool
	MaxDownloadByte int64
	StoreMedia      bool
	MediaDir        string
	MediaBackend    string
	S3Endpoint      string
	S3Bucket        string
	S3AccessKey     string
	S3SecretKey     string
	S3UseSSL        bool
	S3Region        string
	LogLevel        string

	DefaultCountryCode string

	DefaultRateLimit     int
	DefaultRateWindowSec int
	DefaultMaxSessions   int

	StoreMessages        bool
	MessageRetentionDays int
	StoreChats           []string
	StoreChatsExclude    []string

	AccessLogRetentionDays int // 0 = nonaktif, simpan akses log selamanya jika > 0

	BulkMinDelayMS int
	BulkMaxDelayMS int
	BulkAutoResume bool

	WebhookWorkers    int
	WebhookQueueSize  int
	WebhookMaxRetries int
	WebhookBackoffMS  int
}

func getEnvInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// splitList memecah string comma-separated menjadi slice, membuang spasi dan
// entri kosong. String kosong menghasilkan slice kosong (bukan nil-of-one).
func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func getEnvBool(key string, fallback bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

func getEnvInt64(key string, fallback int64) int64 {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}

// Load reads configuration from the environment, applying sensible defaults.
func Load() *Config {
	events := strings.Split(getEnv("WEBHOOK_EVENTS", "message"), ",")
	for i := range events {
		events[i] = strings.TrimSpace(events[i])
	}

	return &Config{
		Port:            getEnv("PORT", "3000"),
		APIKey:          getEnv("API_KEY", ""),
		WebhookURL:      getEnv("WEBHOOK_URL", ""),
		WebhookEvents:   events,
		StoreDir:        getEnv("STORE_DIR", "./data"),
		DatabaseURL:     getEnv("DATABASE_URL", ""),
		DownloadMedia:   getEnvBool("DOWNLOAD_MEDIA", true),
		MaxDownloadByte: getEnvInt64("MAX_DOWNLOAD_BYTES", 200*1024*1024),
		StoreMedia:      getEnvBool("STORE_MEDIA", false),
		MediaDir:        getEnv("MEDIA_DIR", ""),
		MediaBackend:    getEnv("MEDIA_BACKEND", "disk"),
		S3Endpoint:      getEnv("S3_ENDPOINT", ""),
		S3Bucket:        getEnv("S3_BUCKET", ""),
		S3AccessKey:     getEnv("S3_ACCESS_KEY", ""),
		S3SecretKey:     getEnv("S3_SECRET_KEY", ""),
		S3UseSSL:        getEnvBool("S3_USE_SSL", false),
		S3Region:        getEnv("S3_REGION", ""),
		LogLevel:        getEnv("LOG_LEVEL", "INFO"),

		DefaultCountryCode: getEnv("DEFAULT_COUNTRY_CODE", ""),

		DefaultRateLimit:     getEnvInt("DEFAULT_RATE_LIMIT", 0),
		DefaultRateWindowSec: getEnvInt("DEFAULT_RATE_WINDOW_SEC", 60),
		DefaultMaxSessions:   getEnvInt("DEFAULT_MAX_SESSIONS", 0),

		StoreMessages:        getEnvBool("STORE_MESSAGES", false),
		MessageRetentionDays: getEnvInt("MESSAGE_RETENTION_DAYS", 0),
		StoreChats:           splitList(getEnv("STORE_CHATS", "")),
		StoreChatsExclude:    splitList(getEnv("STORE_CHATS_EXCLUDE", "")),

		AccessLogRetentionDays: getEnvInt("ACCESS_LOG_RETENTION_DAYS", 7),

		BulkMinDelayMS: getEnvInt("BULK_MIN_DELAY_MS", 3000),
		BulkMaxDelayMS: getEnvInt("BULK_MAX_DELAY_MS", 6000),
		BulkAutoResume: getEnvBool("BULK_AUTO_RESUME", true),

		WebhookWorkers:    getEnvInt("WEBHOOK_WORKERS", 4),
		WebhookQueueSize:  getEnvInt("WEBHOOK_QUEUE_SIZE", 1000),
		WebhookMaxRetries: getEnvInt("WEBHOOK_MAX_RETRIES", 3),
		WebhookBackoffMS:  getEnvInt("WEBHOOK_BACKOFF_MS", 2000),
	}
}

// WantsEvent reports whether the given event name should be forwarded to the webhook.
func (c *Config) WantsEvent(name string) bool {
	for _, e := range c.WebhookEvents {
		if e == name || e == "*" || e == "all" {
			return true
		}
	}
	return false
}
