package network

// KnownSignalServer — запись в hardcoded-списке проверенных сигнальных серверов.
// Address — host:port (без схемы), Pubkey — hex-encoded Ed25519 ключ сервера.
// Используется для seed первой БД и для backfill pubkey при миграции.
type KnownSignalServer struct {
	Address string
	Pubkey  string // hex Ed25519; пустой допустим только в dev-сборках (TOFU)
}

// KnownSignalServers — прибитый к версии бинаря список проверенных серверов
// (узлы кластера, docs/cluster-contract.md §6.1). Порядок не важен — узел
// выбирается по /healthz. Остальные узлы клиент узнаёт из server_list.
//
// Список пуст: общих серверов у Vox нет, сервер пользователь поднимает сам и
// добавляет в настройках (запись source='manual'). Своя сборка может вписать
// сюда свои узлы — они попадут в БД как 'builtin'; встроенные записи, которых
// в списке больше нет, клиент удаляет сам (dropRetiredBuiltinServers).
var KnownSignalServers = []KnownSignalServer{}
