package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
	"vox_app/internal/config"
	"vox_app/internal/handlers/slogpretty"
	"vox_app/internal/identity"
	"vox_app/internal/logsafe"
	"vox_app/internal/network"
	"vox_app/internal/services"
	"vox_app/internal/services/files"
	"vox_app/internal/session"
	"vox_app/internal/storage"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

const (
	windowStateKey = "window_main"
	updateFlag     = "--from-update"
	defaultWinW    = 1400 // вмещает список, сцену звонка ≥600 и панель 360 без рейки
	defaultWinH    = 800
	minVisibleW    = 200
	minVisibleH    = 60
	saveDebounce   = 500 * time.Millisecond
)

type windowState struct {
	X         int  `json:"x"`
	Y         int  `json:"y"`
	Width     int  `json:"width"`
	Height    int  `json:"height"`
	Maximised bool `json:"maximised"`
}

//go:embed all:frontend
var assets embed.FS

//go:embed frontend/src/icon/icon_256x256.png
var appIcon []byte

// AppVersion — версия клиента, инжектится при сборке через ldflags:
//
//	-X main.AppVersion=v0.00.000.0001
//
// По умолчанию "dev" — dev-сборки не триггерят автообновление.
var AppVersion = "dev"

// UpdatePublicKey — публичный Ed25519 ключ для верификации подписи релизов.
// Инжектится при сборке через ldflags:
//
//	-X main.UpdatePublicKey=<base64>
//
// Пустая строка → автообновление выключено (верификация невозможна).
var UpdatePublicKey = ""

const (
	envLocal = "local"
	envDev   = "dev"
	envProd  = "prod"
)

