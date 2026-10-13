package protocol

import (
	"encoding/json"
	"regexp"
)

// Типы сообщений между клиентом и сигнальным сервером
const (
	// Аутентификация
	TypeChallenge  = "challenge"   // сервер → клиент: challenge для подписи
	TypeAuth       = "auth"        // клиент → сервер: подпись challenge + publicKey + nick#tag
	TypeAuthOK     = "auth_ok"     // сервер → клиент: аутентификация успешна
	TypeICEServers = "ice_servers" // сервер → клиент: ICE/TURN конфигурация
	TypeServerList = "server_list" // сервер → клиент: все узлы кластера (после ice_servers)
	TypeRedirect   = "redirect"    // сервер → клиент вместо auth_ok: узел перегружен, подключись к addr

	// WebRTC сигнализация
	TypeOffer  = "offer"  // клиент → сервер → клиент: SDP offer
	TypeAnswer = "answer" // клиент → сервер → клиент: SDP answer
	TypeICE    = "ice"    // клиент → сервер → клиент: ICE candidate

	// Пиры
	TypePeerSearch    = "peer_search"     // клиент → сервер: поиск пиров по нику
	TypePeerResult    = "peer_result"     // сервер → клиент: результаты поиска
	TypePeerOnline    = "peer_online"     // сервер → клиент: пир появился онлайн
	TypePeerOffline   = "peer_offline"    // сервер → клиент: пир ушёл офлайн
	TypeUpdateNickTag = "update_nick_tag" // клиент → сервер: обновить nick_tag без реконнекта
	TypePresenceWatch = "presence_watch"  // клиент → сервер: кому виден мой статус «в сети» (полная замена списка)

	// Друзья (relay: клиент → сервер → клиент)
	TypeFriendRequest = "friend_request" // заявка в друзья
	TypeFriendAccept  = "friend_accept"  // принятие заявки
	TypeFriendReject  = "friend_reject"  // отклонение заявки
	TypeFriendRemove  = "friend_remove"  // удаление из друзей

	// Звонки (relay: клиент → сервер → клиент)
	TypeCallInvite = "call_invite" // приглашение в звонок
	TypeCallAccept = "call_accept" // принятие звонка
	TypeCallReject = "call_reject" // отклонение звонка
	TypeCallEnd    = "call_end"    // выход/завершение звонка

	// Ошибки
	TypeError = "error" // сервер → клиент: ошибка

	// Debug: сбор логов
	TypeLogPush    = "log_push"    // клиент → сервер: вот мои логи (chunked)
	TypeLogRequest = "log_request" // сервер → клиент: пришли логи

	// Групповой server relay (для групп >50 участников)
	TypeGroupRegister     = "group_register"      // клиент → сервер: регистрирует группу в server registry
	TypeGroupRegisterOK   = "group_register_ok"   // сервер → клиент: ack создания + текущий seq
	TypeGroupMemberAdd    = "group_member_add"    // админ → сервер: добавить участника
	TypeGroupMemberRemove = "group_member_remove" // админ → сервер: удалить участника
	TypeGroupMessageSend  = "group_message_send"  // клиент → сервер: отправить сообщение в группу
	TypeGroupMessageNew   = "group_message_new"   // сервер → клиент: новое сообщение (fan-out)
	TypeGroupSync         = "group_sync"          // клиент → сервер: sync since last seq
	TypeGroupSyncResult   = "group_sync_result"   // сервер → клиент: пропущенные сообщения
	TypeGroupDeliveryMode = "group_delivery_mode" // сервер → клиент: режим доставки изменён
	TypeGroupSyncHint     = "group_sync_hint"     // сервер → клиент: узел догнал сообщения группы, сделай group_sync

	// Кластер: узел ↔ узел по тому же WS-порту (docs/cluster-contract.md).
	// Принимающий узел шлёт обычный challenge; инициатор отвечает node_auth.
	TypeNodeAuth        = "node_auth"         // инициатор → узел: подпись challenge + свой client_challenge
	TypeNodeAuthOK      = "node_auth_ok"      // узел → инициатор: подпись client_challenge
	TypeNodePing        = "node_ping"         // heartbeat линка
	TypeNodePong        = "node_pong"         // ответ на node_ping (тот же ts_ms → RTT)
	TypeNodeDirPut      = "node_dir_put"      // запись каталога своего клиента: появился / сменил ник, watch
	TypeNodeDirDel      = "node_dir_del"      // свой клиент отключился
	TypeNodeDirSnapshot = "node_dir_snapshot" // все свои клиенты, сразу после установки линка
	TypeNodeFwd         = "node_fwd"          // пересылка сигналинга клиенту другого узла
	TypeNodeFwdErr      = "node_fwd_err"      // адресата node_fwd на узле уже нет

	// Кластер: репликация группового relay (журнал на узел, §7).
	TypeNodeGrpMsg     = "node_grp_msg"     // новое сообщение из журнала узла-отправителя
	TypeNodeGrpMsgs    = "node_grp_msgs"    // пачка сообщений, которых нет у получателя (догон)
	TypeNodeGrpMembers = "node_grp_members" // строки состава группы (LWW) после register / add / remove
	TypeNodeGrpState   = "node_grp_state"   // при подъёме линка: группы, составы и головы журналов
)

