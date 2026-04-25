package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Client      ClientConfig      `yaml:"client"`
	Worker      WorkerConfig      `yaml:"worker"`
	Workspace   WorkspaceConfig   `yaml:"workspace"`
	Tools       ToolConfig        `yaml:"tools"`
	Concurrency ConcurrencyConfig `yaml:"concurrency"`
	Diagnostics DiagnosticsConfig `yaml:"diagnostics"`
	Logging     LoggingConfig     `yaml:"logging"`
}

type ClientConfig struct {
	ServerURL      string `yaml:"server_url"`
	BearerToken    string `yaml:"bearer_token"`
	UserAgent      string `yaml:"user_agent"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

type WorkerConfig struct {
	ID                       string            `yaml:"id"`
	Name                     string            `yaml:"name"`
	Version                  string            `yaml:"version"`
	MaxTasks                 int               `yaml:"max_tasks"`
	Tags                     map[string]string `yaml:"tags"`
	HeartbeatIntervalSeconds int               `yaml:"heartbeat_interval_seconds"`
	LeaseWaitSeconds         int               `yaml:"lease_wait_seconds"`
}

type WorkspaceConfig struct {
	Root       string `yaml:"root"`
	Cleanup    bool   `yaml:"cleanup"`
	KeepFailed bool   `yaml:"keep_failed"`
}

type ToolConfig struct {
	FFMPEGPath         string `yaml:"ffmpeg_path"`
	AssetStudioCLIPath string `yaml:"asset_studio_cli_path"`
}

type ConcurrencyConfig struct {
	Download    int `yaml:"download"`
	AssetStudio int `yaml:"asset_studio"`
	PostProcess int `yaml:"postprocess"`
	ACB         int `yaml:"acb"`
	USM         int `yaml:"usm"`
	HCA         int `yaml:"hca"`
}

type DiagnosticsConfig struct {
	RuntimeStatsIntervalSeconds int    `yaml:"runtime_stats_interval_seconds"`
	PprofAddress                string `yaml:"pprof_address"`
	WarnHeapMB                  uint64 `yaml:"warn_heap_mb"`
	WarnGoroutines              int    `yaml:"warn_goroutines"`
}

type LoggingConfig struct {
	Level string `yaml:"level"`
	File  string `yaml:"file"`
}

func Load(path string) (*Config, error) {
	if path == "" {
		path = os.Getenv("MOE_ASSET_CLIENT_CONFIG")
	}
	if path == "" {
		path = "config.yaml"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	data = []byte(expandEnvPreservingTemplates(string(data)))
	cfg := Default()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.applyDefaults()
	return &cfg, nil
}

func Default() Config {
	cfg := Config{}
	cfg.applyDefaults()
	return cfg
}

func expandEnvPreservingTemplates(value string) string {
	return os.Expand(value, func(name string) string {
		if name == strings.ToUpper(name) {
			return os.Getenv(name)
		}
		return "${" + name + "}"
	})
}

func (c *Config) applyDefaults() {
	if c.Client.ServerURL == "" {
		c.Client.ServerURL = "http://127.0.0.1:8080"
	}
	if c.Client.UserAgent == "" {
		c.Client.UserAgent = "MoeInternal/AssetClient"
	}
	if c.Client.TimeoutSeconds == 0 {
		c.Client.TimeoutSeconds = 21600
	}
	if c.Worker.MaxTasks <= 0 {
		c.Worker.MaxTasks = 1
	}
	if c.Worker.HeartbeatIntervalSeconds <= 0 {
		c.Worker.HeartbeatIntervalSeconds = 30
	}
	if c.Worker.LeaseWaitSeconds <= 0 {
		c.Worker.LeaseWaitSeconds = 30
	}
	if c.Worker.Name == "" {
		if hostname, err := os.Hostname(); err == nil && hostname != "" {
			c.Worker.Name = hostname
		} else {
			c.Worker.Name = "moe-asset-client"
		}
	}
	if c.Worker.Version == "" {
		c.Worker.Version = "dev"
	}
	if c.Workspace.Root == "" {
		c.Workspace.Root = "./work"
	}
	if c.Tools.FFMPEGPath == "" {
		c.Tools.FFMPEGPath = "ffmpeg"
	}
	if c.Tools.AssetStudioCLIPath == "" {
		c.Tools.AssetStudioCLIPath = "AssetStudioCLI"
	}
	if c.Concurrency.Download <= 0 {
		c.Concurrency.Download = 2
	}
	if c.Concurrency.ACB <= 0 {
		c.Concurrency.ACB = 16
	}
	if c.Concurrency.USM <= 0 {
		c.Concurrency.USM = 4
	}
	if c.Concurrency.HCA <= 0 {
		c.Concurrency.HCA = 16
	}
	if c.Diagnostics.RuntimeStatsIntervalSeconds == 0 {
		c.Diagnostics.RuntimeStatsIntervalSeconds = 60
	}
	if c.Diagnostics.WarnHeapMB == 0 {
		c.Diagnostics.WarnHeapMB = 4096
	}
	if c.Diagnostics.WarnGoroutines == 0 {
		c.Diagnostics.WarnGoroutines = 2000
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "INFO"
	}
}

func (c Config) HeartbeatInterval() time.Duration {
	return time.Duration(c.Worker.HeartbeatIntervalSeconds) * time.Second
}

func (c Config) LeaseWaitSeconds() int {
	return c.Worker.LeaseWaitSeconds
}
