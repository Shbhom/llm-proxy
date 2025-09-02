package types

import "time"

type Server struct {
	ListenAddr  string `mapstructure:"listen_addr"`
	MetricsAddr string `mapstructure:"metrics_addr"`
}
type Upstream struct {
	BaseURL string `mapstructure:"base_url"`
}
type Pool struct {
	Selection           string  `mapstructure:"selection"`
	LatencyBias         float64 `mapstructure:"latency_bias"`
	StartupRampFraction float64 `mapstructure:"startup_ramp_fraction"`
	TokenPredictor      struct {
		Enabled bool `mapstructure:"enabled"`
	} `mapstructure:"token_predictor"`
}
type RateLimits struct {
	RPMDefault int `mapstructure:"rpm_default"`
	TPMDefault int `mapstructure:"tpm_default"`
	Queue      struct {
		Enabled   bool          `mapstructure:"enabled"`
		MaxWaitMs time.Duration `mapstructure:"max_wait_ms"`
	} `mapstructure:"queue"`
}
type Config struct {
	Server      Server            `mapstructure:"server"`
	Upstream    Upstream          `mapstructure:"upstream"`
	Keys        []Key             `mapstructure:"keys"`
	Persistence PersistenceConfig `mapstructure:"persistence"`
	Pool        Pool              `mapstructure:"pool"`
	RateLimits  RateLimits        `mapstructure:"rate_limits"`
}