// Message — базовое сообщение протокола
type Message struct {
	Type    string          `json:"type"`
	To      string          `json:"to,omitempty"`      // publicKey получателя (для offer/answer/ice)
	From    string          `json:"from,omitempty"`    // publicKey отправителя (заполняется сервером)
	Payload json.RawMessage `json:"payload,omitempty"` // содержимое зависит от type
	// PcId — discriminator транспорта между двумя PeerConnection'ами одного пира.
	// 0 = media (voice + messages), 1 = files (attachments). Используется только
	// для offer/answer/ice. Сервер не интерпретирует — пробрасывает as-is.
	PcId int `json:"pc_id,omitempty"`
}

// ChallengePayload — сервер отправляет challenge при подключении.
// ServerPubkey предъявляется клиенту: клиент сверяет с ожидаемым ключом
// (hardcoded list или TOFU из БД).
type ChallengePayload struct {
	Challenge    string `json:"challenge"`     // hex-encoded random bytes (server → client)
	ServerPubkey string `json:"server_pubkey"` // hex-encoded Ed25519 public key сервера
}

// AuthPayload — клиент отвечает подписью + своим challenge для mutual auth.
type AuthPayload struct {
	PublicKey       string `json:"public_key"`       // hex-encoded Ed25519 public key клиента
	NickTag         string `json:"nick_tag"`         // "nickname#2e93f592048a62c7"
	Signature       string `json:"signature"`        // hex-encoded Ed25519 signature of server challenge
	ClientChallenge string `json:"client_challenge"` // hex-encoded random bytes (client → server) для mutual auth
	// Presence — кому виден статус «в сети» этого клиента (друзья и участники
	// общих групп). nil — старый клиент без списка: статус виден всем, как раньше.
	Presence *PresencePayload `json:"presence,omitempty"`
	// RedirectOK — клиент понимает redirect вместо auth_ok. Старому клиенту
	// перегруженный узел redirect не шлёт.
	RedirectOK bool `json:"redirect_ok,omitempty"`
	// AppVersion — версия приложения клиента ("v0.00.000.0082"), для
	// статистики версий в admin API. Необязательное: не проходит
	// ValidAppVersion — игнорируется, старые клиенты его не шлют.
	AppVersion string `json:"app_version,omitempty"`
	// AuthVersion — 2: подписи с меткой и отпечатком TLS (internal/auth/v2.go).
	// 0 — старый клиент: вход только на порту без TLS.
	AuthVersion int `json:"auth_version,omitempty"`
}

var appVersionRe = regexp.MustCompile(`^v\d+\.\d{2}\.\d{3}\.\d{4}$`)

// ValidAppVersion — формат версии приложения: ≤ 32 символов, vN.NN.NNN.NNNN.
func ValidAppVersion(s string) bool {
	return len(s) <= 32 && appVersionRe.MatchString(s)
}

// ServerNode — узел кластера в формате клиента: addr "host:port".
type ServerNode struct {
	Addr   string `json:"addr"`
	Pubkey string `json:"pubkey"`
}

// ServerListPayload — все узлы кластера (docs/cluster-contract.md §6.1).
type ServerListPayload struct {
	Nodes []ServerNode `json:"nodes"`
}

