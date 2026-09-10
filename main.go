// Package main implements an Open Service Broker API broker that provisions
// openGauss/GaussDB logical databases as service instances (tenants) and
// scoped user accounts as bindings. Every file owns exactly one job; the
// responsibility of each is stated at its top.
//
// main.go is wiring only: it builds the configuration, catalog, encryptor,
// state store, admin and broker, exposes /healthz and starts the HTTP server.
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
	must(logger, err, "invalid configuration")
	data, err := LoadCatalog(cfg.PlansFile)
	must(logger, err, "invalid catalog file")
	if cfg.DBSSLMode == "disable" {
		logger.Warn("GAUSSDB_SSLMODE is disable; the admin password and binding credentials cross the network unencrypted")
	}

	encryptor, err := NewEncryptor(cfg.EncryptionKey)
	must(logger, err, "invalid encryption key")
	if cfg.EncryptionKey == "" {
		logger.Warn("STATE_ENCRYPTION_KEY is not set; binding credentials are stored in plaintext")
	}
	store, err := OpenStore(cfg, encryptor)
	must(logger, err, "cannot open state file")
	defer store.Close()

	db := NewDB(cfg)
	server := &http.Server{
		Addr:              cfg.Host + ":" + strconv.Itoa(cfg.Port),
		ReadHeaderTimeout: 10 * time.Second,
		Handler:           newHandler(cfg, NewBroker(cfg, data, NewAdmin(cfg, db), store, logger), db, logger),
	}
	go func() {
		logger.Info("broker listening", "address", server.Addr)
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

// must logs a fatal error and exits.
func must(logger *slog.Logger, err error, msg string) {
	if err != nil {
		logger.Error(msg, "error", err)
		os.Exit(1)
	}
}

// newHandler wires the brokerapi endpoints under / and the health probe.
func newHandler(cfg *Config, broker *Broker, db DB, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", brokerapi.New(broker, logger, brokerapi.BrokerCredentials{
		Username: cfg.BrokerUsername,
		Password: cfg.BrokerPassword,
	}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		// Unauthenticated on purpose: for load balancers and probes.
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "unreachable", "error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	return mux
}
