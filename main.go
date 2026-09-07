// Package main implements an Open Service Broker API broker that provisions
// openGauss/GaussDB logical databases as service instances (tenants) and
// scoped user accounts as bindings. Every file owns exactly one job; the
// responsibility of each is stated at its top.
//
// main.go is wiring only: it builds the configuration, plans, state store,
// admin and broker, exposes /healthz and starts the HTTP server. Every OSB
// request is answered by brokerapi calling the Broker.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"code.cloudfoundry.org/brokerapi/v13"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := LoadConfig()
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	plans, err := LoadPlans(cfg.PlansFile)
	if err != nil {
		logger.Error("invalid plans file", "error", err)
		os.Exit(1)
	}
	store, err := OpenStore(cfg.StatePath)
	if err != nil {
		logger.Error("cannot open state file", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	server := &http.Server{
		Addr:              cfg.Host + ":" + strconv.Itoa(cfg.Port),
		ReadHeaderTimeout: 10 * time.Second,
		Handler:           newHandler(cfg, NewBroker(cfg, plans, NewAdmin(cfg, NewDB(cfg)), store, logger), logger),
	}
	go func() {
		logger.Info("broker listening", "address", server.Addr, "storage_mode", cfg.StorageMode)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server failed", "error", err)
			os.Exit(1)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
}

// newHandler wires the brokerapi endpoints under / and the health probe.
func newHandler(cfg *Config, broker *Broker, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", brokerapi.New(broker, logger, brokerapi.BrokerCredentials{
		Username: cfg.BrokerUsername,
		Password: cfg.BrokerPassword,
	}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		// Unauthenticated on purpose: for load balancers and probes.
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := broker.HealthCheck(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "unreachable", "error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	return mux
}
