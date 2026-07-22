package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
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

	// llama.cpp reranker
	LlamaRerankerPort string

	// Cloud Embedding (used when ModelPath is an HTTP URL)
	CloudEmbeddingAPIKey string // env: CLOUD_EMBEDDING_API_KEY
	CloudEmbeddingURL    string // env: CLOUD_EMBEDDING_URL
	CloudEmbeddingModel  string // env: CLOUD_EMBEDDING_MODEL

	// Cloud Reranker (used when RerankerModel is an HTTP URL)
	CloudRerankerAPIKey string // env: CLOUD_RERANKER_API_KEY
	CloudRerankerURL    string // env: CLOUD_RERANKER_URL
	CloudRerankerModel  string // env: CLOUD_RERANKER_MODEL

	// Hindsight
	HindsightPath    string
	HindsightPort    string
	LLMProvider      string
	LLMModel         string
	LLMAPIKey        string
	LLMBaseURL       string
	EmbedProvider    string
	EmbedModel       string
	RerankerProvider string
	RerankerModel    string

	// Service timeouts
	StartTimeout   time.Duration
	StopTimeout    time.Duration
	HealthTimeout  time.Duration
	RequestTimeout time.Duration
	RetryAttempts  int
	RetryDelay     time.Duration
	ShutdownTimeout time.Duration

	// Worker pools
	RetainWorkers  int
	ReflectWorkers int
	JobBufferSize  int

	// Queue job timeouts
	QueuePushTimeout    time.Duration
	QueueResponseTimeout time.Duration

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

	// Hindsight API timeouts
	HindsightRetainTimeout  time.Duration
	HindsightRecallTimeout  time.Duration
	HindsightReflectTimeout time.Duration

	// Content size limit
	MaxContentBytes int

	// Circuit breaker
	CircuitBreakerThreshold int
	CircuitBreakerCooldown  time.Duration

	// Retry backoff cap
	RetryMaxDelay time.Duration
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

		// llama.cpp reranker
		LlamaRerankerPort: getEnv("LLAMA_RERANKER_PORT", "8081"),

		// Hindsight
		HindsightPath:    getEnv("HINDSIGHT_PATH", "hindsight-api"),
		HindsightPort:    getEnv("HINDSIGHT_PORT", "8888"),
		LLMProvider:      getEnv("HINDSIGHT_LLM_PROVIDER", "openrouter"),
		LLMModel:         getEnv("HINDSIGHT_LLM_MODEL", "deepseek/deepseek-v4-flash"),
		LLMAPIKey:        getEnv("OPENROUTER_API_KEY", ""),
		LLMBaseURL:       getEnv("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1"),
		EmbedProvider:    getEnv("HINDSIGHT_EMBEDDINGS_PROVIDER", "openai"),
		EmbedModel:       getEnv("HINDSIGHT_EMBEDDINGS_MODEL", "qwen3-embedding-0.6b-Q8_0.gguf"),
		RerankerProvider: getEnv("HINDSIGHT_RERANKER_PROVIDER", "cohere"),
		RerankerModel:    getEnv("HINDSIGHT_RERANKER_MODEL", "./model/bge-reranker-base-Q4_k_m.gguf"),

		// Cloud Embedding (optional — only validated when ModelPath is HTTP URL)
		CloudEmbeddingAPIKey: getEnv("CLOUD_EMBEDDING_API_KEY", ""),
		CloudEmbeddingURL:    getEnv("CLOUD_EMBEDDING_URL", ""),
		CloudEmbeddingModel:  getEnv("CLOUD_EMBEDDING_MODEL", ""),

		// Cloud Reranker (optional — only validated when RerankerModel is HTTP URL)
		CloudRerankerAPIKey: getEnv("CLOUD_RERANKER_API_KEY", ""),
		CloudRerankerURL:    getEnv("CLOUD_RERANKER_URL", ""),
		CloudRerankerModel:  getEnv("CLOUD_RERANKER_MODEL", ""),

		// Service timeouts
		StartTimeout:    getEnvDuration("SERVICE_START_TIMEOUT", 120*time.Second),
		StopTimeout:     getEnvDuration("SERVICE_STOP_TIMEOUT", 5*time.Second),
		HealthTimeout:   getEnvDuration("HEALTH_CHECK_TIMEOUT", 60*time.Second),
		RequestTimeout:  getEnvDuration("MCP_REQUEST_TIMEOUT", 30*time.Second),
		RetryAttempts:   getEnvInt("MCP_RETRY_ATTEMPTS", 3),
		RetryDelay:      getEnvDuration("MCP_RETRY_DELAY", 1*time.Second),
		ShutdownTimeout: getEnvDuration("SHUTDOWN_TIMEOUT", 10*time.Second),

		// Worker pools
		RetainWorkers:  getEnvInt("MEMORY_RETAIN_WORKERS", 2),
		ReflectWorkers: getEnvInt("MEMORY_REFLECT_WORKERS", 2),
		JobBufferSize:  getEnvInt("MEMORY_JOB_BUFFER", 100),

		// Queue job timeouts
		QueuePushTimeout:    getEnvDuration("MEMORY_QUEUE_PUSH_TIMEOUT", 5*time.Second),
		QueueResponseTimeout: getEnvDuration("MEMORY_QUEUE_RESPONSE_TIMEOUT", 60*time.Second),

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

		// Hindsight API timeouts
		HindsightRetainTimeout:  getEnvDuration("HINDSIGHT_RETAIN_TIMEOUT", 60*time.Second),
		HindsightRecallTimeout:  getEnvDuration("HINDSIGHT_RECALL_TIMEOUT", 10*time.Second),
		HindsightReflectTimeout: getEnvDuration("HINDSIGHT_REFLECT_TIMEOUT", 60*time.Second),

		// Content size limit (default 1MB)
		MaxContentBytes: getEnvInt("MAX_CONTENT_BYTES", 1<<20),

		// Circuit breaker
		CircuitBreakerThreshold: getEnvInt("HINDSIGHT_CIRCUIT_BREAKER_THRESHOLD", 5),
		CircuitBreakerCooldown:  getEnvDuration("HINDSIGHT_CIRCUIT_BREAKER_COOLDOWN", 30*time.Second),

		// Retry backoff cap
		RetryMaxDelay: getEnvDuration("MCP_RETRY_MAX_DELAY", 30*time.Second),
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

