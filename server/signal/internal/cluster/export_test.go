package cluster

// DropLinks закрывает все линки узла — для тестов обрыва. Инициатор
// переподключится через DialMinBackoff.
func DropLinks(m *Manager) {
	m.mu.Lock()
	links := make([]*Link, 0, len(m.links))
	for _, l := range m.links {
		links = append(links, l)
	}
	m.mu.Unlock()
	for _, l := range links {
		l.Close()
	}
}
