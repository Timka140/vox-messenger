package main

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"messenger_sfu/internal/config"
	"messenger_sfu/internal/handler"
	"messenger_sfu/internal/room"
)

func main() {
	cfg := config.MustLoad()

	var logLevel slog.Level
	if cfg.Env == "local" {
		logLevel = slog.LevelDebug
	} else {
		logLevel = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))

	log.Info("SFU starting",
		slog.String("addr", cfg.Addr()),
		slog.String("env", cfg.Env),
		slog.Bool("auth", cfg.Secret != ""))

	mgr := room.NewManager(log)
	h := handler.New(mgr, cfg.Secret, log)

	mux := http.NewServeMux()
	mux.Handle("/ws", h)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		rooms, members := mgr.Stats()
		fmt.Fprintf(w, `{"status":"ok","rooms":%d,"members":%d}`, rooms, members)
	})

	if cfg.TLSPort > 0 {
		cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			log.Error("SFU TLS выключен: нет сертификата узла", slog.String("error", err.Error()))
		} else {
			tlsAddr := fmt.Sprintf("%s:%d", cfg.Host, cfg.TLSPort)
			srv := &http.Server{Addr: tlsAddr, Handler: mux,
				TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
			go func() {
				log.Info("SFU TLS listening", slog.String("addr", tlsAddr))
				if err := srv.ListenAndServeTLS("", ""); err != nil {
					log.Error("SFU TLS error", slog.String("error", err.Error()))
				}
			}()
		}
	}

	log.Info("SFU listening", slog.String("addr", cfg.Addr()))
	if err := http.ListenAndServe(cfg.Addr(), mux); err != nil {
		log.Error("SFU server error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}
