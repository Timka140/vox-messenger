package server

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"messenger_signal/internal/admin"
	"messenger_signal/internal/auth"
	"messenger_signal/internal/cluster"
	"messenger_signal/internal/config"
	"messenger_signal/internal/groupstore"
	"messenger_signal/internal/hub"
	"messenger_signal/internal/identity"
	"messenger_signal/internal/logstore"
	"messenger_signal/internal/releases"
	"messenger_signal/pkg/protocol"
)

var upgrader = websocket.Upgrader{
	// Origin не проверяется — клиент не браузер, авторизация через Ed25519 challenge-response
	CheckOrigin: func(r *http.Request) bool { return true },
}

const (
	authTimeout      = 10 * time.Second // время на аутентификацию после подключения
	maxMessageSize   = 64 * 1024        // 64 KB
	searchMaxResults = 20
	// readWait — max тишина от клиента (ни данных, ни pong) до разрыва.
	// pingInterval 30s + запас на два пропущенных pong. Без ReadDeadline
	// half-open соединение висит в ReadMessage вечно, и вся детекция зомби
	// держится на одном ping-loop.
	readWait = 90 * time.Second
	// authWriteWait — дедлайн записи в pre-auth фазе (challenge/auth_ok/ice),
	// когда write pump пира ещё не запущен.
	authWriteWait = 10 * time.Second
)

// Server — HTTP/WebSocket сервер сигнализации
type Server struct {
	hub       *hub.Hub
	cfg       config.WSConfig
	turn      config.TURNConfig
	sfu       config.SFUConfig
	groupsCfg config.GroupConfig
	log       *slog.Logger
	identity  *identity.Identity // ключ сервера для mutual auth
	server    *http.Server
	tlsServer *http.Server     // wss/https listener (nil — TLS выключен)
	tlsCert   *tls.Certificate // самоподписанный сертификат узла (internal/tlscert)
	tlsHash   []byte           // SHA-256 tlsCert — входит в подписи входа v2
	logStore  *logstore.Store
	debugSrv  *http.Server
	logFile   string            // путь к vox-signal.log (для копирования своих логов)
	releases  *releases.Store   // хранилище релизов клиентского приложения (может быть nil)
	groups    *groupstore.Store // server relay для больших групп (nil если отключён)
	rate      *groupstore.RateLimiter
	cluster   *cluster.Manager // узел кластера (nil — одиночный сервер)

	// admin API /admin/v1/*: токен короче admin.MinTokenLen — выключен (404).
	adminToken    string
	releasePubKey ed25519.PublicKey

	// sendLocks — порядок отправки в группу: запись в журнал, рассылка своим
	// и реплика соседям идут под одним замком группы, чтобы на линк и
	// клиентам сообщения уходили в порядке seq. Шардами, а не общим замком:
	// разные группы друг друга не ждут.
	sendLocks [64]sync.Mutex

	// hintTimers — отложенные подсказки group_sync_hint по группам (догон).
	hintMu     sync.Mutex
	hintTimers map[string]*time.Timer
}

// groupSendLock — замок отправки для группы.
func (s *Server) groupSendLock(groupID string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(groupID))
	return &s.sendLocks[h.Sum32()%uint32(len(s.sendLocks))]
}

// errNodeLink — соединение оказалось линком другого узла кластера и уже
// отработано cluster.Accept; handleWS просто выходит.
var errNodeLink = errors.New("cluster node link")

// errRedirected — клиенту отправлен redirect на другой узел.
var errRedirected = errors.New("client redirected")

// SetCluster включает кластер: приём линков node_auth и /healthz с линками.
func (s *Server) SetCluster(m *cluster.Manager) {
	s.cluster = m
}

// New создаёт сервер. id — Ed25519 identity сервера, используется для mutual
// auth (предъявляется клиенту в challenge, подписывает client_challenge в auth_ok).
func New(h *hub.Hub, cfg config.WSConfig, turn config.TURNConfig, sfu config.SFUConfig, log *slog.Logger, id *identity.Identity) *Server {
	return &Server{
		hub:      h,
		cfg:      cfg,
		turn:     turn,
		sfu:      sfu,
		log:      log,
		identity: id,
	}
}

// SetTLS включает второй listener с сертификатом cert (SHA-256 — hash).
func (s *Server) SetTLS(cert tls.Certificate, hash []byte) {
	s.tlsCert = &cert
	s.tlsHash = hash
}

