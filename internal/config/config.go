package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Config holds all runtime settings loaded from .env and environment variables.
type Config struct {
	AppName            string
	AppEnv             string
	AppKey             string
	AppDebug           bool
	AppURL             string
	Port               string
	WorkingTimezone    string
	SwaggerEnabled     bool
	DBConnection       string
	DBHost             string
	DBPort             string
	DBDatabase         string
	DBUsername         string
	DBPassword         string
	AllowedOrigins     []string
	AllowedPatterns    []string
	TrustedProxyCIDRs  []string
	StorageRoot        string
	GithubWebhookSecret string
	VAPIDSubject       string
	VAPIDPublicKey     string
	VAPIDPrivateKey    string
	RateLimitLoginPerMin int
	RateLimitAPIPerMin   int
}

func Load(envPaths ...string) Config {
	for _, p := range envPaths {
		if p != "" {
			_ = godotenv.Load(p)
		}
	}
	if len(envPaths) == 0 {
		_ = godotenv.Load(".env")
	}

	root := envStr("STORAGE_ROOT", "")
	if root == "" {
		root = filepath.Join(".", "storage", "app")
	}

	return Config{
		AppName:             envStr("APP_NAME", "FollowUP"),
		AppEnv:              envStr("APP_ENV", "production"),
		AppKey:              envStr("APP_KEY", ""),
		AppDebug:            envBool("APP_DEBUG", false),
		AppURL:              strings.TrimRight(envStr("APP_URL", "http://localhost:8080"), "/"),
		Port:                envStr("PORT", envStr("APP_PORT", "8080")),
		WorkingTimezone:     envStr("APP_WORKING_TIMEZONE", envStr("APP_TIMEZONE", "Asia/Baghdad")),
		SwaggerEnabled:      envBool("APP_SWAGGER_ENABLED", true),
		DBConnection:        strings.ToLower(envStr("DB_CONNECTION", "mysql")),
		DBHost:              envStr("DB_HOST", "127.0.0.1"),
		DBPort:              envStr("DB_PORT", "3306"),
		DBDatabase:          envStr("DB_DATABASE", "aiicom_followup"),
		DBUsername:          envStr("DB_USERNAME", "root"),
		DBPassword:          envStr("DB_PASSWORD", ""),
		AllowedOrigins:      splitCSV(envStr("FRONTEND_ALLOWED_ORIGINS", "https://follow.ama8.uk")),
		AllowedPatterns:     splitCSV(envStr("FRONTEND_ALLOWED_ORIGIN_PATTERNS", "")),
		TrustedProxyCIDRs:   splitCSV(envStr("TRUSTED_PROXY_CIDRS", "")),
		StorageRoot:         root,
		GithubWebhookSecret: envStr("GITHUB_WEBHOOK_SECRET", ""),
		VAPIDSubject:        envStr("VAPID_SUBJECT", "mailto:support@ama8.uk"),
		VAPIDPublicKey:      strings.TrimSpace(envStr("VAPID_PUBLIC_KEY", "")),
		VAPIDPrivateKey:     strings.TrimSpace(envStr("VAPID_PRIVATE_KEY", "")),
		RateLimitLoginPerMin: envInt("RATE_LIMIT_LOGIN", 30),
		RateLimitAPIPerMin:   envInt("RATE_LIMIT_API", 300),
	}
}

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(envStr(key, "")))
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(envStr(key, ""))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func splitCSV(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		p := strings.Trim(strings.TrimSpace(part), "'\"")
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
