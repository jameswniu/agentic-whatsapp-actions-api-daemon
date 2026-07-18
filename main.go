package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

type Config struct {
	Port                   int
	APIToken               string
	DataDir                string
	ActionsEnabled         bool
	DryRunDefault          bool
	LiveMutationsEnabled   bool
	ConnectWaitTimeout     time.Duration
	OfflineSyncGracePeriod time.Duration
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func loadConfig() (*Config, error) {
	cfg := &Config{
		Port:                   8788,
		APIToken:               os.Getenv("API_TOKEN"),
		DataDir:                os.Getenv("DATA_DIR"),
		ActionsEnabled:         envBool("ACTIONS_ENABLED", false),
		DryRunDefault:          envBool("DRY_RUN", true),
		LiveMutationsEnabled:   envBool("WA_LIVE_MUTATIONS_ENABLED", false),
		ConnectWaitTimeout:     10 * time.Second,
		OfflineSyncGracePeriod: 15 * time.Second,
	}
	if p := os.Getenv("PORT"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid PORT: %w", err)
		}
		cfg.Port = n
	}
	if cfg.APIToken == "" {
		return nil, errors.New("API_TOKEN is required")
	}
	if cfg.DataDir == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		cfg.DataDir = filepath.Join(filepath.Dir(exe), "data")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	return cfg, nil
}

func main() {
	pairMode := flag.Bool("pair", false, "run interactive QR pairing in the foreground, then exit")
	pairPhone := flag.String("pair-phone", "", "pair via 8-char code for this phone number (digits only), then exit")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("config_error", "error", err.Error())
		os.Exit(2)
	}

	store, err := OpenStore(filepath.Join(cfg.DataDir, "actiond.db"))
	if err != nil {
		slog.Error("store_open_failed", "error", err.Error())
		os.Exit(2)
	}
	defer store.Close()

	wa, err := NewWAClient(cfg, store)
	if err != nil {
		slog.Error("wa_client_init_failed", "error", err.Error())
		os.Exit(2)
	}

	if *pairPhone != "" {
		if err := wa.PairWithCode(context.Background(), *pairPhone); err != nil {
			slog.Error("pairing_failed", "error", err.Error())
			os.Exit(1)
		}
		slog.Info("pairing_complete")
		return
	}
	if *pairMode {
		if err := wa.PairInteractive(context.Background()); err != nil {
			slog.Error("pairing_failed", "error", err.Error())
			os.Exit(1)
		}
		slog.Info("pairing_complete")
		return
	}

	if err := wa.Start(); err != nil {
		slog.Error("wa_start_failed", "error", err.Error())
		// Not paired yet is not fatal: serve /health so the operator can see state.
	}

	api := &API{cfg: cfg, store: store, wa: wa}
	srv := &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", cfg.Port),
		Handler:           api.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		slog.Info("service_started",
			"addr", srv.Addr,
			"provider", "whatsmeow",
			"actions_enabled", cfg.ActionsEnabled,
			"dry_run_default", cfg.DryRunDefault,
			"live_mutations_enabled", cfg.LiveMutationsEnabled,
			"data_dir", cfg.DataDir,
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http_server_failed", "error", err.Error())
			os.Exit(1)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	s := <-sig
	slog.Info("shutdown_requested", "signal", s.String())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	wa.Stop()
	slog.Info("shutdown_complete")
}