func main() {
	// Detect и УДАЛЯЕМ флаги --from-update / --wait-pid из os.Args ДО того как
	// config вызовет flag.Parse(). Иначе flag package видит неизвестный флаг и
	// делает os.Exit(2) беззвучно (binary -H windowsgui без консоли) — новый
	// процесс умирает молча сразу после старта, ни одной строки в лог не попадает.
	relaunchedAfterUpdate, waitPid := stripUpdateFlag()
	devBindSpec, devForceRelay := stripDevFlags()
	ensureWebView2()
	cfg := config.MustLoad()
	logger, logFile := setupLogger(cfg.Env)
	if logFile != nil {
		defer logFile.Close()
	}

	if logFile != nil {
		redirectStderr(logFile)
	}

	logger.Info("Запуск приложения", slog.String("env", cfg.Env))

	// Dev-bind: --bind-iface форсит весь сетевой трафик (signaling + WebRTC)
	// через указанный NIC. Используется для тестирования: на хосте с активным
	// VPN запустить App2 с --bind-iface=<physical-NIC>, чтобы трафик шёл мимо
	// VPN-туннеля и две локальные копии видели друг друга через интернет.
	if devBindSpec != "" {
		ip, ifaceName, err := network.ResolveDevBindSpec(devBindSpec)
		if err != nil {
			logger.Error("dev-bind: невалидное значение --bind-iface",
				slog.String("spec", devBindSpec),
				slog.String("err", err.Error()))
		} else {
			network.SetDevBindIP(ip, ifaceName)
			logger.Warn("dev-bind АКТИВЕН — весь трафик через указанный интерфейс",
				slog.String("iface", ifaceName),
				slog.String("ip", ip.String()))
		}
	}
	// --force-relay: ICETransportPolicy=Relay для всех PC. Без этого две
	// локальные копии находят host-пару через loopback (см. ICE selected pair
	// host=192.168.x.x → host=192.168.x.x), и тест "через интернет" фактически
	// идёт по локалке. С relay App2 предложит только TURN-кандидата →
	// удалённый peer пойдёт через VPS → реальный сетевой путь.
	if devForceRelay {
		network.SetForceRelay(true)
		logger.Warn("dev: ICETransportPolicy ПРИНУДИТЕЛЬНО Relay — все P2P через TURN")
	}

	// Если запущены через autoupdate — ждём смерти старого процесса перед тем
	// как открывать БД. Иначе SQLite WAL race: старый процесс ещё держит lock
	// на vox.db-wal через CGO sqlite3, новый процесс виснет в C-коде на
	// PRAGMA journal_mode=WAL (busy_timeout не покрывает file-level locks).
	// До этого фикса юзер видел зависшее окно после обновления.
	if relaunchedAfterUpdate && waitPid > 0 {
		waitForParentExit(logger, waitPid, 30*time.Second)
	} else if relaunchedAfterUpdate {
		// Fallback для случая если update_apply_*.go не передал PID:
		// просто sleep, как было раньше.
		time.Sleep(2 * time.Second)
	}

	// Директория данных — рядом с исполняемым файлом
	dataDir := "."

	// Профиль, созданный из секретной фразы, оставляет базу, перешифрованную
	// ключом из фразы, в vox.db.new и ключ в vox.key.new — ставим их на место
	// до открытия БД. Если старый процесс не успел закрыться, vox.db ещё занята —
	// повторяем; не вышло — файлы остаются до следующего старта, а в текущей
	// базе профиля нет (ApplyMnemonic его туда не пишет), и покажется экран создания.
	swap, err := storage.ApplyPendingRekey(dataDir)
	for i := 0; err != nil && i < 20; i++ {
		time.Sleep(500 * time.Millisecond)
		swap, err = storage.ApplyPendingRekey(dataDir)
	}
	switch {
	case err != nil:
		logger.Error("подмена базы после создания профиля не удалась", slog.String("error", err.Error()))
	case swap == storage.SwapApplied:
		logger.Info("база перешифрована ключом из секретной фразы")
		// Восстановление фразой уносит копию .bak в подменённую базу —
		// сам .bak больше не нужен (при новом профиле его уже нет).
		removeDBFiles(filepath.Join(dataDir, "vox.db.bak"))
	case swap == storage.SwapResumed:
		logger.Warn("прерванная подмена базы доведена до конца")
	case swap == storage.SwapPartial:
		logger.Warn("найдена половина подготовленной перешифровки базы — удалена")
	}

	// Загружаем (или создаём) ключ шифрования БД
	dbKeyPath := filepath.Join(dataDir, "vox.key")
	dbKey, err := identity.LoadOrCreateDBKey(dbKeyPath)
	if err != nil {
		log.Fatal("DB key error: ", err)
	}

	// Открываем зашифрованную БД
	dbPath := filepath.Join(dataDir, "vox.db")
	bakPath := dbPath + ".bak"
	var recoveryMode bool
	store, err := storage.Open(dbPath, dbKey)
	if err != nil {
		// vox.db не открылась — пробуем .bak (после восстановления vox.key из бэкапа)
		if _, bakErr := os.Stat(bakPath); bakErr == nil {
			logger.Info("пробую открыть .bak с текущим ключом")
			bakStore, bakOpenErr := storage.Open(bakPath, dbKey)
			if bakOpenErr == nil {
				// .bak открылась! Значит vox.key восстановлен, а vox.db — мусор
				bakStore.Close()
				// удаляем пустую/испорченную вместе с её WAL
				removeDBFiles(dbPath)
				moveDBFiles(logger, bakPath, dbPath)
				store, err = storage.Open(dbPath, dbKey)
				if err != nil {
					log.Fatal("Storage error (после восстановления .bak): ", err)
				}
				logger.Info("БД восстановлена из .bak")
			}
		}

		// Если всё ещё не открыта — сохраняем копию и пересоздаём
		if store == nil {
			if _, statErr := os.Stat(dbPath); statErr == nil {
				logger.Warn("БД недоступна (ключ утерян?), сохраняю копию в .bak и пересоздаю",
					slog.String("error", err.Error()),
					slog.String("backup", bakPath))
				if _, bakErr := os.Stat(bakPath); bakErr == nil {
					// В .bak уже лежит неразобранная история, а vox.db — пустая
					// база прошлого экрана восстановления: не затираем .bak.
					removeDBFiles(dbPath)
				} else {
					moveDBFiles(logger, dbPath, bakPath)
				}
			}
			store, err = storage.Open(dbPath, dbKey)
			if err != nil {
				log.Fatal("Storage error (после пересоздания): ", err)
			}
			recoveryMode = true
		}
	}
	defer store.Close()

	// .bak лежит, пока пользователь не решил его судьбу (восстановить фразой или
	// удалить): после перезапуска свежая база открывается, но экран — всё равно
	// восстановление, иначе создание профиля упрётся в phrase_mismatch.
	// Только если в открытой базе нет профиля: рабочий профиль рядом с
	// забытым .bak не прячем за экраном восстановления — оттуда «новый
	// профиль» перезаписал бы его ключ.
	if _, bakErr := os.Stat(bakPath); bakErr == nil {
		var n int
		if err := store.DB().QueryRow("SELECT COUNT(*) FROM identity").Scan(&n); err != nil {
			log.Fatal("Storage error (проверка профиля): ", err)
		}
		if n == 0 {
			recoveryMode = true
		} else {
			logger.Warn("рядом с рабочей базой лежит старая .bak, не трогаю", slog.String("backup", bakPath))
		}
	}

	// Сессия
	sess := session.New()

	// BlobStore для attachments — лежит рядом с vox.db в dataDir/attachments/.
	// Шифруется AES-GCM тем же 32-байтным ключом что и БД (vox.key).
	blobs, err := storage.NewBlobStore(dataDir, dbKey)
	if err != nil {
		log.Fatal("BlobStore error: ", err)
	}

	// AssetBridge раздаёт /vox-attach/<sha> и /vox-local/<token> прямо в WebView2
	// без копирования через JS heap. Регистрируется Middleware'ом в Wails ниже.
	// Single-file модель: vox-attach отдаёт файл из files.local_path (готовый
	// файл в Vox Downloads). Если local_path пуст или файла нет на диске → 404,
	// фронт показывает Download.
	assetBridge := services.NewAssetBridge(blobs, store.DB(), logger)

	// Сервисы — порядок создания соблюдает линейный DAG зависимостей:
	// сервис получает все свои зависимости в конструкторе, кроме циклических
	// (network ↔ friends/groups/voice), которые разруливаются через Wire() ниже.
	appSvc := services.NewAppService(logger)
	appSvc.SetLogControl(logLevel, logBaseLevel, filepath.Join(dataDir, "vox-app.log"))
	if recoveryMode {
		appSvc.SetRecoveryMode(true)
	}
	networkSvc := services.NewNetworkService(logger, sess, store)
	identitySvc := services.NewIdentityService(logger, sess, store, dataDir, networkSvc)
	notifySvc := services.NewNotifyService(logger, identitySvc.GetSetting)
	groupsSvc := services.NewGroupsService(logger, sess, store, networkSvc)
	voiceSvc := services.NewVoiceService(logger, store, sess, networkSvc, groupsSvc)
	meshSvc := services.NewMeshtasticService(logger, store, sess)
	messagingSvc := services.NewMessagingService(logger, sess, store, networkSvc, voiceSvc, groupsSvc, meshSvc)
	messagingSvc.SetBlobStore(blobs)
	messagingSvc.SetAssetBridge(assetBridge)
	// Фото профиля и групп: обмен хешами по P2P, отдача через /vox-avatar/.
	avatarSvc := services.NewAvatarService(logger, sess, store, networkSvc, groupsSvc)
	messagingSvc.SetAvatarService(avatarSvc)
	// GroupsService нужен MessagingService для очистки/загрузки attachments
	// (избегаем circular dep через callback'и).
	groupsSvc.SetUnlinkAttachments(messagingSvc.UnlinkAttachmentsForMessage)
	groupsSvc.SetLoadAttachments(messagingSvc.LoadAttachmentsForMessage)
	// При reconnect peer'а — перезапуск chunked-доставки attachment-сообщений
	// (DeliverPendingGroupMessages → restartAttachmentDelivery → sendAttachmentsToPeer).
	groupsSvc.SetRestartAttachmentDelivery(messagingSvc.RestartAttachmentDelivery)
	// Periodic GC stale receiver-assemblies (>5 минут без чанков).
	messagingSvc.StartAttachmentsGC()

	// FilesService — single-file pipeline: чанки идут напрямую WriteAt по offset
	// в .vox-tmp/<file>.partial, sender читает из local_path по offset.
	// См. internal/services/files/.
	filesSvc := files.New(logger, sess, store.DB())
	filesSvc.SetNetwork(networkSvc)
	filesSvc.SetGroups(groupsSvc)
	// Messaging adapter — передаём ссылку на NetworkService.emitEvent через
	// тонкий wrapper, чтобы не таскать весь интерфейс.
	filesSvc.SetMessaging(networkSvc)
	// При DeleteMessage / DeleteGroupMessage очищаются и file-данные:
	// dec refcount → если 0, удаляем local-файл из Vox Downloads.
	messagingSvc.SetCASCleanup(filesSvc.UnlinkFilesForMessage)
	// Retry pending файлов при reconnect: messaging.DeliverPendingMessages для
	// type='attachment' с delivered=0 первым делом смотрит message_files и
	// зовёт этот хук вместо legacy sendAttachmentsToPeer.
	messagingSvc.SetFilesRetryUploadDM(filesSvc.RetryUploadDM)
	// То же для групп: DeliverPendingGroupMessages → RetryUploadGroup.
	groupsSvc.SetFilesRetryUploadGroup(filesSvc.RetryUploadGroup)

	// Роутер attach-фреймов: сначала пробуем новый CAS pipeline (frame types
	// 0x10..0x14 — file_manifest/chunk_*/decline). Если frame не наш —
	// legacy attachments (0x01 chunk, 0x02 complete) идут в MessagingService.
	networkSvc.SetOnAttachMessage(func(fromPubKey string, data []byte) {
		if filesSvc.HandleFrame(fromPubKey, data) {
			return
		}
		messagingSvc.HandleIncomingAttachBinary(fromPubKey, data)
	})

	// Resume любых незавершённых downloads на старте (peers могут быть offline,
	// тогда startDownload вернёт early; они проснутся при peer-online).
	go filesSvc.ResumePendingDownloads()
	// Periodic GC: pending attachment messages > 7 дней + orphan blobs.
	messagingSvc.StartCleanupLoop()
	audioSvc := services.NewAudioService(logger, sess, store, networkSvc, voiceSvc, groupsSvc)
	callsSvc := services.NewCallsService(logger, sess, store, networkSvc, voiceSvc)
	screenSvc := services.NewScreenShareService(logger, store, sess, networkSvc, groupsSvc, voiceSvc)
	friendsSvc := services.NewFriendsService(logger, sess, store, networkSvc)
	network.AppVersion = AppVersion
	updateSvc := services.NewUpdateService(logger, AppVersion, UpdatePublicKey, networkSvc.ActiveSignalHTTPBase)
	updateSvc.CleanupOldBackups()

	// Graceful shutdown при выходе
	defer callsSvc.Shutdown()
	defer voiceSvc.Shutdown()
	defer networkSvc.Shutdown()
	defer meshSvc.Shutdown()

	// Циклические зависимости: networkSvc не знал о friends/groups/voice в своём
	// конструкторе (они зависят от него), теперь wire их обратно одним вызовом.
	networkSvc.Wire(friendsSvc, groupsSvc, voiceSvc)

	// Инициализация сервисов, требующая уже wired зависимостей.
	voiceSvc.InitVoiceCallbacks()
	voiceSvc.LoadSFUSettings()
	voiceSvc.LoadOpusSettings()
	voiceSvc.LoadVADSettings()
	voiceSvc.LoadNeuralNSSettings()
	groupsSvc.LoadRelaySettings()
	callsSvc.InitCallbacks()
	screenSvc.InitCallbacks()

	// EventBus: развязка NetworkService от CallsService/GroupsService/UpdateService.
	// Сервисы подписываются на топики, не зная друг о друге. NetworkService эмитит.
	bus := services.NewEventBus(logger)
	networkSvc.SetEventBus(bus)

	// Подписчики: CallsService — retry/reconnect/cleanup при изменении статуса пира.
	bus.Subscribe(services.TopicPeerOnline, func(p any) {
		if peerID, ok := p.(string); ok {
			callsSvc.RetryCallInvite(peerID)
			callsSvc.HandlePeerOnline(peerID)
		}
	})
	bus.Subscribe(services.TopicPeerOffline, func(p any) {
		if peerID, ok := p.(string); ok {
			callsSvc.HandlePeerOffline(peerID)
		}
	})

	// GroupsService: auto-connect участников общих групп при peer_online,
	// доставка pending messages и actions при открытии DataChannel.
	bus.Subscribe(services.TopicPeerOnline, func(p any) {
		if peerID, ok := p.(string); ok {
			groupsSvc.OnPeerOnlineForGroups(peerID)
			// Заявки на вступление по ссылке, ждущие этого владельца.
			groupsSvc.OnJoinPeerReady(peerID)
			// FilesService: рестарт незавершённых downloads с этого peer'а.
			filesSvc.OnPeerOnline(peerID)
		}
	})
	// Files PC поднимается лениво и позже media PC. Если на TopicP2PReady
	// manifest не ушёл («files PC not ready» — glare на files PC давал answer
	// через ~8-12 с), повторяем доставку в момент фактического открытия
	// attachments DC. DeliverPendingMessages идемпотентна: доставленное
	// повторно не шлётся, а pending-вложения уходят через RetryUploadDM.
	bus.Subscribe(services.TopicAttachReady, func(p any) {
		if pubKey, ok := p.(string); ok {
			messagingSvc.DeliverPendingMessages(pubKey)
			filesSvc.OnPeerOnline(pubKey)
		}
	})
	bus.Subscribe(services.TopicP2PReady, func(p any) {
		if pubKey, ok := p.(string); ok {
			groupsSvc.DeliverPendingGroupMessages(pubKey)
			groupsSvc.DeliverPendingGroupActions(pubKey)
			groupsSvc.OnJoinPeerReady(pubKey)
			// FilesService: возобновить pending downloads и при P2P-реконнекте.
			// Короткий обрыв сети у пира (< pongWait сигналки) не даёт
			// peer_offline/peer_online, поэтому TopicPeerOnline не приходит —
			// а pullLoop уже встал на паузу (peer offline) и ждал перезапуска.
			// Проверено 2026-09-07: Wi-Fi off 30s → загрузка висла на 10%.
			filesSvc.OnPeerOnline(pubKey)
		}
	})

	// UpdateService: информация о последнем релизе из auth_ok сигналки.
	// Также при каждом TopicLatestRelease запускаем self-heal ассетов в фоне:
	// проверяем что файлы текущей версии (модели, runtime DLL) не пропали и
	// не битые, перекачиваем при необходимости. Идемпотентно — на здоровом
	// инсталле возвращает no-op после одного manifest fetch (~5-10KB).
	// Не trigger sync.Once: если первая попытка fail (например нет сети) —
	// следующий reconnect повторит.
	bus.Subscribe(services.TopicLatestRelease, func(p any) {
		if info, ok := p.(*network.LatestReleaseInfo); ok {
			updateSvc.OnLatestReleaseFromAuthOK(info)
		}
		go func() {
			res, err := updateSvc.RepairAssets()
			if err != nil {
				logger.Debug("self-heal skipped", slog.String("reason", err.Error()))
				return
			}
			if res.Repaired > 0 {
				logger.Info("🔧 self-healed missing assets", slog.Int("count", res.Repaired))
			}
		}()
	})

	networkSvc.SetLogFilePath(filepath.Join(dataDir, "vox-app.log"))

	app := application.New(application.Options{
		Name: "Vox",
		Icon: appIcon,
		Services: []application.Service{
			application.NewService(appSvc),
			application.NewService(notifySvc),
			application.NewService(identitySvc),
			application.NewService(networkSvc),
			application.NewService(messagingSvc),
			application.NewService(avatarSvc),
			application.NewService(friendsSvc),
			application.NewService(groupsSvc),
			application.NewService(voiceSvc),
			application.NewService(audioSvc),
			application.NewService(callsSvc),
			application.NewService(screenSvc),
			application.NewService(updateSvc),
			application.NewService(meshSvc),
			application.NewService(services.NewFilesService(filesSvc)),
		},
		Assets: application.AssetOptions{
			Handler:    application.BundledAssetFileServer(assets),
			Middleware: assetBridge.Middleware,
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
		Windows: application.WindowsOptions{
			// Только ENV=local: VOX_REMOTE_DEBUG_PORT=9222 открывает CDP-порт
			// WebView2 для автоматизированного UI-тестирования (скриншоты,
			// консоль, eval). В dev/prod игнорируется.
			AdditionalBrowserArgs: remoteDebugArgs(cfg.Env),
		},
	})

	// Подключаем Wails events к NetworkService
	networkSvc.SetEmitter(func(eventName string, data any) {
		app.Event.Emit(eventName, data)
	})

	// Подключаем emitter к UpdateService
	updateSvc.SetEmitter(func(eventName string, data any) {
		app.Event.Emit(eventName, data)
	})

	// MeshtasticService: события node-update / status-change во фронтенд +
	// автостарт если фича включена в settings. Synchronous: фронту нужен
	// корректный enabled-state по первому запросу Status, иначе toggle
	// показывает OFF до завершения goroutine — пользователь думает что
	// настройка слетела (она нет).
	meshSvc.SetEmitter(func(eventName string, data any) {
		app.Event.Emit(eventName, data)
	})
	meshSvc.AutoStart()

	// Когда UpdateService применил обновление — завершить приложение
	// (новый процесс уже запущен через exec.Command)
	// Выход после обновления/перезапуска — прямым вызовом, не через app.Event:
	// Emit + Quit из обработчика того же события взаимно блокируются в Wails v3 alpha.
	updateSvc.SetQuit(func() {
		logger.Info("update applied, quitting for new process")
		app.Quit()
	})
	// Профиль создан или восстановлен из секретной фразы — новый процесс уже
	// запущен, он подменит базу при старте.
	identitySvc.SetQuit(func() {
		logger.Info("relaunch after profile setup, quitting for new process")
		app.Quit()
	})

	// Автоочистка старых звонков
	go callsSvc.CleanupOldCalls()

	// Native window background = под выбранную тему. Иначе при быстром
	// resize виден белый default фон Windows (CSS-фон html/body внутри
	// WebView2 не успевает перерисоваться между кадрами). Тема читается
	// из БД (settings.ui_theme), сохраняется фронтом при смене темы.
	// По умолчанию dark если ничего не сохранено.
	winBg := application.RGBA{Red: 0x24, Green: 0x28, Blue: 0x30, Alpha: 0xff} // dark --bg-app
	if theme, err := identitySvc.GetSetting("ui_theme"); err == nil && theme == "light" {
		winBg = application.RGBA{Red: 0xf0, Green: 0xf2, Blue: 0xf5, Alpha: 0xff} // light --bg-app
	}

	mainWinOpts := application.WebviewWindowOptions{
		Title:  "Vox",
		Width:  defaultWinW,
		Height: defaultWinH,
		// Узкое окно (фаза 5c, макет Narrow): меньше 800 раскладка уже не
		// помещается даже с рейкой вместо списка и выдвижной панелью.
		MinWidth:         800,
		MinHeight:        560,
		URL:              "/",
		DevToolsEnabled:  cfg.Env == envLocal,
		BackgroundColour: winBg,
		// Перетаскивание файлов в чат: рантайм Wails ловит drop на элементах с
		// data-file-drop-target (оверлей «как фото / как файл» в index.html) и
		// отдаёт пути в WindowFilesDropped ниже.
		EnableFileDrop: true,
	}
	saved, hasSaved := loadWindowState(logger, identitySvc)
	if hasSaved {
		logger.Info("window state: loaded",
			slog.Int("x", saved.X), slog.Int("y", saved.Y),
			slog.Int("w", saved.Width), slog.Int("h", saved.Height),
			slog.Bool("max", saved.Maximised))
		applyWindowState(logger, &mainWinOpts, saved, app)
	} else {
		fitDefaultWindow(&mainWinOpts, app)
		logger.Info("window state: none saved, using defaults",
			slog.Int("w", mainWinOpts.Width), slog.Int("h", mainWinOpts.Height))
	}
	mainWin := app.Window.NewWithOptions(mainWinOpts)

	// Файлы брошены на зону оверлея в чате → фронт (composer-attach.js)
	// показывает их в полосе превью. target — id зоны: drop-as-photo | drop-as-file.
	mainWin.OnWindowEvent(events.Common.WindowFilesDropped, func(e *application.WindowEvent) {
		ctx := e.Context()
		paths := ctx.DroppedFiles()
		if len(paths) == 0 {
			return
		}
		target := ""
		if d := ctx.DropTargetDetails(); d != nil {
			target = d.ElementID
		}
		app.Event.Emit("vox:files-dropped", map[string]any{"paths": paths, "target": target})
	})

	// Окно своих уведомлений (frontend/notify.html): frameless, поверх всех,
	// создаётся скрытым и показывается NotifyService.Resize в правом нижнем
	// углу рабочей области основного монитора. ExStyle задаём целиком:
	// WS_EX_NOACTIVATE — показ и клики не забирают фокус у активного
	// приложения (иначе тост выбивал бы из игры/редактора), WS_EX_TOOLWINDOW
	// — не светится в таскбаре, WS_EX_TOPMOST — поверх окон. Константы
	// числом: пакет w32 windows-only, а main.go собирается и под другие ОС.
	notifyWin := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             "notify",
		Title:            "Vox notifications",
		Width:            services.NotifyWindowWidth,
		Height:           120,
		URL:              "/notify.html",
		AlwaysOnTop:      true,
		Frameless:        true,
		Hidden:           true,
		DisableResize:    true,
		BackgroundColour: winBg,
		DevToolsEnabled:  cfg.Env == envLocal,
		Windows: application.WindowsWindow{
			HiddenOnTaskbar: true,
			ExStyle:         0x08000000 | 0x00000080 | 0x00000008, // NOACTIVATE | TOOLWINDOW | TOPMOST
		},
	})
	notifySvc.SetWindow(&notifyWindowWrapper{app: app, win: notifyWin, main: mainWin})
	notifySvc.SetEmitter(func(name string, data any) { app.Event.Emit(name, data) })
	notifySvc.SetFocusMain(func() {
		mainWin.UnMinimise()
		mainWin.Show()
		mainWin.Focus()
	})
	// Приглашение в групповой звонок — карточкой в окне уведомлений.
	// Личный звонок — отдельное окно по центру (ниже).
	callsSvc.SetIncomingNotifier(notifySvc.ShowIncomingCall)

	// Окно входящего личного звонка (frontend/incoming-call.html): по центру
	// экрана, поверх всех, без захвата фокуса (WS_EX_NOACTIVATE, как окно
	// уведомлений), не в таскбаре. Крестик / Alt+F4 = отклонить (onClosed).
	// Тема — из настроек, чтобы окно совпадало с главным.
	callsSvc.SetWindowCreator(func(title string, w, h int, urlPath string, onClosed func()) services.CallPopup {
		theme := "dark"
		if v, err := identitySvc.GetSetting("ui_theme"); err == nil && v != "" {
			theme = v
		}
		popup := app.Window.NewWithOptions(application.WebviewWindowOptions{
			Name:             "incoming-call",
			Title:            title,
			Width:            w,
			Height:           h,
			URL:              urlPath + "&theme=" + theme,
			AlwaysOnTop:      true,
			Frameless:        true,
			Hidden:           true,
			DisableResize:    true,
			InitialPosition:  application.WindowCentered,
			BackgroundColour: winBg,
			DevToolsEnabled:  cfg.Env == envLocal,
			Windows: application.WindowsWindow{
				HiddenOnTaskbar: true,
				ExStyle:         0x08000000 | 0x00000080 | 0x00000008, // NOACTIVATE | TOOLWINDOW | TOPMOST
			},
		})
		popup.OnWindowEvent(events.Common.WindowClosing, func(_ *application.WindowEvent) { onClosed() })
		return &callPopupWrapper{win: popup, main: mainWin}
	})

	// Окно демонстрации экрана (frontend/screen-view.html): закреплённая
	// демонстрация крупно в отдельном окне поверх всех, resizable, свой
	// заголовок (frameless). Кадры приходят тем же vox:screen-frame.
	// Окон может быть несколько (по одному на пира): своё имя и заголовок
	// (панель задач, Alt+Tab), каждое следующее — каскадом (slot).
	screenSvc.SetViewerWindowFactory(func(peerID, title, query string, slot int, onClosed func()) services.ViewerWindow {
		short := peerID
		if len(short) > 12 {
			short = short[:12]
		}
		winTitle := "Vox — screen"
		if title != "" {
			winTitle = "Vox — " + title
		}
		win := app.Window.NewWithOptions(application.WebviewWindowOptions{
			Name:             "screen-view-" + short,
			Title:            winTitle,
			Width:            960,
			Height:           600,
			MinWidth:         480,
			MinHeight:        300,
			URL:              "/screen-view.html?" + query,
			AlwaysOnTop:      true,
			Frameless:        true,
			Hidden:           true,
			InitialPosition:  application.WindowCentered,
			BackgroundColour: application.RGBA{Red: 0x1b, Green: 0x1e, Blue: 0x25, Alpha: 0xff},
			DevToolsEnabled:  cfg.Env == envLocal,
		})
		win.OnWindowEvent(events.Common.WindowClosing, func(_ *application.WindowEvent) { onClosed() })
		return &viewerWindowWrapper{win: win, slot: slot}
	})

	// Сохранение позиции/размера с дебаунсом.
	// Подписываемся И на Common, И на Windows-специфичные события — на случай
	// если translation layer не отрабатывает в каких-то путях.
	saver := newWindowStateSaver(logger, identitySvc, mainWin)
	moveCb := func(_ *application.WindowEvent) { saver.schedule() }
	resizeCb := func(_ *application.WindowEvent) { saver.schedule() }
	mainWin.OnWindowEvent(events.Common.WindowDidMove, moveCb)
	mainWin.OnWindowEvent(events.Common.WindowDidResize, resizeCb)
	mainWin.OnWindowEvent(events.Windows.WindowDidMove, moveCb)
	mainWin.OnWindowEvent(events.Windows.WindowDidResize, resizeCb)
	mainWin.OnWindowEvent(events.Linux.WindowDidMove, moveCb)
	mainWin.OnWindowEvent(events.Linux.WindowDidResize, resizeCb)
	mainWin.OnWindowEvent(events.Mac.WindowDidMove, moveCb)
	mainWin.OnWindowEvent(events.Mac.WindowDidResize, resizeCb)
	mainWin.OnWindowEvent(events.Common.WindowClosing, func(_ *application.WindowEvent) {
		saver.flushNow()
	})

	// Если приложение запущено после автообновления — поднять окно поверх,
	// как только оно реально отрисуется. Раньше тут была sleep+goroutine ДО
	// app.Run(), которая трогала window.impl до его инициализации и крашила
	// весь процесс молча.
	if relaunchedAfterUpdate {
		var once sync.Once
		mainWin.OnWindowEvent(events.Common.WindowShow, func(_ *application.WindowEvent) {
			once.Do(func() {
				logger.Info("update: bringing window to front after relaunch")
				mainWin.SetAlwaysOnTop(true)
				mainWin.Focus()
				time.AfterFunc(300*time.Millisecond, func() {
					mainWin.SetAlwaysOnTop(false)
				})
			})
		})
	}

	// Graceful shutdown по SIGINT/SIGTERM. Без этого Go по умолчанию делает
	// os.Exit при SIGTERM — defers выше (Shutdown сервисов, NetworkService
	// пушит логи) не отработают. Типичный сценарий: "stop" из IDE/дебаггера,
	// Ctrl+C в консоли, kill от Wails dev supervisor'а. Переводим в нормальный
	// путь выхода: app.Quit() → app.Run() возвращается → defers фигачат.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Info("получен сигнал остановки", slog.String("signal", sig.String()))
		app.Quit()
	}()

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}