// SetGroupStore включает серверный relay для больших групп.
// cfg должен иметь непустой DBPath (иначе не вызывать этот метод).
func (s *Server) SetGroupStore(store *groupstore.Store, cfg config.GroupConfig) {
	s.groups = store
	s.groupsCfg = cfg
	s.rate = groupstore.NewRateLimiter(cfg.RateBurst, cfg.RateRefill, 10*time.Minute)
}

// SetReleasesStore устанавливает хранилище релизов клиента для endpoints /api/update/*
// и расширения payload auth_ok информацией о последнем релизе.
// Можно не вызывать — тогда endpoints вернут 503, а auth_ok не будет содержать LatestRelease.
func (s *Server) SetReleasesStore(store *releases.Store) {
	s.releases = store
}

// SetAdmin включает admin API /admin/v1/* (ADMIN-CONTRACT.md §1). Вызывать до
// Handler/Start. releasePub nil — замена манифеста через API недоступна (503).
func (s *Server) SetAdmin(token string, releasePub ed25519.PublicKey) {
	s.adminToken = token
	s.releasePubKey = releasePub
}

// adminHandler — подписанный admin API либо 404 на всё, если он выключен.
func (s *Server) adminHandler() http.Handler {
	if len(s.adminToken) < admin.MinTokenLen {
		return admin.Disabled()
	}
	opt := admin.Options{
		Token:          s.adminToken,
		ReleasePubKey:  s.releasePubKey,
		NodeID:         s.identity.PubKeyHex(),
		Clients:        s.hub.Count,
		ClientVersions: s.hub.VersionCounts,
		Log:            s.log,
		Releases:       s.releases,
		Logs:           s.logStore,
		Cluster: func() admin.ClusterInfo {
			if s.cluster == nil {
				return admin.ClusterInfo{Enabled: false}
			}
			h := s.cluster.Health()
			ci := admin.ClusterInfo{Enabled: true, Self: s.cluster.SelfAddr(), Peers: []admin.ClusterPeer{}}
			for _, l := range h.Links {
				ci.Peers = append(ci.Peers, admin.ClusterPeer{Addr: l.Addr, ID: l.NodeID, Connected: l.Alive})
			}
			return ci
		},
	}
	if s.logStore != nil {
		opt.CollectLogs = s.collectLogs
		opt.OnlineSources = s.onlineLogSources
	}
	if s.groups != nil {
		opt.Groups = func() (int, bool) {
			n, err := s.groups.CountGroups()
			if err != nil {
				return 0, false
			}
			return n, true
		}
	}
	return admin.New(opt).Handler()
}

// SetLogStore устанавливает хранилище debug-логов
func (s *Server) SetLogStore(store *logstore.Store, logFile string) {
	s.logStore = store
	s.logFile = logFile
}

// Handler — HTTP-маршруты сервера: WS, health, healthz, автообновление.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(s.cfg.Path, s.handleWS)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/healthz", s.handleHealthz)
	// Endpoints автообновления — на том же порту что и WebSocket
	// (если WS проходит firewall — эти тоже пройдут)
	mux.HandleFunc("/api/update/manifest", s.handleUpdateManifest)
	mux.HandleFunc("/api/update/download/", s.handleUpdateDownload)
	mux.Handle("/admin/v1/", s.adminHandler())
	return mux
}

// Start запускает HTTP сервер
func (s *Server) Start() error {
	mux := s.Handler()

	// Читаем timeouts с поправкой: download может быть длительным, поэтому
	// для HTTP сервера ставим большой WriteTimeout
	writeTimeout := s.cfg.WriteTimeout
	if writeTimeout < 5*time.Minute {
		writeTimeout = 5 * time.Minute
	}

	s.server = &http.Server{
		Addr:         s.cfg.Addr(),
		Handler:      mux,
		ReadTimeout:  s.cfg.ReadTimeout,
		WriteTimeout: writeTimeout,
		IdleTimeout:  s.cfg.IdleTimeout,
	}

	if s.tlsCert != nil && s.cfg.TLSPort > 0 {
		s.tlsServer = &http.Server{
			Addr:         fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.TLSPort),
			Handler:      mux,
			ReadTimeout:  s.cfg.ReadTimeout,
			WriteTimeout: writeTimeout,
			IdleTimeout:  s.cfg.IdleTimeout,
			TLSConfig:    &tls.Config{Certificates: []tls.Certificate{*s.tlsCert}, MinVersion: tls.VersionTLS12},
		}
		go func() {
			s.log.Info("TLS listener запускается", slog.String("addr", s.tlsServer.Addr),
				slog.String("cert_sha256", hex.EncodeToString(s.tlsHash)))
			if err := s.tlsServer.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.log.Error("TLS listener остановлен", slog.String("error", err.Error()))
			}
		}()
	}

	s.log.Info("сервер запускается", slog.String("addr", s.cfg.Addr()), slog.String("ws_path", s.cfg.Path))
	return s.server.ListenAndServe()
}

