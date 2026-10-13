package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"flag"
	"fmt"
	"log/slog"
	"messenger_signal/internal/admin"
	"messenger_signal/internal/cluster"
	"messenger_signal/internal/config"
	"messenger_signal/internal/groupstore"
	"messenger_signal/internal/handlers/slogpretty"
	"messenger_signal/internal/hub"
	"messenger_signal/internal/identity"
	"messenger_signal/internal/logstore"
	"messenger_signal/internal/releases"
	"messenger_signal/internal/server"
	"messenger_signal/internal/tlscert"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const identityFile = "vox-signal.key"

const (
	envLocal = "local"
	envDev   = "dev"
	envProd  = "prod"
)

func main() {
	printPubkey := flag.Bool("print-pubkey", false, "вывести pubkey сервера в stdout (для генерации hardcoded list клиента) и выйти")
	flag.Parse()

	id, err := identity.LoadOrCreate(identityFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "identity load failed: %v\n", err)
		os.Exit(1)
	}
	if *printPubkey {
		fmt.Println(id.PubKeyHex())
		return
	}

	cfg := config.MustLoad()
	logger, logFile := setupLogger(cfg.Env)
	if logFile != nil {
		defer logFile.Close()
	}

	if logFile != nil {
		logFile.SetOnOpen(redirectStderr)
	}

	logger.Info("запуск сигнального сервера",
		slog.String("env", cfg.Env),
		slog.String("server_pubkey", id.PubKeyHex()))

	// Hub — реестр пиров
	h := hub.New(logger)
	h.StartPingLoop()

	// WebSocket сервер
	srv := server.New(h, cfg.WS, cfg.TURN, cfg.SFU, logger, id)
	if cfg.WS.TLSPort > 0 {
		cert, hash, err := tlscert.LoadOrCreate(cfg.WS.TLSCert, cfg.WS.TLSKey, cfg.WS.TLSHost)
		if err != nil {
			logger.Error("TLS-сертификат узла", slog.String("error", err.Error()))
			os.Exit(1)
		}
		srv.SetTLS(cert, hash)
	}

	// Кластер узлов (docs/cluster-contract.md). CLUSTER_PEERS пуст —
	// одиночный сервер, как раньше.
	var clusterMgr *cluster.Manager
	if cfg.Cluster.Peers != "" {
		ccfg, err := cluster.ParseConfig(cfg.Cluster.SelfAddr, cfg.Cluster.Peers, cfg.Cluster.SoftLimit, id.PubKeyHex())
		if err != nil {
			logger.Error("конфиг кластера", slog.String("error", err.Error()))
			os.Exit(1)
		}
		if ccfg.Enabled() {
			clusterMgr = cluster.NewManager(ccfg, id, h, logger)
			h.SetCluster(clusterMgr)
			srv.SetCluster(clusterMgr)
			// Start — после группового relay ниже: линку сразу нужна репликация групп.
		}
	} else {
		logger.Info("кластер выключен (CLUSTER_PEERS пуст)")
	}

	// Releases store (клиентское автообновление)
	if cfg.ReleasesDir != "" {
		releasesStore := releases.NewStore(cfg.ReleasesDir)
		srv.SetReleasesStore(releasesStore)
		logger.Info("releases store enabled", slog.String("dir", cfg.ReleasesDir))
	} else {
		logger.Info("releases store disabled (RELEASES_DIR not set)")
	}

	// Group store — серверный relay для больших групп (>50 участников).
	// Включается только если задан путь к БД, иначе всё идёт через P2P как раньше.
	var groupStore *groupstore.Store
	if cfg.Groups.Enabled() {
		gs, err := groupstore.Open(cfg.Groups.DBPath, id.PubKeyHex(), logger)
		if err != nil {
			logger.Error("group store open failed",
				slog.String("path", cfg.Groups.DBPath),
				slog.String("error", err.Error()))
			os.Exit(1)
		}
		groupStore = gs
		srv.SetGroupStore(gs, cfg.Groups)
		logger.Info("group relay enabled",
			slog.String("db", cfg.Groups.DBPath),
			slog.Int("p2p_threshold", cfg.Groups.P2PThreshold),
			slog.Int("e2e_threshold", cfg.Groups.E2EThreshold),
			slog.Duration("retention", cfg.Groups.Retention))

		// Retention cleanup: раз в час удаляем сообщения старше Retention.
		// 1 час — достаточно редко чтобы не тревожить SQLite, но быстро реагирует
		// на изменение политики. SQLite удаляет лениво, VACUUM не запускаем —
		// free pages переиспользуются новыми сообщениями.
		purge := func() {
			cutoff := time.Now().Add(-cfg.Groups.Retention)
			n, err := gs.PurgeOlderThan(cutoff)
			if err != nil {
				logger.Warn("group retention purge failed",
					slog.String("error", err.Error()))
				return
			}
			if n > 0 {
				logger.Info("🧹 group retention purge",
					slog.Int64("deleted", n),
					slog.Time("cutoff", cutoff))
			}
		}
		go func() {
			// Первый прогон сразу: узел, который перезапускают чаще раза в
			// час, иначе не чистился бы никогда.
			purge()
			ticker := time.NewTicker(time.Hour)
			defer ticker.Stop()
			for range ticker.C {
				purge()
			}
		}()
	} else {
		logger.Info("group relay disabled (GROUP_DB_PATH not set)")
	}

	// Кластер стартует после группового relay: при подъёме линка узлы сразу
	// обмениваются состоянием групп.
	if clusterMgr != nil {
		if groupStore != nil {
			clusterMgr.SetGroupHandler(srv)
		}
		clusterMgr.Start()
	}

	// Debug log collection
	if cfg.Debug.Port > 0 {
		store := logstore.New(cfg.Debug.Dir, logstore.Options{
			MaxTotal:      int64(cfg.Debug.MaxTotalMB) << 20,
			MaxAge:        time.Duration(cfg.Debug.MaxAgeDays) * 24 * time.Hour,
			KeepPerSource: cfg.Debug.KeepPerSource,
		}, logger)
		srv.SetLogStore(store, "vox-signal.log")
		go func() {
			if err := srv.StartDebug(cfg.Debug.Host, cfg.Debug.Port); err != nil {
				logger.Error("debug HTTP остановлен", slog.String("error", err.Error()))
			}
		}()
		logger.Info("debug log collection включён",
			slog.String("host", cfg.Debug.Host),
			slog.Int("port", cfg.Debug.Port),
			slog.String("dir", cfg.Debug.Dir),
			slog.Int("max_total_mb", cfg.Debug.MaxTotalMB),
			slog.Int("max_age_days", cfg.Debug.MaxAgeDays))
	}

	// Admin API /admin/v1/* — после всех хранилищ: Handler собирается при Start.
	setupAdmin(srv, cfg.Admin, logger)

	// Запускаем сервер в горутине
	go func() {
		if err := srv.Start(); err != nil {
			logger.Error("сервер остановлен", slog.String("error", err.Error()))
		}
	}()

	// Graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

	sig := <-stop
	logger.Info("получен сигнал остановки", slog.String("signal", sig.String()))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if clusterMgr != nil {
		clusterMgr.Stop()
	}
	h.Stop()
	if groupStore != nil {
		if err := groupStore.Close(); err != nil {
			logger.Error("group store close failed", slog.String("error", err.Error()))
		}
	}
	if err := srv.StopDebug(ctx); err != nil {
		logger.Error("ошибка остановки debug HTTP", slog.String("error", err.Error()))
	}
	if err := srv.Stop(ctx); err != nil {
		logger.Error("ошибка остановки сервера", slog.String("error", err.Error()))
	}

	logger.Info("сервер остановлен")
}