// --- Window state persistence ---

const waitPidFlag = "--wait-pid"

// stripUpdateFlag удаляет --from-update и --wait-pid <N> из os.Args если они есть.
// Возвращает (relaunched, parentPid). Должно вызываться ДО любого обращения к
// flag.Parse(), иначе flag package сделает silent os.Exit(2) на неизвестном флаге
// в windowsgui-бинаре.
//
// `--wait-pid <N>` передаётся из update_apply_*.go при автообновлении —
// PID старого процесса, которого новый должен дождаться (см. waitForParentExit).
func stripUpdateFlag() (bool, int) {
	found := false
	parentPid := 0
	filtered := os.Args[:0:0]
	filtered = append(filtered, os.Args[0])
	skipNext := false
	for i, a := range os.Args[1:] {
		if skipNext {
			skipNext = false
			continue
		}
		if a == updateFlag {
			found = true
			continue
		}
		if a == waitPidFlag {
			// Берём следующий аргумент как pid
			if i+2 < len(os.Args) {
				if pid, err := strconv.Atoi(os.Args[i+2]); err == nil {
					parentPid = pid
				}
			}
			skipNext = true
			continue
		}
		filtered = append(filtered, a)
	}
	os.Args = filtered
	return found, parentPid
}

// waitForParentExit реализован в main_wait_windows.go / main_wait_other.go —
// Windows использует OpenProcess + WaitForSingleObject, остальные OS — Signal(0).