// handleUpdateManifest отдаёт raw manifest.json + подпись в заголовке X-Signature.
// Клиент верифицирует Ed25519 подпись над raw body до парсинга (предотвращает
// parse→verify race и корректно работает на любом форматтере JSON).
func (s *Server) handleUpdateManifest(w http.ResponseWriter, r *http.Request) {
	if s.releases == nil {
		http.Error(w, "updates not configured", http.StatusServiceUnavailable)
		return
	}
	body, sig, err := s.releases.GetBytes()
	if err != nil {
		s.log.Debug("manifest get error", slog.String("error", err.Error()))
		http.Error(w, "manifest not available", http.StatusNotFound)
		return
	}
	// Подпись передаём base64-строкой в заголовке (файл manifest.json.sig хранит
	// уже base64 текст, достаточно trim и отдать).
	sigStr := strings.TrimSpace(string(sig))
	w.Header().Set("Content-Type", "application/json")
	if sigStr != "" {
		w.Header().Set("X-Signature", sigStr)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handleUpdateDownload отдаёт файл артефакта (бинарь, модель, runtime lib).
// URL: /api/update/download/<platform>/<version>/<path...>
// Где <path...> — относительный путь (может содержать подкаталоги),
// каждый сегмент валидируется в releases.ArtifactPath.
func (s *Server) handleUpdateDownload(w http.ResponseWriter, r *http.Request) {
	if s.releases == nil {
		http.Error(w, "updates not configured", http.StatusServiceUnavailable)
		return
	}
	prefix := "/api/update/download/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	parts := strings.SplitN(rest, "/", 3) // platform, version, остаток-как-filename
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		http.Error(w, "expected /api/update/download/<platform>/<version>/<path...>", http.StatusBadRequest)
		return
	}
	platform, ver, filename := parts[0], parts[1], parts[2]

	abs, err := s.releases.ArtifactPath(platform, ver, filename)
	if err != nil {
		s.log.Debug("artifact path rejected",
			slog.String("platform", platform),
			slog.String("version", ver),
			slog.String("filename", filename),
			slog.String("error", err.Error()))
		http.Error(w, "artifact not found", http.StatusNotFound)
		return
	}

	// Снимаем write deadline на этот запрос. http.Server.WriteTimeout — это
	// абсолютный дедлайн с момента начала записи, а большие бинари (80+ МБ)
	// на медленных каналах (~250 КБ/с) легко превышают любой разумный
	// глобальный timeout. ResponseController.SetWriteDeadline(time.Time{})
	// отключает deadline только для этого handler'а, остальные запросы
	// остаются под базовым WriteTimeout.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		s.log.Debug("response controller unsupported for write deadline",
			slog.String("error", err.Error()))
	}

	// Логируем скачивание (полезно для статистики)
	if st, err := os.Stat(abs); err == nil {
		s.log.Info("📦 update download",
			slog.String("platform", platform),
			slog.String("version", ver),
			slog.String("filename", filename),
			slog.Int64("size", st.Size()))
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filepath.Base(abs)+"\"")
	http.ServeFile(w, r, abs)
}

// Stop gracefully останавливает сервер
func (s *Server) Stop(ctx context.Context) error {
	if s.tlsServer != nil {
		_ = s.tlsServer.Shutdown(ctx)
	}
	return s.server.Shutdown(ctx)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status": "ok",
		"peers":  s.hub.Count(),
	})
}