// RedirectPayload — узел перегружен, клиенту подключиться к addr.
// ServerSignature — подпись client_challenge, как в auth_ok: клиент
// принимает redirect только от узла, чей ключ проверил.
type RedirectPayload struct {
	Addr            string `json:"addr"`
	Pubkey          string `json:"pubkey"`
	ServerSignature string `json:"server_signature"`
}

// PresencePayload — watch-список: публичные ключи (hex), которым сервер шлёт
// peer_online / peer_offline этого клиента и отвечает peer_offline_err.
type PresencePayload struct {
	Watch []string `json:"watch"`
}

// PeerSearchPayload — запрос поиска пиров
type PeerSearchPayload struct {
	Query string `json:"query"` // nick или nick#tag (частичное совпадение)
}

// PeerInfo — информация о пире
type PeerInfo struct {
	PublicKey string `json:"public_key"`
	NickTag   string `json:"nick_tag"`
	Online    bool   `json:"online"`
}

// PeerResultPayload — результаты поиска
type PeerResultPayload struct {
	Peers []PeerInfo `json:"peers"`
}

// FriendRequestPayload — данные заявки в друзья
type FriendRequestPayload struct {
	NickTag string `json:"nick_tag"` // ник#тег отправителя
}

// UpdateNickTagPayload — клиент сообщает серверу новый nick_tag без переподключения.
// Идентичность уже проверена при auth — подпись не требуется.
type UpdateNickTagPayload struct {
	NickTag string `json:"nick_tag"`
}

// ErrorPayload — описание ошибки
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	PeerID  string `json:"peer_id,omitempty"` // publicKey пира, к которому относится ошибка
	// Отказ group_message_send: какое сообщение отклонено, чтобы клиент снял
	// его с повтора.
	GroupID     string `json:"group_id,omitempty"`
	ClientMsgID string `json:"client_msg_id,omitempty"`
}

// AuthOKPayload — сервер отправляет после успешной аутентификации.
// ServerSignature — Ed25519 подпись сервера над клиентским ClientChallenge
// (mutual auth, клиент проверяет против ожидаемого pubkey сервера).
type AuthOKPayload struct {
	ServerSignature string             `json:"server_signature"` // hex-encoded Ed25519 signature of client_challenge
	LatestRelease   *LatestReleaseInfo `json:"latest_release,omitempty"`
	SFUEndpoint     string             `json:"sfu_endpoint,omitempty"`
	SFUSecret       string             `json:"sfu_secret,omitempty"`
	// Presence — сервер понимает watch-списки (presence в auth, presence_watch).
	Presence bool `json:"presence,omitempty"`
}

// LatestReleaseInfo — метаданные последнего релиза (без списка файлов).
// Полный манифест получается через HTTP endpoint /api/update/manifest.
type LatestReleaseInfo struct {
	Version   string `json:"version"`   // например "v0.00.000.0001"
	Force     bool   `json:"force"`     // принудительное обновление — UI заблокирован
	Changelog string `json:"changelog"` // краткое описание изменений
	// Revoked — отозванные версии: клиент на такой версии ставит Version,
	// даже если она старше его собственной (откат из админки).
	Revoked []string `json:"revoked,omitempty"`
}

// ICEServerConfig — один ICE-сервер
type ICEServerConfig struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// ICEServersPayload — список ICE-серверов от сигнального сервера
type ICEServersPayload struct {
	Servers []ICEServerConfig `json:"servers"`
}

// CallInvitePayload — данные приглашения в звонок
type CallInvitePayload struct {
	CallID       string   `json:"call_id"`
	NickTag      string   `json:"nick_tag"`
	Participants []string `json:"participants,omitempty"`
}

// CallResponsePayload — ответ на звонок (accept/reject/end)
type CallResponsePayload struct {
	CallID  string `json:"call_id"`
	GroupID string `json:"group_id,omitempty"`
}

// LogPushPayload — клиент отправляет логи на сервер (с поддержкой chunking)
type LogPushPayload struct {
	Source     string `json:"source"`      // анонимный id установки (anon-<16 hex>)
	ChunkIdx   int    `json:"chunk_idx"`   // индекс чанка (0-based)
	ChunkTotal int    `json:"chunk_total"` // всего чанков
	Data       string `json:"data"`        // base64-encoded лог-данные
}

