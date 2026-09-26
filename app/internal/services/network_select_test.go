package services

import (
	"context"
	"strings"
	"testing"

	"vox_app/internal/network"
)

func fakeProbe(results map[string]probeResult) probeFunc {
	return func(_ context.Context, addr string) probeResult { return results[addr] }
}

func addrs(entries []signalServerEntry) string {
	var out []string
	for _, e := range entries {
		out = append(out, e.Addr)
	}
	return strings.Join(out, ",")
}

func TestOrderServersScoreAndFallback(t *testing.T) {
	keyA, keyD := strings.Repeat("a", 64), strings.Repeat("d", 64)
	entries := []signalServerEntry{
		{Addr: "c:1"},               // не ответил
		{Addr: "a:1", Pubkey: keyA}, // 80 мс
		{Addr: "d:1", Pubkey: keyD}, // ответил чужим node_id
		{Addr: "b:1"},               // 30 мс, но на 5 клиентов сверх лимита
	}
	res := map[string]probeResult{
		"a:1": {ok: true, rttMs: 80, clients: 10, softLimit: 400, nodeID: keyA},
		"b:1": {ok: true, rttMs: 30, clients: 405, softLimit: 400},
		"d:1": {ok: true, rttMs: 10, nodeID: strings.Repeat("e", 64)},
	}
	got, _ := orderServers(context.Background(), entries, "", fakeProbe(res))
	if addrs(got) != "a:1,b:1,c:1,d:1" {
		t.Fatalf("порядок %s: ответившие по оценке, затем остальные в прежнем порядке", addrs(got))
	}
}

func TestOrderServersSticky(t *testing.T) {
	entries := []signalServerEntry{{Addr: "x:1"}, {Addr: "y:1"}}
	res := map[string]probeResult{
		"x:1": {ok: true, rttMs: 100},
		"y:1": {ok: true, rttMs: 70},
	}
	if got, _ := orderServers(context.Background(), entries, "x:1", fakeProbe(res)); got[0].Addr != "x:1" {
		t.Fatalf("бонус −50 мс должен оставить последний узел первым: %s", addrs(got))
	}
	res["y:1"] = probeResult{ok: true, rttMs: 40}
	if got, _ := orderServers(context.Background(), entries, "x:1", fakeProbe(res)); got[0].Addr != "y:1" {
		t.Fatalf("узел заметно быстрее бонуса должен победить: %s", addrs(got))
	}
}

func TestConnectOrderDevListKeepsOrder(t *testing.T) {
	t.Setenv("ENV", "local")
	t.Setenv("VOX_SIGNAL_SERVERS", "z:1=,y:1=")
	n, _, _ := newBlockTestNetwork(t)
	entries := []signalServerEntry{{Addr: "z:1"}, {Addr: "y:1"}}
	if got := n.connectOrder(entries); addrs(got) != "z:1,y:1" {
		t.Fatalf("dev-список должен идти как задан: %s", addrs(got))
	}
}

func TestSeedDropsRetiredBuiltin(t *testing.T) {
	n, store, _ := newBlockTestNetwork(t)
	db := store.DB()
	// Старый клиент: встроенный узел под прежним адресом, плюс ручная и cluster-записи.
	if _, err := db.Exec(`INSERT INTO signal_servers (address, pubkey, source) VALUES
		('198.51.100.7:9741', ?, 'builtin'),
		('10.0.0.1:1', '', 'manual'),
		('10.0.0.2:1', '', 'cluster')`, network.VPSSignalPubkey); err != nil {
		t.Fatal(err)
	}
	n.seedKnownServers()

	exists := func(addr string) bool {
		var c int
		_ = db.QueryRow("SELECT COUNT(*) FROM signal_servers WHERE address = ?", addr).Scan(&c)
		return c > 0
	}
	if exists("198.51.100.7:9741") {
		t.Fatal("встроенная запись со старым адресом должна удалиться")
	}
	for _, k := range network.KnownSignalServers {
		if !exists(k.Address) {
			t.Fatalf("текущий встроенный узел %s должен остаться", k.Address)
		}
	}
	for _, keep := range []string{"10.0.0.1:1", "10.0.0.2:1"} {
		if !exists(keep) {
			t.Fatalf("%s (не builtin) не должен удаляться", keep)
		}
	}
}

func TestMergeClusterServers(t *testing.T) {
	n, store, _ := newBlockTestNetwork(t)
	db := store.DB()
	n.seedKnownServers() // builtin: vps1, vps2
	vps1, vps2 := network.KnownSignalServers[0].Address, network.KnownSignalServers[1].Address
	key1, key2, key3 := strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64)
	if _, err := db.Exec(`INSERT INTO signal_servers (address, pubkey, source) VALUES
		('10.0.0.1:1', '', 'manual'),
		('10.0.0.2:1', ?, 'cluster')`, key2); err != nil {
		t.Fatal(err)
	}

	err := n.mergeClusterServers([]network.ServerNode{
		{Addr: vps1, Pubkey: network.VPSSignalPubkey},
		{Addr: vps2, Pubkey: key3}, // ключ не совпадает с зашитым
		{Addr: "10.0.0.3:1", Pubkey: strings.ToUpper(key3)},
		{Addr: "10.0.0.1:1", Pubkey: key1},
		{Addr: "bad address", Pubkey: key1},
		{Addr: "10.0.0.4:1", Pubkey: "zz"},
	})
	if err != nil {
		t.Fatal(err)
	}

	type row struct{ pubkey, source string }
	get := func(addr string) (row, bool) {
		var r row
		err := db.QueryRow("SELECT pubkey, source FROM signal_servers WHERE address = ?", addr).Scan(&r.pubkey, &r.source)
		return r, err == nil
	}
	if _, ok := get("10.0.0.2:1"); ok {
		t.Fatal("устаревшая cluster-запись должна удалиться")
	}
	if r, ok := get("10.0.0.3:1"); !ok || r.source != "cluster" || r.pubkey != key3 {
		t.Fatalf("новый узел: %+v ok=%v", r, ok)
	}
	if r, _ := get("10.0.0.1:1"); r.source != "manual" || r.pubkey != key1 {
		t.Fatalf("ручная запись: источник сохраняется, пустой ключ заполняется: %+v", r)
	}
	if r, _ := get(vps2); r.source != "builtin" || r.pubkey != network.VPS2SignalPubkey {
		t.Fatalf("зашитый ключ vps2 не должен перезаписываться: %+v", r)
	}
	for _, bad := range []string{"bad address", "10.0.0.4:1"} {
		if _, ok := get(bad); ok {
			t.Fatalf("невалидный узел %q не должен попасть в БД", bad)
		}
	}

	// Новый список без 10.0.0.3 удаляет его, ручные и встроенные остаются.
	if err := n.mergeClusterServers(nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := get("10.0.0.3:1"); ok {
		t.Fatal("cluster-запись, которой нет в новом списке, должна удалиться")
	}
	for _, keep := range []string{"10.0.0.1:1", vps1, vps2} {
		if _, ok := get(keep); !ok {
			t.Fatalf("%s не должен удаляться server_list", keep)
		}
	}
}