// handleHealthz — состояние узла для балансировки и проверки линков
// (docs/cluster-contract.md §10). Без кластера links пустой.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	resp := struct {
		NodeID    string               `json:"node_id"`
		Clients   int                  `json:"clients"`
		SoftLimit int                  `json:"soft_limit"`
		DirSize   int                  `json:"dir_size"`
		Links     []cluster.LinkHealth `json:"links"`
	}{
		NodeID:  s.identity.PubKeyHex(),
		Clients: s.hub.Count(),
		Links:   []cluster.LinkHealth{},
	}
	if s.cluster != nil {
		h := s.cluster.Health()
		resp.SoftLimit = h.SoftLimit
		resp.DirSize = h.DirSize
		if h.Links != nil {
			resp.Links = h.Links
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Error("websocket upgrade failed", slog.String("error", err.Error()))
		return
	}

	conn.SetReadLimit(maxMessageSize)

	// Аутентификация: challenge-response. На TLS-соединении отпечаток нашего
	// сертификата входит в подписи v2 — так клиент убеждается, что TLS
	// заканчивается на этом узле, а не у посредника.
	var tlsHash []byte
	if r.TLS != nil {
		tlsHash = s.tlsHash
	}
	peer, err := s.authenticate(conn, tlsHash)
	if errors.Is(err, errNodeLink) || errors.Is(err, errRedirected) {
		conn.Close()
		return
	}
	if err != nil {
		s.log.Warn("аутентификация провалена", slog.String("error", err.Error()))
		s.sendErrorConn(conn, protocol.ErrAuthFailed, err.Error())
		conn.Close()
		return
	}

	// Регистрируем пира в hub
	s.hub.Register(peer)
	defer func() {
		s.hub.Unregister(peer)
		peer.Close()
	}()

	// Режим доставки его групп — уже через write pump пира.
	s.pushServerGroupModes(peer)

	// Live-детекция: любой pong (и любое сообщение в readLoop) продлевает
	// ReadDeadline. Half-open соединение умирает само, независимо от ping-loop.
	conn.SetReadDeadline(time.Now().Add(readWait))
	conn.SetPongHandler(func(string) error {
		peer.UpdatePong()
		return conn.SetReadDeadline(time.Now().Add(readWait))
	})

	// Читаем сообщения
	s.readLoop(peer)
}