// Коды ошибок
const (
	ErrAuthFailed   = "auth_failed"
	ErrPeerNotFound = "peer_not_found"
	ErrPeerOffline  = "peer_offline_err"
	ErrInvalidMsg   = "invalid_message"
	ErrNotAuthed    = "not_authenticated"
	// ErrSessionReplaced — тот же ключ подключился к другому узлу кластера
	// позже; эта сессия закрывается.
	ErrSessionReplaced = "session_replaced"

	// Группы
	ErrGroupNotFound   = "group_not_found"
	ErrNotGroupMember  = "not_group_member"
	ErrNotGroupAdmin   = "not_group_admin"
	ErrRateLimited     = "rate_limited"
	ErrMessageTooLarge = "message_too_large"
)

// Режимы доставки в группе
const (
	DeliveryModeP2P    = "p2p"    // P2P mesh (для групп ≤50)
	DeliveryModeServer = "server" // Server relay (для групп >50)
)

// GroupRegisterPayload — клиент регистрирует группу на сервере.
// Используется при создании группы >50 или при миграции с P2P.
type GroupRegisterPayload struct {
	GroupID string   `json:"group_id"` // UUID группы (совпадает с клиентским)
	Members []string `json:"members"`  // список publicKey участников
	Admins  []string `json:"admins"`   // список publicKey админов (подмножество Members)
}

// GroupRegisterOKPayload — ответ на group_register.
type GroupRegisterOKPayload struct {
	GroupID     string `json:"group_id"`
	CurrentSeq  int64  `json:"current_seq"` // последний seq в группе (0 для новой)
	MemberCount int    `json:"member_count"`
}

// GroupMemberChangePayload — add/remove участника.
type GroupMemberChangePayload struct {
	GroupID   string `json:"group_id"`
	PublicKey string `json:"public_key"`
	IsAdmin   bool   `json:"is_admin,omitempty"` // только для add
}

// GroupMessageSendPayload — клиент отправляет сообщение в группу.
type GroupMessageSendPayload struct {
	GroupID     string `json:"group_id"`
	ClientMsgID string `json:"client_msg_id"` // UUID от клиента, для дедупликации
	Payload     string `json:"payload"`       // plaintext или зашифрованный блоб (base64 или plain)
	Encrypted   bool   `json:"encrypted"`     // true если payload зашифрован e2e
	ClientTsMs  int64  `json:"client_ts_ms"`  // клиентский timestamp (UTC ms)
}

// GroupMessageNewPayload — fan-out нового сообщения от сервера.
type GroupMessageNewPayload struct {
	GroupID     string `json:"group_id"`
	Seq         int64  `json:"seq"`           // монотонный номер в группе (сервер)
	AuthorKey   string `json:"author_key"`    // publicKey автора
	ClientMsgID string `json:"client_msg_id"` // для дедупликации на клиенте
	Payload     string `json:"payload"`
	Encrypted   bool   `json:"encrypted"`
	ServerTsMs  int64  `json:"server_ts_ms"` // серверный timestamp приёма (UTC ms)
	ClientTsMs  int64  `json:"client_ts_ms"` // клиентский timestamp (с поправкой если >5min drift)
	// OriginNode — узел, чей журнал выдал seq (seq уникален в паре группа + узел).
	OriginNode string `json:"origin_node,omitempty"`
}

// GroupSyncPayload — клиент запрашивает пропущенные сообщения.
type GroupSyncPayload struct {
	GroupID string `json:"group_id"`
	Limit   int    `json:"limit,omitempty"` // max 500, default 100
	// Cursors — последний известный seq по каждому журналу {origin_node: seq};
	// журнала нет в карте — он отдаётся с начала. Обязательное поле: без него
	// сервер отвечает ошибкой. Без omitempty, чтобы пустая карта из Go-кода
	// уходила как {}, — вызывающий передаёт не-nil карту.
	Cursors map[string]int64 `json:"cursors"`
}

// GroupSyncResultPayload — ответ на group_sync.
type GroupSyncResultPayload struct {
	GroupID  string                   `json:"group_id"`
	Messages []GroupMessageNewPayload `json:"messages"`
	HasMore  bool                     `json:"has_more"` // true если есть ещё за пределами limit
}

// GroupDeliveryModePayload — уведомление об изменении режима доставки группы.
type GroupDeliveryModePayload struct {
	GroupID string `json:"group_id"`
	Mode    string `json:"mode"` // DeliveryModeP2P или DeliveryModeServer
}

