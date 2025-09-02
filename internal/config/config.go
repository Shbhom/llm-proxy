package config

import (
	"os"
	"strings"

	"github.com/Shbhom/llm-proxy/internal/types"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

func Load() (*types.Config, error) {
	v := viper.New()
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(".")
	v.AddConfigPath("/etc/llm-proxy/")
	v.SetEnvPrefix("PROXY")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// sane defaults
	v.SetDefault("server.listen_addr", ":8080")
	v.SetDefault("server.metrics_addr", ":9090")
	v.SetDefault("persistence.driver", "bolt")
	v.SetDefault("persistence.flush_interval_ms", 500)
	v.SetDefault("pool.selection", "erf_swrr")
	v.SetDefault("pool.startup_ramp_fraction", 0.5)
	v.SetDefault("rate_limits.rpm_default", 30)
	v.SetDefault("rate_limits.tpm_default", 40000)
	v.SetDefault("rate_limits.queue.enabled", true)
	v.SetDefault("rate_limits.queue.max_wait_ms", 15000)

	err := v.ReadInConfig() // optional
	if err != nil {
		logrus.WithError(err).Fatal("error while loading config file")
		os.Exit(1)
	}

	var cfg types.Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
