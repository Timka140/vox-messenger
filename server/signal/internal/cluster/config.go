// Package cluster связывает сигнальные серверы (узлы) в кластер:
// WS-линки узел ↔ узел, реплицируемый каталог «pubkey клиента → узел»,
// пересылка сигналинга и presence. Контракт — docs/cluster-contract.md.
package cluster

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// PeerNode — другой узел кластера из конфига.
type PeerNode struct {
	Addr   string // ws://host:port/ws
	NodeID string // hex Ed25519 pubkey сигналки узла
}

// Config — параметры кластера узла.
type Config struct {
	SelfAddr  string
	Peers     []PeerNode
	SoftLimit int
}

// Enabled — есть хотя бы один другой узел.
func (c Config) Enabled() bool { return len(c.Peers) > 0 }

// ParseConfig разбирает CLUSTER_PEERS: элементы "ws://host:port/ws=<hex pubkey>"
// через ';' или ','. Сам узел (selfID) из списка исключается, поэтому одна и
// та же строка годится для всех узлов.
func ParseConfig(selfAddr, peers string, softLimit int, selfID string) (Config, error) {
	cfg := Config{SelfAddr: strings.TrimSpace(selfAddr), SoftLimit: softLimit}
	selfID = strings.ToLower(selfID)
	seen := make(map[string]bool)

	items := strings.FieldsFunc(peers, func(r rune) bool {
		return r == ';' || r == ',' || r == ' ' || r == '\n' || r == '\t'
	})
	for _, item := range items {
		i := strings.LastIndex(item, "=")
		if i <= 0 || i == len(item)-1 {
			return Config{}, fmt.Errorf("CLUSTER_PEERS: ожидается addr=pubkey, получено %q", item)
		}
		addr := strings.TrimSpace(item[:i])
		key := strings.ToLower(strings.TrimSpace(item[i+1:]))
		if !strings.HasPrefix(addr, "ws://") && !strings.HasPrefix(addr, "wss://") {
			return Config{}, fmt.Errorf("CLUSTER_PEERS: адрес %q должен начинаться с ws:// или wss://", addr)
		}
		if b, err := hex.DecodeString(key); err != nil || len(b) != 32 {
			return Config{}, fmt.Errorf("CLUSTER_PEERS: pubkey %q — не 64 hex", key)
		}
		if key == selfID {
			continue
		}
		if seen[key] {
			return Config{}, fmt.Errorf("CLUSTER_PEERS: узел %s указан дважды", key[:16])
		}
		seen[key] = true
		cfg.Peers = append(cfg.Peers, PeerNode{Addr: addr, NodeID: key})
	}
	return cfg, nil
}
