package main

import (
	"context"
	"log/slog"
	"messenger_debug/internal/config"
	"messenger_debug/internal/server"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fatih/color"
)

func main() {
	cfg := config.MustLoad()
	logger := setupLogger(cfg.Env)

	logger.Info("запуск debug service",
		slog.String("env", cfg.Env),
		slog.Int("port", cfg.HTTPPort),
		slog.String("log_dir", cfg.LogDir),
		slog.String("signal_debug", cfg.SignalDebugAddr))

	srv := server.New(server.Config{
		HTTPHost:        cfg.HTTPHost,
		HTTPPort:        cfg.HTTPPort,
		SignalDebugAddr: cfg.SignalDebugAddr,
		LogDir:          cfg.LogDir,
	}, logger)

	go func() {
		if err := srv.Start(); err != nil {
			logger.Error("сервер остановлен", slog.String("error", err.Error()))
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

	sig := <-stop
	logger.Info("получен сигнал остановки", slog.String("signal", sig.String()))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Stop(ctx); err != nil {
		logger.Error("ошибка остановки сервера", slog.String("error", err.Error()))
	}

	logger.Info("debug service остановлен")
}

func setupLogger(env string) *slog.Logger {
	var handler slog.Handler
	switch env {
	case "local":
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: slog.LevelDebug,
			ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				if a.Key == slog.TimeKey {
					a.Value = slog.StringValue(a.Value.Time().Format("15:04:05.000"))
				}
				if a.Key == slog.LevelKey {
					level := a.Value.String()
					switch level {
					case "DEBUG":
						a.Value = slog.StringValue(color.MagentaString(level))
					case "INFO":
						a.Value = slog.StringValue(color.BlueString(level))
					case "WARN":
						a.Value = slog.StringValue(color.YellowString(level))
					case "ERROR":
						a.Value = slog.StringValue(color.RedString(level))
					}
				}
				return a
			},
		})
	default:
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	}
	return slog.New(handler)
}
