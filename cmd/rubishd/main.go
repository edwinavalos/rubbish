package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gopkg.in/yaml.v2"
)

type Config struct {
	Node    NodeConfig    `yaml:"node"`
	Compute ComputeConfig `yaml:"compute"`
	Storage StorageConfig `yaml:"storage"`
	Auth    AuthConfig    `yaml:"auth"`
}

type NodeConfig struct {
	ID         string `yaml:"id"`
	ListenAddr string `yaml:"listen_addr"`
}

type ComputeConfig struct {
	Provider    string            `yaml:"provider"`
	Firecracker FirecrackerConfig `yaml:"firecracker"`
}

type FirecrackerConfig struct {
	Binary    string `yaml:"binary"`
	Kernel    string `yaml:"kernel"`
	Bridge    string `yaml:"bridge"`
	BridgeIP  string `yaml:"bridge_ip"`
	VMSubnet  string `yaml:"vm_subnet"`
	SocketDir string `yaml:"socket_dir"`
}

type StorageConfig struct {
	WorkspaceRoot string `yaml:"workspace_root"`
	DBPath        string `yaml:"db_path"`
	DMPoolDevice  string `yaml:"dm_pool_device"`
	RetentionDays int    `yaml:"retention_days"`
}

type AuthConfig struct {
	TokenTTLHours int `yaml:"token_ttl_hours"`
}

func applyDefaults(cfg *Config) {
	if cfg.Node.ListenAddr == "" {
		cfg.Node.ListenAddr = ":8080"
	}
	if cfg.Compute.Provider == "" {
		cfg.Compute.Provider = "firecracker"
	}
	fc := &cfg.Compute.Firecracker
	if fc.Binary == "" {
		fc.Binary = "/usr/local/bin/firecracker"
	}
	if fc.Kernel == "" {
		fc.Kernel = "/opt/rubbish/firecracker/vmlinux"
	}
	if fc.Bridge == "" {
		fc.Bridge = "br0"
	}
	if fc.BridgeIP == "" {
		fc.BridgeIP = "172.16.0.1"
	}
	if fc.VMSubnet == "" {
		fc.VMSubnet = "172.16.0.0/24"
	}
	if fc.SocketDir == "" {
		fc.SocketDir = "/run/rubbish/fc"
	}
	if cfg.Storage.WorkspaceRoot == "" {
		cfg.Storage.WorkspaceRoot = "/opt/rubbish/workspaces"
	}
	if cfg.Storage.DBPath == "" {
		cfg.Storage.DBPath = "/opt/rubbish/data/rubbish.db"
	}
	if cfg.Storage.DMPoolDevice == "" {
		cfg.Storage.DMPoolDevice = "/dev/mapper/rubbish-pool"
	}
	if cfg.Storage.RetentionDays == 0 {
		cfg.Storage.RetentionDays = 30
	}
	if cfg.Auth.TokenTTLHours == 0 {
		cfg.Auth.TokenTTLHours = 24
	}
}

func main() {
	configPath := flag.String("config", "/etc/rubbish/config.yaml", "path to config file")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	data, err := os.ReadFile(*configPath)
	if err != nil {
		slog.Error("failed to read config file", "err", err); os.Exit(1)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		slog.Error("failed to parse config file", "err", err); os.Exit(1)
	}
	applyDefaults(&cfg)

	slog.Info("rubishd starting",
		"node_id", cfg.Node.ID,
		"listen_addr", cfg.Node.ListenAddr,
		"compute_provider", cfg.Compute.Provider,
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	// TODO: register gRPC services after buf generate

	srv := &http.Server{
		Addr:              cfg.Node.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		slog.Info("listening", "addr", cfg.Node.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "err", err); os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", "err", err)
	}
}