// setupAdmin включает admin API, если ADMIN_TOKEN не короче 32 символов.
func setupAdmin(srv *server.Server, cfg config.AdminConfig, logger *slog.Logger) {
	if cfg.Token == "" {
		logger.Info("admin API выключен (ADMIN_TOKEN пуст)")
		return
	}
	if len(cfg.Token) < admin.MinTokenLen {
		logger.Error("admin API выключен: ADMIN_TOKEN короче 32 символов")
		return
	}
	var pub ed25519.PublicKey
	if cfg.ReleasePubKey != "" {
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(cfg.ReleasePubKey))
		if err != nil || len(b) != ed25519.PublicKeySize {
			logger.Error("RELEASE_PUBKEY невалиден — замена манифеста через admin API недоступна")
		} else {
			pub = ed25519.PublicKey(b)
		}
	}
	srv.SetAdmin(cfg.Token, pub)
	logger.Info("admin API включён", slog.Bool("release_pubkey", pub != nil))
}

func setupLogger(env string) (*slog.Logger, *rotatingFile) {
	switch env {
	case envLocal:
		return setupPrettySlog()
	case envDev:
		return setupJSONSlog(slog.LevelDebug)
	case envProd:
		return setupJSONSlog(slog.LevelInfo)
	default:
		return setupPrettySlog()
	}
}

func setupPrettySlog() (*slog.Logger, *rotatingFile) {
	opts := slogpretty.PrettyHandlerOptions{
		SlogOpts: &slog.HandlerOptions{
			Level: slog.LevelDebug,
		},
	}
	prettyHandler := opts.NewPrettyHandler(os.Stdout)

	// Лог-файл для отладки (дописывается при перезапуске, ротация по размеру)
	f, err := openRotatingFile("vox-signal.log", logFileMax)
	if err != nil {
		return slog.New(prettyHandler), nil
	}

	jsonHandler := slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(newMultiHandler(prettyHandler, jsonHandler)), f
}

// multiHandler — пишет в несколько slog.Handler одновременно
type multiHandler struct {
	handlers []slog.Handler
}

func newMultiHandler(handlers ...slog.Handler) *multiHandler {
	return &multiHandler{handlers: handlers}
}

func (h *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, r.Level) {
			if err := handler.Handle(ctx, r); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		handlers[i] = handler.WithAttrs(attrs)
	}
	return &multiHandler{handlers: handlers}
}

func (h *multiHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		handlers[i] = handler.WithGroup(name)
	}
	return &multiHandler{handlers: handlers}
}

func setupJSONSlog(level slog.Level) (*slog.Logger, *rotatingFile) {
	f, err := openRotatingFile("vox-signal.log", logFileMax)
	if err != nil {
		return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})), nil
	}

	// В stdout (nohup → signal-server.log) — только предупреждения и ошибки:
	// полный лог уже в vox-signal.log, дубль съедал диск узла.
	stdoutLevel := level
	if stdoutLevel < slog.LevelWarn {
		stdoutLevel = slog.LevelWarn
	}
	jsonStdout := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: stdoutLevel})
	jsonFile := slog.NewJSONHandler(f, &slog.HandlerOptions{Level: level})
	return slog.New(newMultiHandler(jsonStdout, jsonFile)), f
}
