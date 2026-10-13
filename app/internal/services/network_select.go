package services

// Выбор узла кластера при подключении к сигналке (docs/cluster-contract.md §6).

import (
	"crypto/tls"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"vox_app/internal/network"
)

const (
	// healthzTimeout — сколько ждём ответа /healthz от всех узлов разом.
	healthzTimeout = 3 * time.Second
	// overloadPenaltyMs — штраф за каждого клиента сверх soft_limit узла.
	overloadPenaltyMs = 100
	// stickyBonusMs — бонус последнему узлу, к которому подключались:
	// не прыгаем между узлами при равной задержке.
	stickyBonusMs = 50
	// signalConnectTimeout — одна попытка Connect к узлу.
	signalConnectTimeout = 10 * time.Second
)

// probeResult — ответ /healthz узла.
type probeResult struct {
	ok        bool
	rttMs     int64
	clients   int
	softLimit int
	nodeID    string
}

// probeFunc — опрос одного узла; в тестах подменяется.
type probeFunc func(ctx context.Context, addr string) probeResult

// probeHealthz — GET https://<host:port>/healthz (http:// для тестового ws://).
// Только для порядка перебора узлов: сертификат здесь не закреплён, подлинность
// узла проверяет вход v2 при подключении.
func probeHealthz(ctx context.Context, addr string) probeResult {
	scheme := "https://"
	base := addr
	if strings.HasPrefix(base, "ws://") {
		scheme, base = "http://", strings.TrimPrefix(base, "ws://")
	}
	base = strings.TrimPrefix(base, "wss://")
	if i := strings.Index(base, "/"); i >= 0 {
		base = base[:i]
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+base+"/healthz", nil)
	if err != nil {
		return probeResult{}
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}}
	if d := network.LocalDialer(healthzTimeout); d != nil {
		tr.DialContext = d.DialContext
	}
	client := &http.Client{Timeout: healthzTimeout, Transport: tr}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return probeResult{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return probeResult{}
	}
	var h struct {
		NodeID    string `json:"node_id"`
		Clients   int    `json:"clients"`
		SoftLimit int    `json:"soft_limit"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&h); err != nil {
		return probeResult{}
	}
	return probeResult{
		ok:        true,
		rttMs:     time.Since(start).Milliseconds(),
		clients:   h.Clients,
		softLimit: h.SoftLimit,
		nodeID:    strings.ToLower(h.NodeID),
	}
}

// serverScore — оценка узла (меньше — лучше): задержка плюс штраф за
// клиентов сверх soft_limit, минус бонус последнему успешному узлу.
func serverScore(r probeResult, sticky bool) int64 {
	score := r.rttMs
	if r.softLimit > 0 && r.clients > r.softLimit {
		score += int64(r.clients-r.softLimit) * overloadPenaltyMs
	}
	if sticky {
		score -= stickyBonusMs
	}
	return score
}

// orderServers — порядок перебора серверов: ответившие на /healthz по
// возрастанию оценки (ничья — случайно), затем не ответившие и узлы, чей
// node_id не совпал с закреплённым ключом, — в прежнем порядке.
func orderServers(ctx context.Context, entries []signalServerEntry, last string, probe probeFunc) ([]signalServerEntry, map[string]probeResult) {
	results := make(map[string]probeResult, len(entries))
	var mu sync.Mutex
	var wg sync.WaitGroup
	pctx, cancel := context.WithTimeout(ctx, healthzTimeout)
	defer cancel()
	for _, e := range entries {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			r := probe(pctx, addr)
			mu.Lock()
			results[addr] = r
			mu.Unlock()
		}(e.Addr)
	}
	wg.Wait()

	type scored struct {
		entry signalServerEntry
		score int64
	}
	var good []scored
	var rest []signalServerEntry
	for _, e := range entries {
		r := results[e.Addr]
		mismatch := r.ok && e.Pubkey != "" && r.nodeID != "" && !strings.EqualFold(e.Pubkey, r.nodeID)
		if !r.ok || mismatch {
			rest = append(rest, e)
			continue
		}
		good = append(good, scored{entry: e, score: serverScore(r, e.Addr == last)})
	}
	rand.Shuffle(len(good), func(i, j int) { good[i], good[j] = good[j], good[i] })
	sort.SliceStable(good, func(i, j int) bool { return good[i].score < good[j].score })

	out := make([]signalServerEntry, 0, len(entries))
	for _, g := range good {
		out = append(out, g.entry)
	}
	return append(out, rest...), results
}

// connectOrder — список серверов для перебора: dev-список (VOX_SIGNAL_SERVERS)
// как есть, иначе выбор узла по /healthz.
func (s *NetworkService) connectOrder(servers []signalServerEntry) []signalServerEntry {
	if len(servers) < 2 || devSignalServers() != nil {
		return servers
	}
	s.mu.RLock()
	last := s.lastSignalAddr
	s.mu.RUnlock()

	ordered, results := orderServers(context.Background(), servers, last, probeHealthz)
	var b strings.Builder
	for i, e := range ordered {
		if i > 0 {
			b.WriteString(", ")
		}
		r := results[e.Addr]
		if !r.ok {
			fmt.Fprintf(&b, "%s нет ответа", e.Addr)
			continue
		}
		fmt.Fprintf(&b, "%s %d мс %d/%d", e.Addr, r.rttMs, r.clients, r.softLimit)
	}
	s.log.Info("выбор узла сигналки", slog.String("order", b.String()), slog.String("last", last))
	return ordered
}

// connectOrdered — подключает client к первому доступному серверу после
// выбора узла. Не больше одного перехода по redirect за вызов: цель
// redirect пробуется сразу, затем остальные по порядку. Возвращает сервер,
// к которому подключились.
func (s *NetworkService) connectOrdered(client *network.SignalClient, servers []signalServerEntry,
	privKey ed25519.PrivateKey, pubKey ed25519.PublicKey, nickTag string) (signalServerEntry, error) {

	queue := s.connectOrder(servers)
	redirected := false
	var lastErr error
	for i := 0; i < len(queue); i++ {
		entry := queue[i]
		ctx, cancel := context.WithTimeout(context.Background(), signalConnectTimeout)
		err := client.Connect(ctx, entry.Addr, entry.Pubkey, privKey, pubKey, nickTag)
		cancel()

		var rd *network.RedirectError
		if errors.As(err, &rd) {
			lastErr = err
			if redirected {
				s.log.Warn("повторный redirect пропущен", slog.String("addr", entry.Addr), slog.String("redirect_to", rd.Addr))
				continue
			}
			redirected = true
			target := signalServerEntry{Addr: rd.Addr, Pubkey: rd.Pubkey}
			next := make([]signalServerEntry, 0, len(queue)+1)
			next = append(next, queue[:i+1]...)
			next = append(next, target)
			for _, e := range queue[i+1:] {
				if e.Addr != target.Addr {
					next = append(next, e)
				}
			}
			queue = next
			s.log.Info("узел перегружен, перехожу по redirect",
				slog.String("addr", entry.Addr), slog.String("redirect_to", rd.Addr))
			continue
		}
		if err != nil {
			s.log.Warn("не удалось подключиться к серверу",
				slog.String("addr", entry.Addr), slog.String("error", err.Error()))
			lastErr = err
			continue
		}

		// TOFU: если pubkey не был задан — сохраняем тот, что предъявил сервер.
		if entry.Pubkey == "" {
			s.tofuStorePubkey(entry.Addr, client.ObservedServerPubkey())
		}
		s.mu.Lock()
		s.lastSignalAddr = entry.Addr
		s.mu.Unlock()
		return entry, nil
	}
	if lastErr == nil {
		lastErr = errors.New("нет серверов")
	}
	return signalServerEntry{}, lastErr
}
