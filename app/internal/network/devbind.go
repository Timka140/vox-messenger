package network

import (
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

// Dev-bind: форс-выбор сетевого интерфейса для всего исходящего трафика.
//
// Зачем нужно: на dev-машине один процесс должен ходить ВНЕ активного VPN
// (или, наоборот, ТОЛЬКО через VPN), чтобы две локальных копии видели друг
// друга через разные публичные пути. Без этого ICE неизбежно выбирает host-
// пару через localhost/LAN и тестировать поведение через интернет невозможно.
//
// Когда активно (DevBindIP() != nil):
//   - WebSocket к сигналке использует net.Dialer{LocalAddr} с этим IP
//   - pion ICE собирает host-кандидаты ТОЛЬКО с указанного интерфейса
//     (whitelist переопределяет дефолтный blacklist виртуальных адаптеров)
//   - все исходящие сокеты выходят через этот NIC (на Windows это работает
//     через IP_UNICAST_IF, который Go ставит при LocalAddr-bind)
//
// Активируется флагом --bind-iface=<name|ip> в main.go.

var (
	devBindIP    atomic.Pointer[net.IP]
	devBindIface atomic.Pointer[string]
	forceRelay   atomic.Bool
)

// SetForceRelay включает форс-режим ICETransportPolicyRelay для всех
// PeerConnection'ов. Используется для тестов: App2 предложит ТОЛЬКО relay-
// кандидаты (TURN-VPS), и удалённый peer будет вынужден ходить через TURN —
// host/srflx-shortcut'ы исключены. Без этого на одной машине ICE выбирает
// host-пару и трафик не покидает loopback (см. логи v0.0070-debug:
// "ICE selected pair host=192.168.31.206 → host=192.168.31.206").
func SetForceRelay(v bool) {
	forceRelay.Store(v)
}

// ForceRelay возвращает true если включён форс ICETransportPolicyRelay.
func ForceRelay() bool {
	return forceRelay.Load()
}

// SetDevBindIP включает dev-bind на конкретный сетевой интерфейс.
// ip == nil выключает режим. ifaceName используется только для логов
// и матчинга в SetInterfaceFilter (по имени).
func SetDevBindIP(ip net.IP, ifaceName string) {
	if ip == nil {
		devBindIP.Store(nil)
		devBindIface.Store(nil)
		return
	}
	ipCopy := append(net.IP(nil), ip...)
	devBindIP.Store(&ipCopy)
	name := ifaceName
	devBindIface.Store(&name)
}

// DevBindIP возвращает выбранный IP или nil если режим выключен.
func DevBindIP() net.IP {
	p := devBindIP.Load()
	if p == nil {
		return nil
	}
	return *p
}

// DevBindIfaceName возвращает имя интерфейса (для filter по имени)
// или пустую строку если режим выключен.
func DevBindIfaceName() string {
	p := devBindIface.Load()
	if p == nil {
		return ""
	}
	return *p
}

// ResolveDevBindSpec принимает либо IPv4-адрес ("192.168.1.50"), либо имя
// интерфейса ("Ethernet", "Wi-Fi", "eth0"), либо подстроку имени. Возвращает
// (ip, iface name) пригодные для SetDevBindIP.
//
// Имя ищем case-insensitive: сначала точное совпадение, потом substring.
// Если substring даёт несколько кандидатов — ошибка с перечислением.
func ResolveDevBindSpec(spec string) (net.IP, string, error) {
	if spec == "" {
		return nil, "", nil
	}

	// 1) IPv4-адрес
	if ip := net.ParseIP(spec); ip != nil {
		ip4 := ip.To4()
		if ip4 == nil {
			return nil, "", fmt.Errorf("only IPv4 supported, got %s", spec)
		}
		// Найдём имя интерфейса (для логов и filter-by-name)
		ifaces, _ := net.Interfaces()
		for _, ifc := range ifaces {
			addrs, _ := ifc.Addrs()
			for _, a := range addrs {
				ipa, _, err := net.ParseCIDR(a.String())
				if err != nil {
					continue
				}
				if ipa.To4() != nil && ipa.Equal(ip4) {
					return ip4, ifc.Name, nil
				}
			}
		}
		// IP не на этой машине — всё равно используем (юзер знает что делает)
		return ip4, "", nil
	}

	// 2) Имя интерфейса
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, "", fmt.Errorf("enumerate interfaces: %w", err)
	}
	lower := strings.ToLower(spec)
	var exact *net.Interface
	var partials []net.Interface
	for i := range ifaces {
		name := strings.ToLower(ifaces[i].Name)
		if name == lower {
			exact = &ifaces[i]
			break
		}
		if strings.Contains(name, lower) {
			partials = append(partials, ifaces[i])
		}
	}
	var matched *net.Interface
	switch {
	case exact != nil:
		matched = exact
	case len(partials) == 1:
		matched = &partials[0]
	case len(partials) > 1:
		names := make([]string, 0, len(partials))
		for _, p := range partials {
			names = append(names, p.Name)
		}
		return nil, "", fmt.Errorf("interface %q ambiguous: %s", spec, strings.Join(names, ", "))
	default:
		return nil, "", fmt.Errorf("interface %q not found", spec)
	}

	addrs, err := matched.Addrs()
	if err != nil {
		return nil, "", fmt.Errorf("addrs of %q: %w", matched.Name, err)
	}
	for _, a := range addrs {
		ipa, _, err := net.ParseCIDR(a.String())
		if err != nil {
			continue
		}
		ip4 := ipa.To4()
		if ip4 != nil && !ip4.IsLoopback() && !ip4.IsLinkLocalUnicast() {
			return ip4, matched.Name, nil
		}
	}
	return nil, "", fmt.Errorf("interface %q has no usable IPv4", matched.Name)
}

// LocalDialer возвращает *net.Dialer с LocalAddr=DevBindIP() для bind на
// конкретный NIC, либо nil если dev-bind выключен (caller использует свой
// дефолтный dialer).
func LocalDialer(timeout time.Duration) *net.Dialer {
	ip := DevBindIP()
	if ip == nil {
		return nil
	}
	return &net.Dialer{
		Timeout:   timeout,
		LocalAddr: &net.TCPAddr{IP: ip},
	}
}