const (
	bindIfaceFlag  = "--bind-iface"
	forceRelayFlag = "--force-relay"
)

// stripDevFlags удаляет --bind-iface <name|ip> (или --bind-iface=<…>) и
// --force-relay из os.Args. Возвращает (bindSpec, forceRelay). Парсится
// вручную по той же причине что и stripUpdateFlag: flag.Parse() в
// windowsgui-бинаре делает silent os.Exit(2) на неизвестном флаге.
//
// bindSpec интерпретируется в network.ResolveDevBindSpec — может быть IPv4
// либо имя сетевого интерфейса (точное или substring).
// forceRelay — бесзначный bool-флаг, форсит ICETransportPolicyRelay.
func stripDevFlags() (string, bool) {
	bindSpec := ""
	relay := false
	filtered := os.Args[:0:0]
	filtered = append(filtered, os.Args[0])
	skipNext := false
	for i, a := range os.Args[1:] {
		if skipNext {
			skipNext = false
			continue
		}
		if a == bindIfaceFlag {
			if i+2 < len(os.Args) {
				bindSpec = os.Args[i+2]
			}
			skipNext = true
			continue
		}
		if eq := bindIfaceFlag + "="; len(a) > len(eq) && a[:len(eq)] == eq {
			bindSpec = a[len(eq):]
			continue
		}
		if a == forceRelayFlag {
			relay = true
			continue
		}
		filtered = append(filtered, a)
	}
	os.Args = filtered
	return bindSpec, relay
}