func (s *Server) authenticate(conn *websocket.Conn, tlsHash []byte) (*hub.Peer, error) {
	// Генерируем challenge
	challenge, err := auth.GenerateChallenge()
	if err != nil {
		return nil, err
	}

	// Отправляем challenge клиенту вместе с публичным ключом сервера
	challengePayload, _ := json.Marshal(protocol.ChallengePayload{
		Challenge:    challenge,
		ServerPubkey: hex.EncodeToString(s.identity.Public),
	})
	challengeMsg, _ := json.Marshal(protocol.Message{
		Type:    protocol.TypeChallenge,
		Payload: challengePayload,
	})

	_ = conn.SetWriteDeadline(time.Now().Add(authWriteWait))
	if err := conn.WriteMessage(websocket.TextMessage, challengeMsg); err != nil {
		return nil, err
	}

	// Ждём ответ с таймаутом
	conn.SetReadDeadline(time.Now().Add(authTimeout))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	// Сбрасываем deadline после аутентификации
	conn.SetReadDeadline(time.Time{})

	var msg protocol.Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, err
	}
	// Другой узел кластера отвечает на тот же challenge сообщением node_auth.
	if msg.Type == protocol.TypeNodeAuth {
		if s.cluster == nil {
			return nil, fmt.Errorf("node_auth: кластер на этом узле выключен")
		}
		var np protocol.NodeAuthPayload
		if err := json.Unmarshal(msg.Payload, &np); err != nil {
			return nil, fmt.Errorf("node_auth: %w", err)
		}
		if err := s.cluster.Accept(conn, challenge, np, tlsHash); err != nil {
			return nil, fmt.Errorf("node_auth: %w", err)
		}
		return nil, errNodeLink
	}
	if msg.Type != protocol.TypeAuth {
		return nil, fmt.Errorf("expected auth message, got %s", msg.Type)
	}

	var authPayload protocol.AuthPayload
	if err := json.Unmarshal(msg.Payload, &authPayload); err != nil {
		return nil, err
	}

	// Mutual auth. Поле client_challenge обязательное; пустой/невалидный — отвал.
	clientChallengeBytes, err := hex.DecodeString(authPayload.ClientChallenge)
	if err != nil || len(clientChallengeBytes) == 0 {
		return nil, fmt.Errorf("invalid client_challenge")
	}
	if authPayload.AuthVersion < auth.AuthVersion {
		return nil, fmt.Errorf("старый вход без auth_version не поддерживается")
	}
	clientPub, err := hex.DecodeString(authPayload.PublicKey)
	if err != nil || len(clientPub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid public key")
	}
	// Подписи с меткой и полями (internal/auth/v2.go): ни одна сторона не
	// подписывает сырые чужие байты.
	challengeBytes, _ := hex.DecodeString(challenge)
	sig, err := hex.DecodeString(authPayload.Signature)
	signed := auth.Message(auth.LabelClient, s.identity.Public, challengeBytes, tlsHash, clientPub)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(clientPub), signed, sig) {
		return nil, fmt.Errorf("signature verification failed")
	}
	serverSig := ed25519.Sign(s.identity.Private,
		auth.Message(auth.LabelServer, clientPub, clientChallengeBytes, tlsHash, s.identity.Public))

	// Узел перегружен, а сосед свободнее — отправляем клиента туда (§6.3).
	// Только клиентам, которые объявили redirect_ok.
	if authPayload.RedirectOK && s.cluster != nil {
		if target, ok := s.cluster.RedirectTarget(s.hub.Count()); ok {
			addr := cluster.ClientAddr(target.Addr)
			rsig := ed25519.Sign(s.identity.Private,
				auth.Message(auth.LabelRedirect, clientChallengeBytes, []byte(addr), []byte(strings.ToLower(target.NodeID)), tlsHash))
			rp, _ := json.Marshal(protocol.RedirectPayload{
				Addr:            addr,
				Pubkey:          target.NodeID,
				ServerSignature: hex.EncodeToString(rsig),
			})
			data, _ := json.Marshal(protocol.Message{Type: protocol.TypeRedirect, Payload: rp})
			_ = conn.SetWriteDeadline(time.Now().Add(authWriteWait))
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return nil, err
			}
			s.log.Info("↪ клиент перенаправлен на другой узел",
				slog.String("nick_tag", authPayload.NickTag),
				slog.String("to", cluster.ClientAddr(target.Addr)),
				slog.Int("clients", s.hub.Count()))
			return nil, errRedirected
		}
	}

	okPayload := protocol.AuthOKPayload{
		ServerSignature: hex.EncodeToString(serverSig),
		Presence:        true,
	}
	if s.releases != nil {
		okPayload.LatestRelease = s.releases.LatestReleaseInfo()
	}
	if s.sfu.Endpoint != "" {
		okPayload.SFUEndpoint = s.sfu.Endpoint
		if s.sfu.TLSEndpoint != "" {
			okPayload.SFUEndpoint = s.sfu.TLSEndpoint
		}
	}
	okPayloadBytes, _ := json.Marshal(okPayload)
	okMsg, _ := json.Marshal(protocol.Message{Type: protocol.TypeAuthOK, Payload: okPayloadBytes})
	_ = conn.SetWriteDeadline(time.Now().Add(authWriteWait))
	if err := conn.WriteMessage(websocket.TextMessage, okMsg); err != nil {
		return nil, err
	}

	// Отправляем ICE-серверы (STUN + TURN если настроен)
	servers := []protocol.ICEServerConfig{
		{URLs: []string{"stun:stun.l.google.com:19302"}},
		{URLs: []string{"stun:stun1.l.google.com:19302"}},
	}
	if s.turn.HasTURN() {
		servers = append(servers, protocol.ICEServerConfig{
			URLs:       []string{s.turn.URL},
			Username:   s.turn.Username,
			Credential: s.turn.Credential,
		})
	}
	icePayload, _ := json.Marshal(protocol.ICEServersPayload{Servers: servers})
	iceMsg, _ := json.Marshal(protocol.Message{Type: protocol.TypeICEServers, Payload: icePayload})
	_ = conn.SetWriteDeadline(time.Now().Add(authWriteWait))
	if err := conn.WriteMessage(websocket.TextMessage, iceMsg); err != nil {
		return nil, err
	}
	// Все узлы кластера — клиент сольёт их в свой список серверов (§6.1).
	if s.cluster != nil {
		slp, _ := json.Marshal(protocol.ServerListPayload{Nodes: s.cluster.ServerList()})
		slMsg, _ := json.Marshal(protocol.Message{Type: protocol.TypeServerList, Payload: slp})
		_ = conn.SetWriteDeadline(time.Now().Add(authWriteWait))
		if err := conn.WriteMessage(websocket.TextMessage, slMsg); err != nil {
			return nil, err
		}
	}
	// Дальше все записи идут через write pump пира — снимаем разовый дедлайн,
	// pump ставит свой перед каждой записью.
	_ = conn.SetWriteDeadline(time.Time{})

	peer := hub.NewPeer(authPayload.PublicKey, authPayload.NickTag, conn)
	if protocol.ValidAppVersion(authPayload.AppVersion) {
		peer.AppVersion = authPayload.AppVersion
	}
	// Политика видимости статуса — до hub.Register: рассылка peer_online
	// идёт уже по ней.
	if authPayload.Presence != nil {
		peer.SetPresence(authPayload.Presence.Watch)
	}
	return peer, nil
}

