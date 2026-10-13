package services

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

// Настройка «Отправлять анонимные логи» (Настройки → О программе).
// Логи уходят на сигналку под случайным id установки, не под ником:
// id показан в настройках, чтобы пользователь мог назвать его при обращении.
const (
	settingDiagLogsEnabled = "diag_logs_enabled" // "0" — выключено; пусто/"1" — включено
	settingDiagLogID       = "diag_log_id"
	diagLogIDPrefix        = "anon-"
)

var diagLogIDMu sync.Mutex

// diagLogsEnabled — отправка логов включена (по умолчанию — да).
func (s *NetworkService) diagLogsEnabled() bool {
	if s.store == nil {
		return false
	}
	var v string
	s.store.DB().QueryRow("SELECT value FROM settings WHERE key = ?", settingDiagLogsEnabled).Scan(&v)
	return v != "0"
}

// diagLogID — id логов этой установки; при первом вызове создаётся и
// сохраняется: "anon-" + 16 hex из crypto/rand.
func (s *NetworkService) diagLogID() (string, error) {
	diagLogIDMu.Lock()
	defer diagLogIDMu.Unlock()
	var id string
	s.store.DB().QueryRow("SELECT value FROM settings WHERE key = ?", settingDiagLogID).Scan(&id)
	if validDiagLogID(id) {
		return id, nil
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id = diagLogIDPrefix + hex.EncodeToString(b)
	if _, err := s.store.DB().Exec(
		"INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)", settingDiagLogID, id); err != nil {
		return "", err
	}
	return id, nil
}

// validDiagLogID — формат, который принимает сигналка (logstore.ValidClientSource).
func validDiagLogID(id string) bool {
	if len(id) != len(diagLogIDPrefix)+16 || id[:len(diagLogIDPrefix)] != diagLogIDPrefix {
		return false
	}
	for _, c := range id[len(diagLogIDPrefix):] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// GetDiagLogID — id логов для показа в настройках.
func (s *NetworkService) GetDiagLogID() (string, error) {
	return s.diagLogID()
}
