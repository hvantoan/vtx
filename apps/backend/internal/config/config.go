package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port              string
	Host              string
	Env               string
	AuthToken         string
	DatabaseURL       string
	DBMaxOpenConns    int
	DBMaxIdleConns    int
	DBConnMaxLifetime time.Duration
	TemporalHostPort  string
	TemporalNamespace string
	TemporalQueue     string
	AgentRuntimeDir   string
	AgentTaskTimeout  time.Duration
}

func Load() *Config {
	return &Config{
		Port:              getEnv("PORT", "8080"),
		Host:              getEnv("API_HOST", "0.0.0.0"),
		Env:               getEnv("APP_ENV", "development"),
		AuthToken:         getEnv("API_AUTH_TOKEN", "vtx-dev-secret-token-change-in-production"),
		DatabaseURL:       getEnv("DATABASE_URL", "postgres://vtx_user:vtx_password@localhost:5432/vtx_platform?sslmode=disable"),
		DBMaxOpenConns:    getEnvAsInt("DB_MAX_OPEN_CONNS", 25),
		DBMaxIdleConns:    getEnvAsInt("DB_MAX_IDLE_CONNS", 5),
		DBConnMaxLifetime: getEnvAsDuration("DB_CONN_MAX_LIFETIME", 15*time.Minute),
		TemporalHostPort:  getEnv("TEMPORAL_HOST_PORT", "localhost:7233"),
		TemporalNamespace: getEnv("TEMPORAL_NAMESPACE", "default"),
		TemporalQueue:     getEnv("TEMPORAL_TASK_QUEUE", "vtx-task-queue"),
		AgentRuntimeDir:   getEnv("AGENT_RUNTIME_DIR", "./apps/agent-runtime"),
		AgentTaskTimeout:  getEnvAsDuration("AGENT_TASK_TIMEOUT", 60*time.Second),
	}
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func getEnvAsInt(key string, defaultVal int) int {
	valStr := os.Getenv(key)
	if valStr == "" {
		return defaultVal
	}
	val, err := strconv.Atoi(valStr)
	if err != nil {
		return defaultVal
	}
	return val
}

func getEnvAsDuration(key string, defaultVal time.Duration) time.Duration {
	valStr := os.Getenv(key)
	if valStr == "" {
		return defaultVal
	}
	d, err := time.ParseDuration(valStr)
	if err != nil {
		return defaultVal
	}
	return d
}