func (s *Server) readLoop(peer *hub.Peer) {
	for {
		_, raw, err := peer.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				s.log.Warn("ошибка чтения", slog.String("peer", peer.NickTag()), slog.String("error", err.Error()))
			}
			return
		}
		// Входящие данные = пир жив, продлеваем дедлайн наравне с pong
		peer.Conn.SetReadDeadline(time.Now().Add(readWait))

		var msg protocol.Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			s.log.Warn("невалидный JSON", slog.String("peer", peer.NickTag()))
			s.sendError(peer, protocol.ErrInvalidMsg, "invalid JSON")
			continue
		}

		s.handleMessage(peer, &msg)
	}
}

func (s *Server) handleMessage(peer *hub.Peer, msg *protocol.Message) {
	nick := peer.NickTag()
	switch msg.Type {
	case protocol.TypeOffer, protocol.TypeAnswer, protocol.TypeICE,
		protocol.TypeFriendRequest, protocol.TypeFriendAccept, protocol.TypeFriendReject, protocol.TypeFriendRemove,
		protocol.TypeCallInvite, protocol.TypeCallAccept, protocol.TypeCallReject, protocol.TypeCallEnd:
		if msg.To == "" {
			s.log.Warn("сообщение без поля 'to'",
				slog.String("type", msg.Type),
				slog.String("from", nick))
			s.sendError(peer, protocol.ErrInvalidMsg, "missing 'to' field")
			return
		}
		toShort := msg.To
		if len(toShort) > 16 {
			toShort = toShort[:16]
		}
		s.log.Info("📨 relay",
			slog.String("type", msg.Type),
			slog.String("from", nick),
			slog.String("to", toShort))
		if err := s.hub.Route(peer.PublicKey, msg); err != nil {
			s.log.Warn("❌ relay failed",
				slog.String("type", msg.Type),
				slog.String("from", nick),
				slog.String("to", toShort),
				slog.String("error", err.Error()))
		}

	case protocol.TypePeerSearch:
		var payload protocol.PeerSearchPayload
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			s.sendError(peer, protocol.ErrInvalidMsg, "invalid search payload")
			return
		}
		results := s.hub.SearchPeers(payload.Query, searchMaxResults)
		s.log.Info("🔍 search",
			slog.String("from", nick),
			slog.String("query", payload.Query),
			slog.Int("results", len(results)))
		resultPayload, _ := json.Marshal(protocol.PeerResultPayload{Peers: results})
		response, _ := json.Marshal(protocol.Message{
			Type:    protocol.TypePeerResult,
			Payload: resultPayload,
		})
		peer.Send(response)

	case protocol.TypeUpdateNickTag:
		var payload protocol.UpdateNickTagPayload
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			s.sendError(peer, protocol.ErrInvalidMsg, "invalid update_nick_tag payload")
			return
		}
		if payload.NickTag == "" {
			s.sendError(peer, protocol.ErrInvalidMsg, "nick_tag is empty")
			return
		}
		old, ok := s.hub.UpdateNickTag(peer.PublicKey, payload.NickTag)
		if !ok {
			return
		}
		s.log.Info("📛 nick_tag обновлён",
			slog.String("public_key", peer.PublicKey),
			slog.String("old", old),
			slog.String("new", payload.NickTag))

	case protocol.TypePresenceWatch:
		var payload protocol.PresencePayload
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			s.sendError(peer, protocol.ErrInvalidMsg, "invalid presence_watch payload")
			return
		}
		s.hub.UpdatePresence(peer, payload.Watch)

	case protocol.TypeGroupRegister:
		s.handleGroupRegister(peer, msg)
	case protocol.TypeGroupMemberAdd:
		s.handleGroupMemberChange(peer, msg, true)
	case protocol.TypeGroupMemberRemove:
		s.handleGroupMemberChange(peer, msg, false)
	case protocol.TypeGroupMessageSend:
		s.handleGroupMessageSend(peer, msg)
	case protocol.TypeGroupSync:
		s.handleGroupSync(peer, msg)

	case protocol.TypeSFUTokenReq:
		s.handleSFUTokenReq(peer, msg)

	case protocol.TypeLogPush:
		if s.logStore == nil {
			return
		}
		var payload protocol.LogPushPayload
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			s.log.Warn("невалидный log_push", slog.String("from", nick))
			return
		}
		// Клиент шлёт анонимный id установки; ник как имя источника не
		// принимаем — логи не должны лежать под ником.
		if !logstore.ValidClientSource(payload.Source) {
			s.log.Warn("log_push: недопустимый source, отброшен", slog.String("from", nick))
			return
		}
		peer.SetDiagID(payload.Source)
		complete := s.logStore.SaveChunk(payload.Source, payload.ChunkIdx, payload.ChunkTotal, payload.Data)
		if complete {
			s.log.Info("📋 логи получены",
				slog.String("from", nick),
				slog.Int("chunks", payload.ChunkTotal))
		}

	default:
		s.log.Warn("неизвестный тип сообщения",
			slog.String("type", msg.Type),
			slog.String("from", nick))
		s.sendError(peer, protocol.ErrInvalidMsg, "unknown message type: "+msg.Type)
	}
}