func loadWindowState(logger *slog.Logger, ident *services.IdentityService) (windowState, bool) {
	raw, err := ident.GetSetting(windowStateKey)
	if err != nil || raw == "" {
		return windowState{}, false
	}
	var st windowState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		logger.Warn("window state: parse failed", slog.String("error", err.Error()))
		return windowState{}, false
	}
	return st, true
}

// applyWindowState заполняет options сохранённой позицией, проверяя что окно
// будет видно хотя бы на одном текущем экране. Если ни один экран не подходит
// (отключили монитор, изменили разрешение) — оставляет defaults и WindowCentered.
func applyWindowState(logger *slog.Logger, opts *application.WebviewWindowOptions, st windowState, app *application.App) {
	if st.Width >= 400 && st.Width <= 10000 {
		opts.Width = st.Width
	}
	if st.Height >= 300 && st.Height <= 10000 {
		opts.Height = st.Height
	}
	if st.Maximised {
		opts.StartState = application.WindowStateMaximised
		opts.InitialPosition = application.WindowCentered
		logger.Info("window state: applying maximised")
		return
	}
	if !pointVisibleOnAnyScreen(app, st.X, st.Y, opts.Width, opts.Height) {
		logger.Info("window state: saved position not visible, centering",
			slog.Int("x", st.X), slog.Int("y", st.Y))
		opts.InitialPosition = application.WindowCentered
		return
	}
	opts.InitialPosition = application.WindowXY
	opts.X = st.X
	opts.Y = st.Y
	logger.Info("window state: applying saved position",
		slog.Int("x", st.X), slog.Int("y", st.Y),
		slog.Int("w", opts.Width), slog.Int("h", opts.Height))
}

