package network

// KnownSignalServer — запись в hardcoded-списке проверенных сигнальных серверов.
// Address — host:port (без схемы), Pubkey — hex-encoded Ed25519 ключ сервера.
// Используется для seed первой БД и для backfill pubkey при миграции.
type KnownSignalServer struct {
	Address string
	Pubkey  string // hex Ed25519; пустой допустим только в dev-сборках (TOFU)
}

// VPSSignalPubkey — Ed25519 публичный ключ основного VPS-сервера сигналки.
// Заполняется автоматически скриптом build/deploy-signal.sh из локального
// build/remote/vox-signal.key при каждом деплое. Пустой = TOFU при первом
// подключении (только если деплой ещё не запускался — dev-сборки).
//
// Эта строка ОТРЕДАКТИРОВАНА АВТОМАТИЧЕСКИ — формат должен сохраняться
// для регекспа в build/sync-signal-pubkey.sh.
const VPSSignalPubkey = "0000000000000000000000000000000000000000000000000000000000000001"

// VPS2SignalPubkey — ключ второго узла кластера (vps2, build/nodes/vps2.conf).
// Правится вручную: sync-signal-pubkey.sh обновляет только VPSSignalPubkey.
const VPS2SignalPubkey = "0000000000000000000000000000000000000000000000000000000000000002"

// KnownSignalServers — прибитый к версии бинаря список проверенных серверов
// (узлы кластера, docs/cluster-contract.md §6.1). Порядок не важен — узел
// выбирается по /healthz. Остальные узлы клиент узнаёт из server_list.
// Обновление: при компрометации ключа deploy-signal.sh регенерит .key и
// автоматически обновит VPSSignalPubkey; auto-update раскатает новый клиент.
var KnownSignalServers = []KnownSignalServer{
	{Address: "node1.example.org:9741", Pubkey: VPSSignalPubkey},
	{Address: "node2.example.org:9741", Pubkey: VPS2SignalPubkey},
}