// GroupSyncHintPayload — узел получил догоном сообщения группы; вместо
// поштучной рассылки клиент сам забирает их постраничным group_sync.
type GroupSyncHintPayload struct {
	GroupID string `json:"group_id"`
}

// NodeDirEntry — запись каталога кластера: клиент pubkey подключён к узлу
// node_id. Private=false — клиент без watch-списка, статус виден всем.
type NodeDirEntry struct {
	PubKey    string   `json:"pubkey"`
	NickTag   string   `json:"nick_tag"`
	NodeID    string   `json:"node_id"`
	SessionID string   `json:"session_id"`
	TSms      int64    `json:"ts_ms"` // время auth_ok по часам домашнего узла
	Private   bool     `json:"private,omitempty"`
	Watch     []string `json:"watch,omitempty"`
}

// NodeAuthPayload — инициатор линка: свой node_id (hex pubkey), подпись
// challenge принимающего узла и свой challenge для взаимной проверки.
type NodeAuthPayload struct {
	NodeID          string `json:"node_id"`
	Signature       string `json:"signature"`
	ClientChallenge string `json:"client_challenge"`
}

// NodeAuthOKPayload — подпись принимающего узла над client_challenge.
type NodeAuthOKPayload struct {
	ServerSignature string `json:"server_signature"`
}

// NodePingPayload — node_ping / node_pong. Clients — клиентов на узле
// отправителя: по ним узел выбирает, куда перенаправить клиента.
type NodePingPayload struct {
	TSms    int64 `json:"ts_ms"`
	Clients int   `json:"clients"`
}

// NodeDirDelPayload — клиент pubkey с сессией session_id отключился.
type NodeDirDelPayload struct {
	PubKey    string `json:"pubkey"`
	SessionID string `json:"session_id"`
}

// NodeDirSnapshotPayload — все клиенты узла-отправителя.
type NodeDirSnapshotPayload struct {
	Entries []NodeDirEntry `json:"entries"`
}

// NodeFwdPayload — сообщение клиента from для клиента msg.to на узле-получателе.
type NodeFwdPayload struct {
	From string  `json:"from"`
	Msg  Message `json:"msg"`
}

// NodeGrpMsg — сообщение группы из журнала узла origin_node.
type NodeGrpMsg struct {
	GroupID     string `json:"group_id"`
	OriginNode  string `json:"origin_node"`
	Seq         int64  `json:"seq"`
	AuthorKey   string `json:"author_key"`
	ClientMsgID string `json:"client_msg_id"`
	Payload     string `json:"payload"`
	Encrypted   bool   `json:"encrypted,omitempty"`
	ServerTsMs  int64  `json:"server_ts_ms"`
	ClientTsMs  int64  `json:"client_ts_ms"`
}

// NodeGrpMember — строка состава группы; побеждает больший ts_ms, при
// равенстве — удаление.
type NodeGrpMember struct {
	PubKey  string `json:"pubkey"`
	IsAdmin bool   `json:"is_admin,omitempty"`
	Removed bool   `json:"removed,omitempty"`
	TSms    int64  `json:"ts_ms"`
}

// NodeGrpMembersPayload — изменения состава группы.
type NodeGrpMembersPayload struct {
	GroupID   string          `json:"group_id"`
	CreatedMs int64           `json:"created_ms"`
	Members   []NodeGrpMember `json:"members"`
}

// NodeGrpGroupState — группа узла: головы журналов и полный состав (состав
// только при подъёме линка, в периодической сверке его нет).
type NodeGrpGroupState struct {
	GroupID   string           `json:"group_id"`
	CreatedMs int64            `json:"created_ms"`
	Members   []NodeGrpMember  `json:"members,omitempty"`
	Heads     map[string]int64 `json:"heads"` // origin_node → max seq
}

// NodeGrpStatePayload — все группы узла, шлётся при подъёме линка.
type NodeGrpStatePayload struct {
	Groups []NodeGrpGroupState `json:"groups"`
}

// NodeGrpMsgsPayload — пачка сообщений для догона.
type NodeGrpMsgsPayload struct {
	Messages []NodeGrpMsg `json:"messages"`
}

// NodeFwdErrPayload — адресата to на узле нет; узел from решает, говорить ли
// отправителю peer_offline_err.
type NodeFwdErrPayload struct {
	From string `json:"from"`
	To   string `json:"to"`
	Code string `json:"code"`
}