// fitDefaultWindow — размер по умолчанию не больше рабочей области основного
// экрана (ноутбук 1366 не вмещает 1400), с полями 40 px и не меньше MinWidth/MinHeight.
func fitDefaultWindow(opts *application.WebviewWindowOptions, app *application.App) {
	if app == nil || app.Screen == nil {
		return
	}
	sc := app.Screen.GetPrimary()
	if sc == nil || sc.WorkArea.Width <= 0 || sc.WorkArea.Height <= 0 {
		return
	}
	if w := sc.WorkArea.Width - 40; opts.Width > w {
		opts.Width = max(w, opts.MinWidth)
	}
	if h := sc.WorkArea.Height - 40; opts.Height > h {
		opts.Height = max(h, opts.MinHeight)
	}
}

// pointVisibleOnAnyScreen возвращает true если хотя бы заголовок окна
// (первые minVisibleW × minVisibleH px) попадает в WorkArea какого-либо экрана.
func pointVisibleOnAnyScreen(app *application.App, x, y, w, h int) bool {
	if app == nil || app.Screen == nil {
		return true // нечем проверить — доверяем сохранённому
	}
	screens := app.Screen.GetAll()
	if len(screens) == 0 {
		return true
	}
	titleW := minVisibleW
	if w < titleW {
		titleW = w
	}
	titleH := minVisibleH
	if h < titleH {
		titleH = h
	}
	titleRight := x + titleW
	titleBottom := y + titleH
	for _, sc := range screens {
		wa := sc.WorkArea
		if x < wa.X+wa.Width && titleRight > wa.X &&
			y < wa.Y+wa.Height && titleBottom > wa.Y {
			return true
		}
	}
	return false
}

