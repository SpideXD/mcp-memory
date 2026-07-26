package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

type Config struct {
	// Server
	Port      string
	Host      string
	AuthToken string
	AlertURL  string
	AlertMode string

	// llama.cpp embedder
	LlamaPath string
	LlamaPort string
	LlamaHost string
	ModelPath string
	CtxSize   string
	GPULayers string

	// Cloud Embedding (used when ModelPath is an HTTP URL)
	CloudEmbeddingAPIKey string // env: CLOUD_EMBEDDING_API_KEY
	CloudEmbeddingURL    string // env: CLOUD_EMBEDDING_URL
	CloudEmbeddingModel  string // env: CLOUD_EMBEDDING_MODEL

	// Service timeouts
	StartTimeout   time.Duration
	StopTimeout    time.Duration
	HealthTimeout  time.Duration
	RequestTimeout time.Duration
	RetryAttempts  int
	RetryDelay     time.Duration
	ShutdownTimeout time.Duration

	// HTTP server
	HTTPReadTimeout time.Duration
	HTTPIdleTimeout time.Duration
	MaxBodyBytes    int64

	// Sessions
	MaxSessions          int
	SSEMessageBuffer     int
	SessionIdleTimeout   time.Duration
	SessionCleanInterval time.Duration

	// Health monitor
	HealthCheckInterval time.Duration
	ConsecutiveFailures int

	// Content size limit
	MaxContentBytes int

	// Retry backoff cap
	RetryMaxDelay time.Duration

	// Backend selection (default: "cognee-python")
	Backend Backend

	// Cognee
	CogneePort               string        // COGNEE_PORT, default "8000"
	CogneeDataDir            string        // COGNEE_DATA_DIR, default "./cognee-data"
	CogneeBinary             string        // COGNEE_BINARY, Rust binary path
	CogneePythonPath         string        // COGNEE_PYTHON_PATH, Python venv path
	CogneeLLMApiKey          string        // COGNEE_LLM_API_KEY (defaults to OPENROUTER_API_KEY if unset)
	CogneeLLMModel           string        // COGNEE_LLM_MODEL (default "deepseek/deepseek-v4-flash")
	CogneeLLMEndpoint        string        // COGNEE_LLM_ENDPOINT (default "https://openrouter.ai/api/v1")
	CogneeEmbeddingEndpoint  string        // COGNEE_EMBEDDING_ENDPOINT (default "http://localhost:8080/v1")
	CogneeEmbeddingProvider  string        // COGNEE_EMBEDDING_PROVIDER (default "openai")
	CogneeMaxConcurrentRetains int         // COGNEE_MAX_CONCURRENT_RETAINS, default 10
	CogneeRetainTimeout      time.Duration // COGNEE_RETAIN_TIMEOUT, default 900s (15 min)
	TemporalCognify          bool          // COGNEE_TEMPORAL_COGNIFY, default true
	MemoryOnly               bool          // COGNEE_MEMORY_ONLY, default true

	// Auto-improve
	AutoImproveAfterN  int           // AUTO_IMPROVE_AFTER_N, 0=disabled, default 0
	AutoImproveCooldown time.Duration // AUTO_IMPROVE_COOLDOWN, default 120s

	// Error webhook
	ErrorWebhookURL string // ERROR_WEBHOOK_URL, default "" (disabled)

	// Generic backend timeouts
	BackendRetainTimeout  time.Duration // BACKEND_RETAIN_TIMEOUT
	BackendRecallTimeout  time.Duration // BACKEND_RECALL_TIMEOUT
	BackendReflectTimeout time.Duration // BACKEND_REFLECT_TIMEOUT

	// Queue (M3)
	QueueDBPath        string        // QUEUE_DB_PATH, default "./data/queue.db"
	QueueMaxPending    int           // QUEUE_MAX_PENDING, default 1000
	QueueJobTTL        time.Duration // QUEUE_JOB_TTL, default 24h
	QueueTTLInterval   time.Duration // QUEUE_TTL_INTERVAL, default 5m
	QueueWorkerCount   int           // QUEUE_WORKER_COUNT, default 4
	QueueMaxConcurrent int           // QUEUE_MAX_CONCURRENT, default = COGNEE_MAX_CONCURRENT_RETAINS, fallback 3
}

