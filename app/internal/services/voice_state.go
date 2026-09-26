package services

// Mute / Deafen state API.
//
// Тривиальные state-методы для UI кнопок «микрофон» / «наушники».
// Все методы захватывают s.mu только для атомарного присваивания и
// сразу после релиза вызывают broadcastVoiceStatus, чтобы пиры видели
// обновлённый VoiceStatus в ~real-time.
//
// Mute и Deafen связаны:
//   - Deafen → muted = true (нельзя слышать = нельзя говорить)
//   - Undeafen → восстанавливает muted до значения mutedBeforeDeafen

// MuteVoice отключает микрофон (не выходит из канала).
func (s *VoiceService) MuteVoice() error {
	s.mu.Lock()
	s.muted = true
	s.mu.Unlock()
	s.broadcastVoiceStatus()
	return nil
}

// UnmuteVoice включает микрофон.
func (s *VoiceService) UnmuteVoice() error {
	s.mu.Lock()
	s.muted = false
	s.mu.Unlock()
	s.broadcastVoiceStatus()
	return nil
}

// DeafenVoice отключает наушники + автоматически мутит микрофон.
// Запоминает прежнее состояние mute, чтобы UndeafenVoice мог его восстановить.
func (s *VoiceService) DeafenVoice() error {
	s.mu.Lock()
	s.mutedBeforeDeafen = s.muted
	s.deafened = true
	s.muted = true
	s.mu.Unlock()
	s.broadcastVoiceStatus()
	return nil
}

// UndeafenVoice включает наушники + восстанавливает прежнее состояние микрофона.
func (s *VoiceService) UndeafenVoice() error {
	s.mu.Lock()
	s.deafened = false
	s.muted = s.mutedBeforeDeafen
	s.mu.Unlock()
	s.broadcastVoiceStatus()
	return nil
}

// IsDeafened возвращает текущее состояние deafen.
func (s *VoiceService) IsDeafened() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deafened
}
