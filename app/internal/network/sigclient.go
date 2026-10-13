package network

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Типы сообщений (дублируем из server/signal/pkg/protocol чтобы не тянуть зависимость)
const (
	TypeChallenge     = "challenge"
	TypeAuth          = "auth"
	TypeAuthOK        = "auth_ok"
	TypeOffer         = "offer"
	TypeAnswer        = "answer"
	TypeICE           = "ice"
	TypePeerSearch    = "peer_search"
	TypePeerResult    = "peer_result"
	TypePeerOnline    = "peer_online"
	TypePeerOffline   = "peer_offline"
	TypeUpdateNickTag = "update_nick_tag"
	TypePresenceWatch = "presence_watch" // кому виден мой статус «в сети» (полная замена списка)
	TypeFriendRequest = "friend_request"
	TypeFriendAccept  = "friend_accept"
	TypeFriendReject  = "friend_reject"
	TypeFriendRemove  = "friend_remove"
	TypeError         = "error"
	TypeICEServers    = "ice_servers"
	TypeServerList    = "server_list" // все узлы кластера (после ice_servers)
	TypeRedirect      = "redirect"    // вместо auth_ok: узел перегружен, иди на addr
	TypeCallInvite    = "call_invite"
	TypeCallAccept    = "call_accept"
	TypeCallReject    = "call_reject"
	TypeCallEnd       = "call_end"

	// Debug: сбор логов
	TypeLogPush    = "log_push"
	TypeLogRequest = "log_request"

	// Server relay для больших групп (зеркало server/signal/pkg/protocol)
	TypeGroupRegister     = "group_register"
	TypeGroupRegisterOK   = "group_register_ok"
	TypeGroupMemberAdd    = "group_member_add"
	TypeGroupMemberRemove = "group_member_remove"
	TypeGroupMessageSend  = "group_message_send"
	TypeGroupMessageNew   = "group_message_new"
	TypeGroupSync         = "group_sync"
	TypeGroupSyncResult   = "group_sync_result"
	TypeGroupDeliveryMode = "group_delivery_mode"
	// TypeGroupSyncHint — узел догнал сообщения группы и шлёт подсказку вместо
	// поштучной рассылки; клиент забирает их group_sync.
	TypeGroupSyncHint = "group_sync_hint"
)

// Режимы доставки группы (зеркало server/signal/pkg/protocol).
const (
	DeliveryModeP2P    = "p2p"
	DeliveryModeServer = "server"
)