type windowStateSaver struct {
	mu        sync.Mutex
	timer     *time.Timer
	logger    *slog.Logger
	ident     *services.IdentityService
	win       *application.WebviewWindow
	startedAt time.Time
}

const startupGrace = 2 * time.Second

func newWindowStateSaver(logger *slog.Logger, ident *services.IdentityService, win *application.WebviewWindow) *windowStateSaver {
	return &windowStateSaver{logger: logger, ident: ident, win: win, startedAt: time.Now()}
}

func (s *windowStateSaver) schedule() {
	// Игнорируем события первые ~2 сек после старта — Wails выстреливает
	// WindowDidMove/Resize на начальную раскладку, и без grace мы перезаписываем
	// валидное сохранённое состояние мусором "позиция при создании окна".
	if time.Since(s.startedAt) < startupGrace {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer != nil {
		s.timer.Stop()
	}
	s.timer = time.AfterFunc(saveDebounce, s.persist)
}

func (s *windowStateSaver) flushNow() {
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.mu.Unlock()
	s.persist()
}

func (s *windowStateSaver) persist() {
	if s.win == nil {
		return
	}
	maximised := s.win.IsMaximised()
	x, y := s.win.Position()
	w, h := s.win.Size()
	// Минимизированное окно возвращает (-32000, -32000) на Windows и/или нулевые
	// размеры — не сохраняем такое состояние, иначе при следующем запуске потеряем позицию.
	if !maximised && (w < 200 || h < 100 || x <= -10000 || y <= -10000) {
		s.logger.Debug("window state: skipping save (minimised/bad geometry)",
			slog.Int("x", x), slog.Int("y", y), slog.Int("w", w), slog.Int("h", h))
		return
	}
	st := windowState{X: x, Y: y, Width: w, Height: h, Maximised: maximised}
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	if err := s.ident.SaveSetting(windowStateKey, string(data)); err != nil {
		s.logger.Debug("window state: save failed", slog.String("error", err.Error()))
		return
	}
	s.logger.Debug("window state: saved",
		slog.Int("x", x), slog.Int("y", y), slog.Int("w", w), slog.Int("h", h),
		slog.Bool("max", maximised))
}

// --- Logger setup ---

func setupLogger(env string) (*slog.Logger, *os.File) {
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

// logRotateKeep — сколько предыдущих сессий хранить на диске (плюс текущий лог).
const logRotateKeep = 4

// logLevel — уровень всех обработчиков, меняется на ходу («Подробное
// логирование», AppService.SetVerboseLogging). logBaseLevel — уровень окружения.
var (
	logLevel     = new(slog.LevelVar)
	logBaseLevel slog.Level
)

// rotateLogFiles перемещает текущий vox-app.log в .log.1, сдвигает старые
// .log.N → .log.N+1, удаляет всё что старше logRotateKeep. Затем создаёт пустой
// vox-app.log для новой сессии.
//
// Также создаёт маркер vox-app.log.1.unsent — NetworkService.pushLogsToServer
// увидит его и однократно отправит лог предыдущей сессии на сервер, чтобы
// можно было дебажить крэши через fetch-logs.sh.
func rotateLogFiles(name string) {
	// Удаляем самый старый файл и его маркер (вываливается из окна хранения)
	oldest := fmt.Sprintf("%s.%d", name, logRotateKeep)
	_ = os.Remove(oldest)
	_ = os.Remove(oldest + ".unsent")
	// Сдвигаем .N → .N+1 с конца, вместе с парным .unsent маркером
	// (чтобы неотправленные сессии сохраняли статус после ротации).
	for i := logRotateKeep - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", name, i)
		dst := fmt.Sprintf("%s.%d", name, i+1)
		if _, err := os.Stat(src); err == nil {
			_ = os.Rename(src, dst)
		}
		if _, err := os.Stat(src + ".unsent"); err == nil {
			_ = os.Rename(src+".unsent", dst+".unsent")
		}
	}
	// Текущий → .1, ставим маркер на отправку
	if _, err := os.Stat(name); err == nil {
		if err := os.Rename(name, name+".1"); err == nil {
			_ = os.WriteFile(name+".1.unsent", nil, 0644)
		}
	}
}

func openLogFile() (*os.File, error) {
	rotateLogFiles("vox-app.log")
	return os.OpenFile("vox-app.log", os.O_CREATE|os.O_WRONLY, 0644)
}

func setupPrettySlog() (*slog.Logger, *os.File) {
	logBaseLevel = slog.LevelDebug
	logLevel.Set(logBaseLevel)
	opts := slogpretty.PrettyHandlerOptions{
		SlogOpts: &slog.HandlerOptions{
			Level: logLevel,
		},
	}
	prettyHandler := opts.NewPrettyHandler(os.Stdout)

	f, err := openLogFile()
	if err != nil {
		return slog.New(logsafe.Wrap(prettyHandler)), nil
	}

	jsonHandler := slog.NewJSONHandler(f, &slog.HandlerOptions{Level: logLevel})
	return slog.New(logsafe.Wrap(newMultiHandler(prettyHandler, jsonHandler))), f
}

func setupJSONSlog(level slog.Level) (*slog.Logger, *os.File) {
	logBaseLevel = level
	logLevel.Set(level)
	f, err := openLogFile()
	if err != nil {
		return slog.New(logsafe.Wrap(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))), nil
	}

	jsonStdout := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})
	jsonFile := slog.NewJSONHandler(f, &slog.HandlerOptions{Level: logLevel})
	return slog.New(logsafe.Wrap(newMultiHandler(jsonStdout, jsonFile))), f
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

