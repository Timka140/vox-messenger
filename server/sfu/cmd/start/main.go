package main

import (
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

	log.Info("SFU listening", slog.String("addr", cfg.Addr()))
	if err := http.ListenAndServe(cfg.Addr(), mux); err != nil {
		log.Error("SFU server error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}