// serverLogSource — источник, под которым сохраняется собственный лог сигналки.
const serverLogSource = "signal-server"

// collectLogs просит клиентов этого узла прислать логи (log_request) и
// сохраняет собственный лог сигналки. targets пуст — все онлайн-клиенты;
// иначе — клиенты, у которых совпал nick#tag (как есть или в виде имени
// источника логов) или публичный ключ (hex); "signal-server" в списке — только
// собственный лог. Возвращает, скольким клиентам ушёл запрос.
// Общий код debug /collect и admin POST /admin/v1/logs/collect.
func (s *Server) collectLogs(targets []string) int {
	want := make(map[string]bool, len(targets))
	for _, t := range targets {
		want[t] = true
	}
	if s.logFile != "" && (len(targets) == 0 || want[serverLogSource]) {
		if err := s.logStore.SaveFromFile(serverLogSource, s.logFile); err != nil {
			s.log.Warn("не удалось сохранить лог сигналки", slog.String("error", err.Error()))
		}
	}
	msg, _ := json.Marshal(protocol.Message{Type: protocol.TypeLogRequest})
	return s.hub.SendWhere(func(p *hub.Peer) bool {
		if len(targets) == 0 {
			return true
		}
		nt := p.NickTag()
		id := p.DiagID()
		return want[nt] || want[logstore.SourceName(nt)] || want[p.PublicKey] || (id != "" && want[id])
	}, msg)
}

// onlineLogSources — имена источников логов клиентов, подключённых к этому
// узлу: анонимные id из log_push этой сессии и (для старых каталогов)
// logstore.SourceName от nick#tag.
func (s *Server) onlineLogSources() map[string]bool {
	peers := s.hub.PeerList()
	out := make(map[string]bool, len(peers))
	for _, p := range peers {
		out[logstore.SourceName(p.NickTag)] = true
	}
	for _, id := range s.hub.DiagIDs() {
		out[id] = true
	}
	return out
}