func LoadConfig() Config {
	return Config{
		// Server
		Port:      getEnv("MCP_PORT", "8899"),
		Host:      getEnv("MCP_HOST", "0.0.0.0"),
		AuthToken: getEnv("MCP_AUTH_TOKEN", ""),
		AlertURL:  getEnv("ALERT_URL", ""),
		AlertMode: getEnv("ALERT_MODE", "optional"),

		// llama.cpp embedder
		LlamaPath: getEnv("LLAMA_PATH", "./bin/llama/llama-server"),
		LlamaPort: getEnv("LLAMA_PORT", "8080"),
		LlamaHost: getEnv("LLAMA_HOST", "0.0.0.0"),
		ModelPath: getEnv("LLAMA_MODEL_PATH", "./model/qwen3-embedding-0.6b-Q8_0.gguf"),
		CtxSize:   getEnv("LLAMA_CTX_SIZE", "8192"),
		GPULayers: getEnv("LLAMA_GPU_LAYERS", "999"),

		// Cloud Embedding (optional — only validated when ModelPath is HTTP URL)
		CloudEmbeddingAPIKey: getEnv("CLOUD_EMBEDDING_API_KEY", ""),
		CloudEmbeddingURL:    getEnv("CLOUD_EMBEDDING_URL", ""),
		CloudEmbeddingModel:  getEnv("CLOUD_EMBEDDING_MODEL", ""),

		// Service timeouts
		StartTimeout:    getEnvDuration("SERVICE_START_TIMEOUT", 120*time.Second),
		StopTimeout:     getEnvDuration("SERVICE_STOP_TIMEOUT", 5*time.Second),
		HealthTimeout:   getEnvDuration("HEALTH_CHECK_TIMEOUT", 60*time.Second),
		RequestTimeout:  getEnvDuration("MCP_REQUEST_TIMEOUT", 30*time.Second),
		RetryAttempts:   getEnvInt("MCP_RETRY_ATTEMPTS", 3),
		RetryDelay:      getEnvDuration("MCP_RETRY_DELAY", 1*time.Second),
		ShutdownTimeout: getEnvDuration("SHUTDOWN_TIMEOUT", 10*time.Second),

		// HTTP server
		HTTPReadTimeout: getEnvDuration("HTTP_READ_TIMEOUT", 10*time.Second),
		HTTPIdleTimeout: getEnvDuration("HTTP_IDLE_TIMEOUT", 120*time.Second),
		MaxBodyBytes:    int64(getEnvInt("HTTP_MAX_BODY_BYTES", 1<<20)),

		// Sessions
		MaxSessions:          getEnvInt("MCP_MAX_SESSIONS", 100),
		SSEMessageBuffer:     getEnvInt("MCP_SSE_BUFFER", 100),
		SessionIdleTimeout:   getEnvDuration("MCP_SESSION_IDLE", 30*time.Minute),
		SessionCleanInterval: getEnvDuration("MCP_SESSION_CLEAN_INTERVAL", 30*time.Second),

		// Health monitor
		HealthCheckInterval: getEnvDuration("HEALTH_CHECK_INTERVAL", 5*time.Second),
		ConsecutiveFailures: getEnvInt("HEALTH_CONSECUTIVE_FAILURES", 2),

		// Content size limit (default 1MB)
		MaxContentBytes: getEnvInt("MAX_CONTENT_BYTES", 1<<20),

		// Retry backoff cap
		RetryMaxDelay: getEnvDuration("MCP_RETRY_MAX_DELAY", 30*time.Second),

		// Backend selection
		Backend: Backend(getEnv("BACKEND", "cognee-python")),

		// Cognee
		CogneePort:               getEnv("COGNEE_PORT", "8000"),
		CogneeDataDir:            getEnv("COGNEE_DATA_DIR", "./cognee-data"),
		CogneeBinary:             getEnv("COGNEE_BINARY", ""),
		CogneePythonPath:         getEnv("COGNEE_PYTHON_PATH", ""),
		CogneeLLMApiKey:          getEnv("COGNEE_LLM_API_KEY", getEnv("OPENROUTER_API_KEY", "")),
		CogneeLLMModel:           getEnv("COGNEE_LLM_MODEL", "deepseek/deepseek-v4-flash"),
		CogneeLLMEndpoint:        getEnv("COGNEE_LLM_ENDPOINT", "https://openrouter.ai/api/v1"),
		CogneeEmbeddingEndpoint:  getEnv("COGNEE_EMBEDDING_ENDPOINT", "http://localhost:"+getEnv("LLAMA_PORT", "8080")+"/v1"),
		CogneeEmbeddingProvider:  getEnv("COGNEE_EMBEDDING_PROVIDER", "openai"),
		CogneeMaxConcurrentRetains: getEnvInt("COGNEE_MAX_CONCURRENT_RETAINS", 10),
		CogneeRetainTimeout:      getEnvDuration("COGNEE_RETAIN_TIMEOUT", 900*time.Second),
		TemporalCognify:          getEnvBool("COGNEE_TEMPORAL_COGNIFY", true),
		MemoryOnly:               getEnvBool("COGNEE_MEMORY_ONLY", true),

		// Auto-improve
		AutoImproveAfterN:  getEnvInt("AUTO_IMPROVE_AFTER_N", 0),
		AutoImproveCooldown: clampDuration(getEnvDuration("AUTO_IMPROVE_COOLDOWN", 120*time.Second), 0),

		// Error webhook
		ErrorWebhookURL: getEnv("ERROR_WEBHOOK_URL", ""),

		// Generic backend timeouts
		BackendRetainTimeout:  getEnvDuration("BACKEND_RETAIN_TIMEOUT", 60*time.Second),
		BackendRecallTimeout:  getEnvDuration("BACKEND_RECALL_TIMEOUT", 10*time.Second),
		BackendReflectTimeout: getEnvDuration("BACKEND_REFLECT_TIMEOUT", 60*time.Second),

		// Queue (M3)
		QueueDBPath:        getEnv("QUEUE_DB_PATH", "./data/queue.db"),
		QueueMaxPending:    getEnvInt("QUEUE_MAX_PENDING", 1000),
		QueueJobTTL:        getEnvDuration("QUEUE_JOB_TTL", 24*time.Hour),
		QueueTTLInterval:   getEnvDuration("QUEUE_TTL_INTERVAL", 5*time.Minute),
		QueueWorkerCount:   getEnvInt("QUEUE_WORKER_COUNT", 4),
		QueueMaxConcurrent: getEnvInt("QUEUE_MAX_CONCURRENT", getEnvInt("COGNEE_MAX_CONCURRENT_RETAINS", 3)),
	}
}