// Message — сообщение протокола сигнального сервера
type Message struct {
	Type    string          `json:"type"`
	To      string          `json:"to,omitempty"`
	From    string          `json:"from,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	// PcId — discriminator транспорта между двумя PeerConnection'ами одного пира.
	// 0 = media (voice + messages), 1 = files (attachments). Только для offer/answer/ice.
	PcId int `json:"pc_id,omitempty"`
}

type ChallengePayload struct {
	Challenge    string `json:"challenge"`     // hex-encoded random bytes (server → client)
	ServerPubkey string `json:"server_pubkey"` // hex-encoded Ed25519 public key сервера
}

type AuthPayload struct {
	PublicKey       string `json:"public_key"` // hex-encoded Ed25519 public key клиента
	NickTag         string `json:"nick_tag"`
	Signature       string `json:"signature"`        // hex-encoded Ed25519 signature of server challenge
	ClientChallenge string `json:"client_challenge"` // hex-encoded random bytes для mutual auth
	// Presence — кому виден мой статус «в сети» (друзья и участники общих
	// групп). Старый сервер поле игнорирует — статус виден всем, как раньше.
	Presence *PresencePayload `json:"presence,omitempty"`
	// RedirectOK — клиент понимает redirect вместо auth_ok (узел кластера
	// перегружен и отправляет на соседа).
	RedirectOK bool `json:"redirect_ok,omitempty"`
	// AppVersion — версия клиента для статистики админки (сколько онлайн на
	// какой версии). Старый сервер поле игнорирует.
	AppVersion string `json:"app_version,omitempty"`
}

// AppVersion — версия этой сборки (main.AppVersion), уходит в auth.
// «dev» и пустая не отправляются.
var AppVersion string

// ServerNode — узел кластера из server_list (зеркало server/signal/pkg/protocol).
type ServerNode struct {
	Addr   string `json:"addr"` // host:port
	Pubkey string `json:"pubkey"`
}

// ServerListPayload — все узлы кластера; сервер шлёт после ice_servers.
type ServerListPayload struct {
	Nodes []ServerNode `json:"nodes"`
}

// RedirectPayload — узел перегружен, подключиться к Addr. ServerSignature —
// подпись client_challenge, как в auth_ok.
type RedirectPayload struct {
	Addr            string `json:"addr"`
	Pubkey          string `json:"pubkey"`
	ServerSignature string `json:"server_signature"`
}

// RedirectError — Connect получил подписанный redirect: подключиться к Addr
// с ожидаемым ключом Pubkey.
type RedirectError struct {
	Addr   string
	Pubkey string
}

func (e *RedirectError) Error() string { return "signal redirect to " + e.Addr }

// PresencePayload — watch-список (зеркало server/signal/pkg/protocol): публичные
// ключи, которым сервер показывает мой статус «в сети».
type PresencePayload struct {
	Watch []string `json:"watch"`
}

type PeerSearchPayload struct {
	Query string `json:"query"`
}

type PeerInfo struct {
	PublicKey string `json:"public_key"`
	NickTag   string `json:"nick_tag"`
	Online    bool   `json:"online"`
}

type PeerResultPayload struct {
	Peers []PeerInfo `json:"peers"`
}

type FriendRequestPayload struct {
	NickTag string `json:"nick_tag"`
}

// UpdateNickTagPayload — сообщает серверу новый nick_tag без переподключения.
type UpdateNickTagPayload struct {
	NickTag string `json:"nick_tag"`
}

type CallInvitePayload struct {
	CallID       string   `json:"call_id"`
	NickTag      string   `json:"nick_tag"`
	Participants []string `json:"participants,omitempty"`
	GroupID      string   `json:"group_id,omitempty"`
}

type CallResponsePayload struct {
	CallID  string `json:"call_id"`
	GroupID string `json:"group_id,omitempty"`
}

type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	PeerID  string `json:"peer_id,omitempty"`
	// Отказ group_message_send: какое сообщение отклонено.
	GroupID     string `json:"group_id,omitempty"`
	ClientMsgID string `json:"client_msg_id,omitempty"`
}

// LogPushPayload — отправка логов на сервер (с поддержкой chunking)
type LogPushPayload struct {
	Source     string `json:"source"`
	ChunkIdx   int    `json:"chunk_idx"`
	ChunkTotal int    `json:"chunk_total"`
	Data       string `json:"data"`
}

// GroupRegisterPayload — клиент регистрирует группу на сервере.
type GroupRegisterPayload struct {
	GroupID string   `json:"group_id"`
	Members []string `json:"members"`
	Admins  []string `json:"admins"`
}

// GroupRegisterOKPayload — ответ на group_register.
type GroupRegisterOKPayload struct {
	GroupID     string `json:"group_id"`
	CurrentSeq  int64  `json:"current_seq"`
	MemberCount int    `json:"member_count"`
}

// GroupMemberChangePayload — add/remove участника (для админа).
type GroupMemberChangePayload struct {
	GroupID   string `json:"group_id"`
	PublicKey string `json:"public_key"`
	IsAdmin   bool   `json:"is_admin,omitempty"`
}

// GroupMessageSendPayload — клиент отправляет сообщение в группу.
type GroupMessageSendPayload struct {
	GroupID     string `json:"group_id"`
	ClientMsgID string `json:"client_msg_id"`
	Payload     string `json:"payload"`
	Encrypted   bool   `json:"encrypted"`
	ClientTsMs  int64  `json:"client_ts_ms"`
}

// GroupMessageNewPayload — сервер рассылает новое сообщение участникам.
type GroupMessageNewPayload struct {
	GroupID     string `json:"group_id"`
	Seq         int64  `json:"seq"`
	AuthorKey   string `json:"author_key"`
	ClientMsgID string `json:"client_msg_id"`
	Payload     string `json:"payload"`
	Encrypted   bool   `json:"encrypted"`
	ServerTsMs  int64  `json:"server_ts_ms"`
	ClientTsMs  int64  `json:"client_ts_ms"`
	// OriginNode — узел, чей журнал выдал seq (seq уникален в паре группа + узел).
	OriginNode string `json:"origin_node,omitempty"`
}

// GroupSyncPayload — клиент запрашивает пропущенные сообщения. Cursors —
// последний известный seq по каждому журналу {origin_node: seq}; всегда
// отправляется (пустой = с начала): без курсоров сервер отвечает ошибкой.
type GroupSyncPayload struct {
	GroupID string           `json:"group_id"`
	Limit   int              `json:"limit,omitempty"`
	Cursors map[string]int64 `json:"cursors"`
}

// GroupSyncResultPayload — ответ на group_sync.
type GroupSyncResultPayload struct {
	GroupID  string                   `json:"group_id"`
	Messages []GroupMessageNewPayload `json:"messages"`
	HasMore  bool                     `json:"has_more"`
}

// GroupDeliveryModePayload — уведомление об изменении режима доставки.
type GroupDeliveryModePayload struct {
	GroupID string `json:"group_id"`
	Mode    string `json:"mode"`
}

// GroupSyncHintPayload — подсказка сделать group_sync по группе.
type GroupSyncHintPayload struct {
	GroupID string `json:"group_id"`
}

// ICEServerConfig — один ICE-сервер полученный от сигнального сервера
type ICEServerConfig struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// ICEServersPayload — список ICE-серверов
type ICEServersPayload struct {
	Servers []ICEServerConfig `json:"servers"`
}

// AuthOKPayload — содержимое auth_ok от сервера.
// ServerSignature — Ed25519 подпись над client_challenge для mutual auth.
// Дубль из server/signal/pkg/protocol/message.go — app и signal разные go.mod.
type AuthOKPayload struct {
	ServerSignature string             `json:"server_signature"`
	LatestRelease   *LatestReleaseInfo `json:"latest_release,omitempty"`
	SFUEndpoint     string             `json:"sfu_endpoint,omitempty"`
	SFUSecret       string             `json:"sfu_secret,omitempty"`
	Presence        bool               `json:"presence,omitempty"` // сервер понимает watch-списки
}

// LatestReleaseInfo — метаданные последнего релиза.
type LatestReleaseInfo struct {
	Version   string `json:"version"`
	Force     bool   `json:"force"`
	Changelog string `json:"changelog"`
	// Revoked — отозванные версии: клиент на такой версии ставит Version,
	// даже если она старше (откат из админки).
	Revoked []string `json:"revoked,omitempty"`
}

func authAppVersion() string {
	if AppVersion == "" || AppVersion == "dev" {
		return ""
	}
	return AppVersion
}

// MessageHandler — обработчик входящих сообщений от сигнального сервера
type MessageHandler func(msg *Message)

// SignalClient — WebSocket клиент для подключения к сигнальному серверу
type SignalClient struct {
	mu      sync.Mutex
	conn    *websocket.Conn
	log     *slog.Logger
	addr    string
	handler MessageHandler
	done    chan struct{}
	closed  bool

	// Колбэк при потере связи (не вызывается при нормальном Close)
	onDisconnect func()

	// Колбэк при запросе логов от сервера
	onLogRequest func()

	// Колбэк при получении информации о последнем релизе (из auth_ok payload)
	onLatestRelease func(*LatestReleaseInfo)
	// Колбэк при получении SFU конфигурации (из auth_ok payload)
	onSFUConfig func(endpoint, secret string)

	// Каналы для синхронных запросов
	peerResultCh chan []PeerInfo

	// ICE-серверы полученные от сигнального сервера при аутентификации
	serverICEServers []ICEServerConfig

	// Pubkey сервера, фактически предъявленный в challenge и проверенный mutual-auth-ом.
	// Используется caller'ом для TOFU writeback в БД (когда expectedPubkey был пустой).
	observedServerPubkey string

	// Активный heartbeat: read deadline сбрасывается при любом сообщении/pong от сервера.
	// Если сервер не шлёт ничего за pongWait — ReadMessage возвращает ошибку → reconnect.
	pongWait time.Duration

	// Статус «в сети» только для watch-списка: presenceFn даёт список для auth
	// при каждом (пере)подключении; serverPresence — сервер из auth_ok понимает
	// presence_watch (старому слать нельзя — ответит invalid_message).
	presenceFn     func() []string
	serverPresence bool
}

// SetPresenceProvider — источник watch-списка для auth. nil — список не
// шлётся (статус виден всем, как у старых клиентов).
func (c *SignalClient) SetPresenceProvider(fn func() []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.presenceFn = fn
}

// SupportsPresence — сервер текущего соединения понимает presence_watch.
func (c *SignalClient) SupportsPresence() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serverPresence
}

// SendPresenceWatch — новый watch-список (полная замена).
func (c *SignalClient) SendPresenceWatch(keys []string) error {
	if keys == nil {
		keys = []string{}
	}
	return c.sendTyped(TypePresenceWatch, PresencePayload{Watch: keys})
}

// ObservedServerPubkey возвращает hex-encoded pubkey сервера, проверенный
// в последнем успешном Connect. Пустая строка до первого Connect.
func (c *SignalClient) ObservedServerPubkey() string {
	return c.observedServerPubkey
}

// ServerICEServers возвращает ICE-серверы полученные от сигнального сервера
func (c *SignalClient) ServerICEServers() []ICEServerConfig {
	return c.serverICEServers
}

// SetOnDisconnect устанавливает колбэк при потере связи с сигнальным сервером
func (c *SignalClient) SetOnDisconnect(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onDisconnect = fn
}

// SetOnLogRequest устанавливает колбэк при запросе логов от сервера
func (c *SignalClient) SetOnLogRequest(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onLogRequest = fn
}

// SetOnLatestRelease устанавливает колбэк при получении информации о последнем релизе
// (из auth_ok payload). Вызывается при каждом успешном (re)connect к сигналке.
func (c *SignalClient) SetOnLatestRelease(fn func(*LatestReleaseInfo)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onLatestRelease = fn
}

// SetOnSFUConfig устанавливает колбэк при получении SFU конфигурации от сигнального сервера.
func (c *SignalClient) SetOnSFUConfig(fn func(endpoint, secret string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onSFUConfig = fn
}

// NewSignalClient создаёт клиент (не подключается)
func NewSignalClient(log *slog.Logger, handler MessageHandler) *SignalClient {
	return &SignalClient{
		log:          log,
		handler:      handler,
		done:         make(chan struct{}),
		peerResultCh: make(chan []PeerInfo, 1),
	}
}

// ErrServerPubkeyMismatch возвращается когда сервер предъявил pubkey, отличный от ожидаемого
// (потенциальный MITM либо смена ключа сервера). Сравнение делается ДО подписи auth,
// чтобы не передавать атакующему даже сигнатуру challenge.
var ErrServerPubkeyMismatch = fmt.Errorf("server identity mismatch")

// Connect подключается к сигнальному серверу и проходит mutual auth.
// expectedPubkey — hex-encoded Ed25519 pubkey ожидаемого сервера; если пустой,
// клиент принимает любой pubkey (TOFU). Фактически предъявленный pubkey доступен
// через ObservedServerPubkey() после успешного Connect — caller сохраняет его в БД.
func (c *SignalClient) Connect(ctx context.Context, addr, expectedPubkey string, privKey ed25519.PrivateKey, pubKey ed25519.PublicKey, nickTag string) error {
	const op = "SignalClient.Connect"

	// Если адрес без path — добавляем /ws
	wsURL := "ws://" + addr
	if !strings.Contains(addr, "/") {
		wsURL = "ws://" + addr + "/ws"
	}
	c.log.Info("подключение к сигнальному серверу", slog.String("addr", addr))

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}
	// Dev-bind: --bind-iface заставляет TCP-сокет к сигналке выйти через
	// конкретный NIC (на Windows — IP_UNICAST_IF из LocalAddr). Иначе пакеты
	// уходят по дефолтному роуту (через VPN, если активен).
	if d := LocalDialer(10 * time.Second); d != nil {
		dialer.NetDialContext = d.DialContext
	}
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("%s: dial: %w", op, err)
	}

	// Аутентификация: ждём challenge
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		conn.Close()
		return fmt.Errorf("%s: read challenge: %w", op, err)
	}

	var challengeMsg Message
	if err := json.Unmarshal(raw, &challengeMsg); err != nil {
		conn.Close()
		return fmt.Errorf("%s: parse challenge: %w", op, err)
	}
	if challengeMsg.Type != TypeChallenge {
		conn.Close()
		return fmt.Errorf("%s: expected challenge, got %s", op, challengeMsg.Type)
	}

	var challenge ChallengePayload
	if err := json.Unmarshal(challengeMsg.Payload, &challenge); err != nil {
		conn.Close()
		return fmt.Errorf("%s: parse challenge payload: %w", op, err)
	}

	// Сравнение server_pubkey ДО отправки auth: если ожидаемый pubkey задан и не
	// совпадает — отказываем без передачи атакующему даже подписи challenge.
	serverPubBytes, err := hex.DecodeString(challenge.ServerPubkey)
	if err != nil || len(serverPubBytes) != ed25519.PublicKeySize {
		conn.Close()
		return fmt.Errorf("%s: invalid server_pubkey from server", op)
	}
	if expectedPubkey != "" && !strings.EqualFold(expectedPubkey, challenge.ServerPubkey) {
		conn.Close()
		return fmt.Errorf("%s: %w (expected %s, got %s)", op, ErrServerPubkeyMismatch, expectedPubkey, challenge.ServerPubkey)
	}

	// Подписываем server-side challenge
	challengeBytes, err := hex.DecodeString(challenge.Challenge)
	if err != nil {
		conn.Close()
		return fmt.Errorf("%s: decode challenge: %w", op, err)
	}
	signature := ed25519.Sign(privKey, challengeBytes)

	// Свой client_challenge для mutual auth (32 байта, сервер подпишет в auth_ok)
	clientChallenge := make([]byte, 32)
	if _, err := rand.Read(clientChallenge); err != nil {
		conn.Close()
		return fmt.Errorf("%s: client_challenge rand: %w", op, err)
	}

	// Watch-список — в auth: сервер рассылает peer_online при регистрации,
	// политика к этому моменту должна быть известна.
	c.mu.Lock()
	presenceFn := c.presenceFn
	c.mu.Unlock()
	var presence *PresencePayload
	if presenceFn != nil {
		watch := presenceFn()
		if watch == nil {
			watch = []string{}
		}
		presence = &PresencePayload{Watch: watch}
	}

	authPayload, err := json.Marshal(AuthPayload{
		PublicKey:       hex.EncodeToString(pubKey),
		NickTag:         nickTag,
		Signature:       hex.EncodeToString(signature),
		ClientChallenge: hex.EncodeToString(clientChallenge),
		Presence:        presence,
		RedirectOK:      true,
		AppVersion:      authAppVersion(),
	})
	if err != nil {
		conn.Close()
		return fmt.Errorf("%s: marshal auth payload: %w", op, err)
	}
	authMsg, err := json.Marshal(Message{
		Type:    TypeAuth,
		Payload: authPayload,
	})
	if err != nil {
		conn.Close()
		return fmt.Errorf("%s: marshal auth msg: %w", op, err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, authMsg); err != nil {
		conn.Close()
		return fmt.Errorf("%s: send auth: %w", op, err)
	}

	// Ждём auth_ok
	_, raw, err = conn.ReadMessage()
	if err != nil {
		conn.Close()
		return fmt.Errorf("%s: read auth_ok: %w", op, err)
	}
	conn.SetReadDeadline(time.Time{}) // сбросить deadline

	var authOK Message
	if err := json.Unmarshal(raw, &authOK); err != nil {
		conn.Close()
		return fmt.Errorf("%s: parse auth_ok: %w", op, err)
	}
	// Узел кластера перегружен и отправляет на соседа. Принимаем redirect,
	// только если он подписан тем же ключом, что предъявлен в challenge.
	if authOK.Type == TypeRedirect {
		conn.Close()
		var rp RedirectPayload
		if err := json.Unmarshal(authOK.Payload, &rp); err != nil {
			return fmt.Errorf("%s: parse redirect: %w", op, err)
		}
		sig, err := hex.DecodeString(rp.ServerSignature)
		if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(serverPubBytes, clientChallenge, sig) {
			return fmt.Errorf("%s: redirect: server_signature verification failed", op)
		}
		target, err := hex.DecodeString(rp.Pubkey)
		if rp.Addr == "" || err != nil || len(target) != ed25519.PublicKeySize {
			return fmt.Errorf("%s: redirect: invalid target", op)
		}
		c.log.Info("сигнальный сервер перенаправил на другой узел",
			slog.String("addr", addr), slog.String("redirect_to", rp.Addr))
		return &RedirectError{Addr: rp.Addr, Pubkey: strings.ToLower(rp.Pubkey)}
	}
	if authOK.Type != TypeAuthOK {
		conn.Close()
		if authOK.Type == TypeError {
			var errPayload ErrorPayload
			json.Unmarshal(authOK.Payload, &errPayload)
			return fmt.Errorf("%s: auth failed: %s", op, errPayload.Message)
		}
		return fmt.Errorf("%s: expected auth_ok, got %s", op, authOK.Type)
	}

	// Парсим auth_ok payload + проверяем server_signature над client_challenge.
	// Сервер обязан вернуть валидную подпись: без неё не доказано владение приватником.
	if len(authOK.Payload) == 0 {
		conn.Close()
		return fmt.Errorf("%s: empty auth_ok payload (legacy server?)", op)
	}
	var okPayload AuthOKPayload
	if err := json.Unmarshal(authOK.Payload, &okPayload); err != nil {
		conn.Close()
		return fmt.Errorf("%s: parse auth_ok payload: %w", op, err)
	}
	serverSig, err := hex.DecodeString(okPayload.ServerSignature)
	if err != nil || len(serverSig) != ed25519.SignatureSize {
		conn.Close()
		return fmt.Errorf("%s: invalid server_signature", op)
	}
	if !ed25519.Verify(serverPubBytes, clientChallenge, serverSig) {
		conn.Close()
		return fmt.Errorf("%s: server_signature verification failed", op)
	}
	c.observedServerPubkey = challenge.ServerPubkey
	c.mu.Lock()
	c.serverPresence = okPayload.Presence
	c.mu.Unlock()

	if okPayload.LatestRelease != nil {
		c.log.Info("обнаружен последний релиз на сервере",
			slog.String("version", okPayload.LatestRelease.Version),
			slog.Bool("force", okPayload.LatestRelease.Force))
		if c.onLatestRelease != nil {
			c.onLatestRelease(okPayload.LatestRelease)
		}
	}
	if okPayload.SFUEndpoint != "" && c.onSFUConfig != nil {
		c.onSFUConfig(okPayload.SFUEndpoint, okPayload.SFUSecret)
	}

	// Читаем ice_servers (сервер отправляет сразу после auth_ok).
	// Если первым прилетело НЕ ice_servers (queued offer/call_invite/peer_online
	// при реконнекте) — не выбрасываем его (иначе пир ждёт answer до dtls
	// timeout), а откладываем и передадим handler'у после старта readLoop.
	var earlyMsg *Message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, raw, err = conn.ReadMessage()
	if err == nil {
		var iceMsg Message
		if json.Unmarshal(raw, &iceMsg) == nil {
			if iceMsg.Type == TypeICEServers {
				var payload ICEServersPayload
				if json.Unmarshal(iceMsg.Payload, &payload) == nil && len(payload.Servers) > 0 {
					c.serverICEServers = payload.Servers
					c.log.Info("ICE-серверы получены от сигнального сервера", slog.Int("count", len(payload.Servers)))
				}
			} else {
				m := iceMsg
				earlyMsg = &m
			}
		}
	}
	conn.SetReadDeadline(time.Time{})

	// Настраиваем ping handler + read deadline для обнаружения обрыва связи.
	// Сервер шлёт ping каждые 30s. Если за 45s не пришло ни ping, ни данных — считаем связь потерянной.
	c.pongWait = 45 * time.Second
	conn.SetReadDeadline(time.Now().Add(c.pongWait))
	conn.SetPingHandler(func(msg string) error {
		conn.SetReadDeadline(time.Now().Add(c.pongWait))
		// Отвечаем pong (дефолтный handler заменён, нужно отвечать вручную)
		return conn.WriteControl(websocket.PongMessage, []byte(msg), time.Now().Add(time.Second))
	})

	c.mu.Lock()
	c.conn = conn
	c.addr = addr
	c.closed = false
	c.done = make(chan struct{})
	c.mu.Unlock()

	c.log.Info("аутентификация успешна", slog.String("addr", addr))

	// Запускаем чтение сообщений
	go c.readLoop()

	// Отложенное первое сообщение (не ice_servers) отдаём handler'у уже после
	// старта readLoop, чтобы сохранить порядок и не потерять queued offer.
	if earlyMsg != nil && c.handler != nil {
		go c.handler(earlyMsg)
	}

	return nil
}

// signalWriteWait — дедлайн одной записи в WS. На полумёртвом TCP (заполненный
// буфер, типично для CGNAT/VPN-обрывов) без дедлайна WriteMessage висит минуты,
// держа c.mu → блокируются все Send (в т.ч. pion OnICECandidate-колбэки) и Close.
const signalWriteWait = 5 * time.Second

// Send отправляет сообщение через WebSocket
func (c *SignalClient) Send(msg *Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil || c.closed {
		return fmt.Errorf("not connected")
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(signalWriteWait))
	if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		// gorilla делает writeErr «липким»: после таймаута/ошибки записи все
		// последующие WriteMessage отвергаются, но readLoop продолжает читать,
		// IsConnected()==true, и клиент молча теряет offer/answer/ICE. Рвём
		// сокет — readLoop получит ошибку чтения, вызовет onDisconnect и
		// запустится авто-реконнект. closed не трогаем: это не штатный Close.
		c.log.Warn("ошибка записи в сигналку, рву соединение",
			slog.String("error", err.Error()))
		_ = c.conn.Close()
		return err
	}
	return nil
}

// sendTyped — внутренний helper: маршалит payload и отправляет Message{Type, Payload}.
func (c *SignalClient) sendTyped(t string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", t, err)
	}
	return c.Send(&Message{Type: t, Payload: raw})
}

// SendGroupRegister регистрирует группу на сервере signal (server relay).
// members/admins — список publicKey (hex).
func (c *SignalClient) SendGroupRegister(groupID string, members, admins []string) error {
	return c.sendTyped(TypeGroupRegister, GroupRegisterPayload{
		GroupID: groupID,
		Members: members,
		Admins:  admins,
	})
}

// SendGroupMessageRelay шлёт текстовое сообщение группы через server relay.
func (c *SignalClient) SendGroupMessageRelay(groupID, clientMsgID, payload string, encrypted bool, clientTsMs int64) error {
	return c.sendTyped(TypeGroupMessageSend, GroupMessageSendPayload{
		GroupID:     groupID,
		ClientMsgID: clientMsgID,
		Payload:     payload,
		Encrypted:   encrypted,
		ClientTsMs:  clientTsMs,
	})
}

// GroupSendFrameSize — размер кадра websocket, который уйдёт для
// group_message_send, той же сериализацией, что и в Send. Считать нужно
// именно кадр: JSON экранирует <, > и & шестью байтами, и тело в пределах
// лимита может дать кадр больше предела чтения сервера — тот рвёт
// соединение без ошибки.
func GroupSendFrameSize(groupID, clientMsgID, payload string, encrypted bool, clientTsMs int64) int {
	raw, err := json.Marshal(GroupMessageSendPayload{
		GroupID:     groupID,
		ClientMsgID: clientMsgID,
		Payload:     payload,
		Encrypted:   encrypted,
		ClientTsMs:  clientTsMs,
	})
	if err != nil {
		return 0
	}
	data, err := json.Marshal(&Message{Type: TypeGroupMessageSend, Payload: raw})
	if err != nil {
		return 0
	}
	return len(data)
}

// SendGroupMemberChange уведомляет сервер об изменении членства (только админ).
// add=true → group_member_add, add=false → group_member_remove.
func (c *SignalClient) SendGroupMemberChange(groupID, peerID string, add, isAdmin bool) error {
	t := TypeGroupMemberAdd
	if !add {
		t = TypeGroupMemberRemove
	}
	return c.sendTyped(t, GroupMemberChangePayload{
		GroupID:   groupID,
		PublicKey: peerID,
		IsAdmin:   isAdmin,
	})
}

// SendGroupSync запрашивает у сервера сообщения после курсоров журналов
// {origin_node: seq}; журнала нет в cursors — с начала.
func (c *SignalClient) SendGroupSync(groupID string, cursors map[string]int64, limit int) error {
	if cursors == nil {
		cursors = map[string]int64{}
	}
	return c.sendTyped(TypeGroupSync, GroupSyncPayload{
		GroupID: groupID,
		Limit:   limit,
		Cursors: cursors,
	})
}

// SendLogPush отправляет логи на сервер с chunking (48KB на чанк)
func (c *SignalClient) SendLogPush(source string, logData []byte) error {
	const chunkSize = 48000 // ~48KB raw → ~64KB base64

	encoded := base64.StdEncoding.EncodeToString(logData)
	total := int(math.Ceil(float64(len(encoded)) / float64(chunkSize)))
	if total == 0 {
		total = 1
	}

	for i := 0; i < total; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if end > len(encoded) {
			end = len(encoded)
		}

		payload, err := json.Marshal(LogPushPayload{
			Source:     source,
			ChunkIdx:   i,
			ChunkTotal: total,
			Data:       encoded[start:end],
		})
		if err != nil {
			return fmt.Errorf("marshal log_push chunk %d: %w", i, err)
		}

		if err := c.Send(&Message{Type: TypeLogPush, Payload: payload}); err != nil {
			return fmt.Errorf("send log_push chunk %d/%d: %w", i+1, total, err)
		}
	}

	c.log.Debug("логи отправлены на сервер",
		slog.String("source", source),
		slog.Int("chunks", total),
		slog.Int("bytes", len(logData)))
	return nil
}

// SearchPeers отправляет запрос поиска пиров (асинхронно)
func (c *SignalClient) SearchPeers(query string) error {
	payload, err := json.Marshal(PeerSearchPayload{Query: query})
	if err != nil {
		return fmt.Errorf("marshal peer search: %w", err)
	}
	return c.Send(&Message{
		Type:    TypePeerSearch,
		Payload: payload,
	})
}

// SearchPeersSync отправляет запрос поиска и ждёт результат
func (c *SignalClient) SearchPeersSync(query string, timeout time.Duration) ([]PeerInfo, error) {
	// Очищаем канал от старых результатов
	select {
	case <-c.peerResultCh:
	default:
	}

	if err := c.SearchPeers(query); err != nil {
		return nil, err
	}

	select {
	case result := <-c.peerResultCh:
		return result, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("search timeout")
	}
}

// SendFriendRequest отправляет заявку в друзья через signal server
// SendUpdateNickTag сообщает серверу новый nick_tag для текущей сессии
// без разрыва WS/P2P. Идентичность уже подтверждена при auth.
func (c *SignalClient) SendUpdateNickTag(nickTag string) error {
	return c.sendTyped(TypeUpdateNickTag, UpdateNickTagPayload{NickTag: nickTag})
}

func (c *SignalClient) SendFriendRequest(toPubKey, nickTag string) error {
	payload, err := json.Marshal(FriendRequestPayload{NickTag: nickTag})
	if err != nil {
		return fmt.Errorf("marshal friend request: %w", err)
	}
	return c.Send(&Message{
		Type:    TypeFriendRequest,
		To:      toPubKey,
		Payload: payload,
	})
}

// SendFriendAccept отправляет принятие заявки
func (c *SignalClient) SendFriendAccept(toPubKey, nickTag string) error {
	payload, err := json.Marshal(FriendRequestPayload{NickTag: nickTag})
	if err != nil {
		return fmt.Errorf("marshal friend accept: %w", err)
	}
	return c.Send(&Message{
		Type:    TypeFriendAccept,
		To:      toPubKey,
		Payload: payload,
	})
}

// SendFriendReject отправляет отклонение заявки
func (c *SignalClient) SendFriendReject(toPubKey string) error {
	return c.Send(&Message{
		Type: TypeFriendReject,
		To:   toPubKey,
	})
}

// SendFriendRemove уведомляет об удалении из друзей
func (c *SignalClient) SendFriendRemove(toPubKey string) error {
	return c.Send(&Message{
		Type: TypeFriendRemove,
		To:   toPubKey,
	})
}

// SendCallInvite отправляет приглашение в звонок
func (c *SignalClient) SendCallInvite(toPubKey, callID, nickTag string, participants []string, groupID string) error {
	payload, err := json.Marshal(CallInvitePayload{
		CallID:       callID,
		NickTag:      nickTag,
		Participants: participants,
		GroupID:      groupID,
	})
	if err != nil {
		return fmt.Errorf("marshal call invite: %w", err)
	}
	return c.Send(&Message{
		Type:    TypeCallInvite,
		To:      toPubKey,
		Payload: payload,
	})
}

// SendCallAccept отправляет принятие звонка с groupId для голосового канала
func (c *SignalClient) SendCallAccept(toPubKey, callID, groupID string) error {
	payload, err := json.Marshal(CallResponsePayload{CallID: callID, GroupID: groupID})
	if err != nil {
		return fmt.Errorf("marshal call accept: %w", err)
	}
	return c.Send(&Message{
		Type:    TypeCallAccept,
		To:      toPubKey,
		Payload: payload,
	})
}

// SendCallReject отправляет отклонение звонка
func (c *SignalClient) SendCallReject(toPubKey, callID string) error {
	payload, err := json.Marshal(CallResponsePayload{CallID: callID})
	if err != nil {
		return fmt.Errorf("marshal call reject: %w", err)
	}
	return c.Send(&Message{
		Type:    TypeCallReject,
		To:      toPubKey,
		Payload: payload,
	})
}

// SendCallEnd отправляет завершение/выход из звонка
func (c *SignalClient) SendCallEnd(toPubKey, callID string) error {
	payload, err := json.Marshal(CallResponsePayload{CallID: callID})
	if err != nil {
		return fmt.Errorf("marshal call end: %w", err)
	}
	return c.Send(&Message{
		Type:    TypeCallEnd,
		To:      toPubKey,
		Payload: payload,
	})
}

// Close закрывает соединение. Идемпотентен.
// conn НЕ зануляется: readLoop работает с собственной копией указателя, а
// зануление под гонку с его ReadMessage давало nil-deref/краш на Windows
// (concurrent Close+Read). Мёртвый conn после Close безвреден — closed=true
// отсекает Send, а сам сокет закрыт.
func (c *SignalClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil && !c.closed {
		c.closed = true
		close(c.done)
		_ = c.conn.SetWriteDeadline(time.Now().Add(time.Second))
		c.conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		c.conn.Close()
		c.log.Info("отключён от сигнального сервера", slog.String("addr", c.addr))
	}
}

// IsConnected проверяет подключение
func (c *SignalClient) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	// conn больше не зануляется в Close — признак живости именно !closed.
	return c.conn != nil && !c.closed
}

// Addr возвращает адрес текущего сервера
func (c *SignalClient) Addr() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.addr
}

func (c *SignalClient) readLoop() {
	// Локальная копия conn/done, взятая один раз под локом. Дальше readLoop
	// работает только с ней — поле c.conn может конкурентно читаться другими
	// методами, но здесь мы его не трогаем, поэтому гонки Close+Read нет.
	c.mu.Lock()
	conn := c.conn
	done := c.done
	pongWait := c.pongWait
	c.mu.Unlock()

	defer func() {
		// Проверяем, был ли это нормальный Close (closed=true) или обрыв
		c.mu.Lock()
		wasNormalClose := c.closed
		fn := c.onDisconnect
		c.mu.Unlock()

		c.Close()

		if !wasNormalClose && fn != nil {
			go fn()
		}
	}()

	for {
		select {
		case <-done:
			return
		default:
		}

		_, raw, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				c.log.Warn("ошибка чтения от сигналки", slog.String("error", err.Error()))
			}
			return
		}

		// Сбрасываем read deadline при каждом полученном сообщении
		if pongWait > 0 {
			conn.SetReadDeadline(time.Now().Add(pongWait))
		}

		var msg Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			c.log.Warn("невалидный JSON от сигналки", slog.String("error", err.Error()))
			continue
		}

		// Перехватываем log_request — вызываем колбэк
		if msg.Type == TypeLogRequest {
			c.mu.Lock()
			fn := c.onLogRequest
			c.mu.Unlock()
			if fn != nil {
				go fn()
			}
			continue
		}

		// Перехватываем peer_result для синхронного поиска
		if msg.Type == TypePeerResult {
			var result PeerResultPayload
			if err := json.Unmarshal(msg.Payload, &result); err == nil {
				select {
				case c.peerResultCh <- result.Peers:
				default:
				}
			}
		}

		if c.handler != nil {
			c.handler(&msg)
		}
	}
}