// isCloudURL returns true if s is an HTTP or HTTPS URL (i.e., a cloud
// service endpoint rather than a local filesystem path).
func isCloudURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// IsCloudEmbedding returns true iff ModelPath is an HTTP/HTTPS URL,
// indicating the embedding service should use a cloud endpoint.
func (c Config) IsCloudEmbedding() bool { return isCloudURL(c.ModelPath) }

// IsCloudReranker returns true iff RerankerModel is an HTTP/HTTPS URL,
// indicating the reranker service should use a cloud endpoint.
func (c Config) IsCloudReranker() bool { return isCloudURL(c.RerankerModel) }

// Validate checks the configuration for common mistakes.
func (c Config) Validate() error {
	if c.LLMAPIKey == "" {
		return fmt.Errorf("OPENROUTER_API_KEY is required")
	}
	if c.MaxSessions < 1 {
		return fmt.Errorf("MCP_MAX_SESSIONS must be >= 1, got %d", c.MaxSessions)
	}
	if c.MaxContentBytes < 1 {
		return fmt.Errorf("MAX_CONTENT_BYTES must be >= 1, got %d", c.MaxContentBytes)
	}
	if c.RetainWorkers < 1 || c.ReflectWorkers < 1 {
		return fmt.Errorf("worker count must be >= 1 (retain=%d, reflect=%d)", c.RetainWorkers, c.ReflectWorkers)
	}
	if c.StartTimeout <= 0 || c.StopTimeout <= 0 || c.ShutdownTimeout <= 0 {
		return fmt.Errorf("timeouts must be positive")
	}
	// Validate model files exist (skip for cloud endpoints)
	for _, path := range []string{c.ModelPath, c.RerankerModel} {
		if isCloudURL(path) {
			continue
		}
		if !filepath.IsAbs(path) {
			wd, _ := os.Getwd()
			path = filepath.Join(wd, path)
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return fmt.Errorf("model file not found: %s", path)
		}
	}

	// Cloud embedding: if configured, all three fields are required
	if c.IsCloudEmbedding() {
		if strings.TrimSpace(c.CloudEmbeddingAPIKey) == "" {
			return fmt.Errorf("CLOUD_EMBEDDING_API_KEY is required when LLAMA_MODEL_PATH is a cloud URL")
		}
		if strings.TrimSpace(c.CloudEmbeddingURL) == "" {
			return fmt.Errorf("CLOUD_EMBEDDING_URL is required when LLAMA_MODEL_PATH is a cloud URL")
		}
		if strings.TrimSpace(c.CloudEmbeddingModel) == "" {
			return fmt.Errorf("CLOUD_EMBEDDING_MODEL is required when LLAMA_MODEL_PATH is a cloud URL")
		}
	}

	// Cloud reranker: if configured, all three fields are required
	if c.IsCloudReranker() {
		if strings.TrimSpace(c.CloudRerankerAPIKey) == "" {
			return fmt.Errorf("CLOUD_RERANKER_API_KEY is required when HINDSIGHT_RERANKER_MODEL is a cloud URL")
		}
		if strings.TrimSpace(c.CloudRerankerURL) == "" {
			return fmt.Errorf("CLOUD_RERANKER_URL is required when HINDSIGHT_RERANKER_MODEL is a cloud URL")
		}
		if strings.TrimSpace(c.CloudRerankerModel) == "" {
			return fmt.Errorf("CLOUD_RERANKER_MODEL is required when HINDSIGHT_RERANKER_MODEL is a cloud URL")
		}
	}

	return nil
}