func getEnv(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" { return v }
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if v := os.Getenv(key); v != "" {
		var i int
		if _, err := fmt.Sscanf(v, "%d", &i); err == nil { return i }
	}
	return defaultValue
}

func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil { return d }
		log.Printf("WARN: invalid duration for %s=%q, using default %v", key, v, defaultValue)
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	v := strings.ToLower(os.Getenv(key))
	if v == "" {
		return defaultValue
	}
	return v == "true" || v == "1" || v == "yes"
}

// clampDuration returns d if d >= min, otherwise returns min.
func clampDuration(d, min time.Duration) time.Duration {
	if d < min {
		return min
	}
	return d
}

// isCloudURL returns true if s is an HTTP or HTTPS URL (i.e., a cloud
// service endpoint rather than a local filesystem path).
func isCloudURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// IsCloudEmbedding returns true iff ModelPath is an HTTP/HTTPS URL,
// indicating the embedding service should use a cloud endpoint.
func (c Config) IsCloudEmbedding() bool { return isCloudURL(c.ModelPath) }



// Validate checks the configuration for common mistakes.
func (c Config) Validate() error {
	if c.MaxSessions < 1 {
		return fmt.Errorf("MCP_MAX_SESSIONS must be >= 1, got %d", c.MaxSessions)
	}
	if c.MaxContentBytes < 1 {
		return fmt.Errorf("MAX_CONTENT_BYTES must be >= 1, got %d", c.MaxContentBytes)
	}
	if c.StartTimeout <= 0 || c.StopTimeout <= 0 || c.ShutdownTimeout <= 0 {
		return fmt.Errorf("timeouts must be positive")
	}

	// Branch validation per backend type
	switch c.Backend {
	case BackendCogneePython:
		// Cognee uses llama-server for embeddings, not model files directly
		// Validate Cognee Python path is resolvable
		if c.CogneePythonPath != "" {
			info, err := os.Stat(c.CogneePythonPath)
			if err != nil {
				return fmt.Errorf("COGNEE_PYTHON_PATH not found: %s: %w", c.CogneePythonPath, err)
			}
			if !info.Mode().IsRegular() || info.Size() == 0 || info.Mode()&0111 == 0 {
				return fmt.Errorf("COGNEE_PYTHON_PATH is not a valid executable: %s", c.CogneePythonPath)
			}
		}
		if c.CogneeMaxConcurrentRetains < 1 {
			return fmt.Errorf("COGNEE_MAX_CONCURRENT_RETAINS must be >= 1, got %d", c.CogneeMaxConcurrentRetains)
		}
		if c.CogneeRetainTimeout <= 0 {
			return fmt.Errorf("COGNEE_RETAIN_TIMEOUT must be positive")
		}
		if c.QueueMaxPending < 1 {
			return fmt.Errorf("QUEUE_MAX_PENDING must be >= 1, got %d", c.QueueMaxPending)
		}
		if c.QueueWorkerCount < 1 {
			return fmt.Errorf("QUEUE_WORKER_COUNT must be >= 1, got %d", c.QueueWorkerCount)
		}
		if c.QueueMaxConcurrent < 1 {
			return fmt.Errorf("QUEUE_MAX_CONCURRENT must be >= 1, got %d", c.QueueMaxConcurrent)
		}

	case BackendCogneeRust:
		// Validate Cognee binary is resolvable
		if c.CogneeBinary == "" {
			return fmt.Errorf("COGNEE_BINARY is required for cognee-rust backend")
		}
		if info, err := os.Stat(c.CogneeBinary); err != nil {
			return fmt.Errorf("COGNEE_BINARY not found: %s: %w", c.CogneeBinary, err)
		} else if !info.Mode().IsRegular() || info.Size() == 0 || info.Mode()&0111 == 0 {
			return fmt.Errorf("COGNEE_BINARY is not a valid executable: %s", c.CogneeBinary)
		}
		if c.CogneeMaxConcurrentRetains < 1 {
			return fmt.Errorf("COGNEE_MAX_CONCURRENT_RETAINS must be >= 1, got %d", c.CogneeMaxConcurrentRetains)
		}
		if c.CogneeRetainTimeout <= 0 {
			return fmt.Errorf("COGNEE_RETAIN_TIMEOUT must be positive")
		}
		if c.QueueMaxPending < 1 {
			return fmt.Errorf("QUEUE_MAX_PENDING must be >= 1, got %d", c.QueueMaxPending)
		}
		if c.QueueWorkerCount < 1 {
			return fmt.Errorf("QUEUE_WORKER_COUNT must be >= 1, got %d", c.QueueWorkerCount)
		}
		if c.QueueMaxConcurrent < 1 {
			return fmt.Errorf("QUEUE_MAX_CONCURRENT must be >= 1, got %d", c.QueueMaxConcurrent)
		}

	default:
		return fmt.Errorf("unknown BACKEND: %q (valid: cognee-python, cognee-rust)", c.Backend)
	}

	return nil
}