// StartDebug запускает debug HTTP сервер на отдельном порту. host пуст —
// 127.0.0.1: порт не должен быть виден снаружи.
func (s *Server) StartDebug(host string, port int) error {
	if s.logStore == nil {
		return fmt.Errorf("logStore не настроен")
	}

	mux := http.NewServeMux()

	// --- Collect: запросить логи у всех онлайн-клиентов ---
	collectHandler := func(w http.ResponseWriter, r *http.Request) {
		s.collectLogs(nil)

		peers := s.hub.PeerList()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status":          "log_request sent",
			"online_peers":    len(peers),
			"peers_requested": peers,
		})
	}
	mux.HandleFunc("POST /collect", collectHandler)
	mux.HandleFunc("POST /api/collect", collectHandler)

	// --- Peers: список подключённых пиров ---
	peersHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.hub.PeerList())
	}
	mux.HandleFunc("GET /peers", peersHandler)
	mux.HandleFunc("GET /api/peers", peersHandler)

	// --- Sources: список источников логов ---
	mux.HandleFunc("GET /api/sources", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		sources := s.logStore.ListSources()
		if sources == nil {
			sources = []logstore.SourceInfo{}
		}
		json.NewEncoder(w).Encode(sources)
	})

	// --- Status: статус сервера + пиры + источники ---
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		peers := s.hub.PeerList()
		sources := s.logStore.ListSources()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status":       "ok",
			"peers_online": len(peers),
			"peers":        peers,
			"sources":      len(sources),
		})
	})

	// --- Logs: чтение логов ---

	// GET /api/logs/{source}/latest — последний лог конкретного источника
	mux.HandleFunc("GET /api/logs/{source}/latest", func(w http.ResponseWriter, r *http.Request) {
		source := r.PathValue("source")
		if source == "" {
			http.Error(w, `{"error":"source required"}`, http.StatusBadRequest)
			return
		}
		data, err := s.logStore.ReadLogs(source, time.Time{})
		if err != nil || len(data) == 0 {
			http.Error(w, `{"error":"no logs found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Write(data)
	})

	// GET /api/logs/{source} — все логи источника (?since=RFC3339)
	mux.HandleFunc("GET /api/logs/{source}", func(w http.ResponseWriter, r *http.Request) {
		source := r.PathValue("source")
		if source == "" {
			http.Error(w, `{"error":"source required"}`, http.StatusBadRequest)
			return
		}
		var since time.Time
		if sinceStr := r.URL.Query().Get("since"); sinceStr != "" {
			t, err := time.Parse(time.RFC3339, sinceStr)
			if err != nil {
				http.Error(w, `{"error":"invalid since format, use RFC3339"}`, http.StatusBadRequest)
				return
			}
			since = t
		}
		data, err := s.logStore.ReadLogs(source, since)
		if err != nil || len(data) == 0 {
			http.Error(w, `{"error":"no logs found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Write(data)
	})

	// GET /api/logs — все логи всех источников
	mux.HandleFunc("GET /api/logs", func(w http.ResponseWriter, r *http.Request) {
		var since time.Time
		if sinceStr := r.URL.Query().Get("since"); sinceStr != "" {
			t, err := time.Parse(time.RFC3339, sinceStr)
			if err != nil {
				http.Error(w, `{"error":"invalid since format, use RFC3339"}`, http.StatusBadRequest)
				return
			}
			since = t
		}
		allLogs := s.logStore.ReadAllLogs(since)
		w.Header().Set("Content-Type", "application/json")
		result := make(map[string]string, len(allLogs))
		for k, v := range allLogs {
			result[k] = string(v)
		}
		json.NewEncoder(w).Encode(result)
	})

	if host == "" {
		host = "127.0.0.1"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	s.debugSrv = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	s.log.Info("debug HTTP запускается", slog.String("addr", addr))
	return s.debugSrv.ListenAndServe()
}

// StopDebug останавливает debug HTTP сервер
func (s *Server) StopDebug(ctx context.Context) error {
	if s.debugSrv == nil {
		return nil
	}
	return s.debugSrv.Shutdown(ctx)
}

// sendError шлёт ошибку аутентифицированному пиру через его write pump.
// Прямая запись в Conn здесь недопустима: readLoop-горутина пира гоняется
// с ping-loop и рассылками других горутин, gorilla это роняет.
func (s *Server) sendError(peer *hub.Peer, code, message string) {
	payload, _ := json.Marshal(protocol.ErrorPayload{Code: code, Message: message})
	data, _ := json.Marshal(protocol.Message{Type: protocol.TypeError, Payload: payload})
	_ = peer.Send(data)
}

// sendGroupSendError — отказ group_message_send с id отклонённого сообщения.
func (s *Server) sendGroupSendError(peer *hub.Peer, code, message, groupID, clientMsgID string) {
	payload, _ := json.Marshal(protocol.ErrorPayload{
		Code: code, Message: message, GroupID: groupID, ClientMsgID: clientMsgID,
	})
	data, _ := json.Marshal(protocol.Message{Type: protocol.TypeError, Payload: payload})
	_ = peer.Send(data)
}

// sendErrorConn — только для pre-auth фазы, когда Peer ещё не создан и conn
// принадлежит единственной горутине handleWS.
func (s *Server) sendErrorConn(conn *websocket.Conn, code, message string) {
	payload, _ := json.Marshal(protocol.ErrorPayload{Code: code, Message: message})
	data, _ := json.Marshal(protocol.Message{Type: protocol.TypeError, Payload: payload})
	_ = conn.SetWriteDeadline(time.Now().Add(authWriteWait))
	_ = conn.WriteMessage(websocket.TextMessage, data)
}