// callPopupWrapper оборачивает Wails WebviewWindow в интерфейс CallPopup
type callPopupWrapper struct {
	win  *application.WebviewWindow
	main *application.WebviewWindow
}

// Show — показать без захвата фокуса. WS_EX_NOACTIVATE защищает только от
// чужих окон: если фокус у нашего процесса, WebView2 при Show перетащит его в
// попап — возвращаем главному (как notifyWindowWrapper.ShowAt).
func (w *callPopupWrapper) Show() {
	refocus := w.main != nil && w.main.IsFocused()
	w.win.Show()
	if refocus {
		w.main.Focus()
	}
}

func (w *callPopupWrapper) Close() {
	w.win.Close()
}

// viewerWindowWrapper — окно демонстрации для ScreenShareService.
type viewerWindowWrapper struct {
	win  *application.WebviewWindow
	slot int // сколько окон демонстрации уже открыто — сдвиг каскадом
}

// Show — по центру; окна по центру встали бы точно друг на друга, поэтому
// каждое следующее сдвигаем на 32 px вправо-вниз.
func (w *viewerWindowWrapper) Show() {
	w.win.Show()
	if w.slot > 0 {
		x, y := w.win.Position()
		w.win.SetPosition(x+32*w.slot, y+32*w.slot)
	}
}

func (w *viewerWindowWrapper) Close()                { w.win.Close() }
func (w *viewerWindowWrapper) Focus()                { w.win.Focus() }
func (w *viewerWindowWrapper) SetAlwaysOnTop(b bool) { w.win.SetAlwaysOnTop(b) }
func (w *viewerWindowWrapper) Minimise()             { w.win.Minimise() }
func (w *viewerWindowWrapper) ToggleMaximise()       { w.win.ToggleMaximise() }
func (w *viewerWindowWrapper) ToggleFullscreen() {
	if w.win.IsFullscreen() {
		w.win.UnFullscreen()
	} else {
		w.win.Fullscreen()
	}
}

// notifyWindowWrapper — окно уведомлений для NotifyService.
type notifyWindowWrapper struct {
	app  *application.App
	win  *application.WebviewWindow
	main *application.WebviewWindow
}

// ShowAt — правый нижний угол рабочей области основного монитора (над
// таскбаром), отступ 16 px. Show на окне с WS_EX_NOACTIVATE не активирует его.
func (w *notifyWindowWrapper) ShowAt(width, height int) {
	const margin = 16
	x, y := margin, margin
	if scr := w.app.Screen.GetPrimary(); scr != nil {
		wa := scr.WorkArea
		x = wa.X + wa.Width - width - margin
		y = wa.Y + wa.Height - height - margin
	}
	// WS_EX_NOACTIVATE защищает только от чужих окон: если фокус уже у
	// нашего процесса (пользователь печатает в Vox), WebView2 при Show
	// перетаскивает его в окно уведомлений. Возвращаем главному окну.
	refocus := w.main != nil && w.main.IsFocused()
	w.win.SetSize(width, height)
	w.win.SetPosition(x, y)
	w.win.Show()
	if refocus {
		w.main.Focus()
	}
}

func (w *notifyWindowWrapper) Hide() {
	w.win.Hide()
}

// remoteDebugArgs — аргументы WebView2 для remote debugging (Chrome DevTools
// Protocol). Включается только в ENV=local переменной VOX_REMOTE_DEBUG_PORT,
// чтобы автотесты могли снимать скриншоты и выполнять JS в странице.
func remoteDebugArgs(env string) []string {
	if env != envLocal {
		return nil
	}
	port := os.Getenv("VOX_REMOTE_DEBUG_PORT")
	if port == "" {
		return nil
	}
	return []string{"--remote-debugging-port=" + port}
}

// moveDBFiles переносит базу SQLite вместе с -wal/-shm: без WAL в копии
// пропадут последние записи, а чужой WAL рядом с новой базой ей помешает.
func moveDBFiles(logger *slog.Logger, from, to string) {
	removeDBFiles(to)
	for _, sfx := range []string{"", "-wal", "-shm"} {
		if err := os.Rename(from+sfx, to+sfx); err != nil && !os.IsNotExist(err) {
			logger.Error("перенос файла базы не удался",
				slog.String("from", from+sfx), slog.String("to", to+sfx), slog.String("error", err.Error()))
		}
	}
}

func removeDBFiles(path string) {
	for _, sfx := range []string{"", "-wal", "-shm"} {
		os.Remove(path + sfx)
	}
}
