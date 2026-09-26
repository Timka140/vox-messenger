package services

import (
	"log/slog"
	"sync"

	toast "git.sr.ht/~jackmordaunt/go-toast"
)

// NotifyService — всплывающие уведомления приложения.
//
// Режим задаётся настройкой notify_mode:
//   - "custom" (default) — своё окно notify.html в правом нижнем углу
//     (frameless, поверх всех, не забирает фокус). Go только эмитит
//     vox:toast-show, страница сама держит стек карточек, таймеры и
//     через Resize/Dismissed управляет размером и видимостью окна.
//   - "system" — системный toast Windows (go-toast), как было раньше.
//   - "off" — ничего.
//
// Клик по карточке → OpenTarget → vox:open-chat главному окну + фокус.
type NotifyService struct {
	log        *slog.Logger
	getSetting func(key string) (string, error)

	mu        sync.Mutex
	win       NotifyWindow
	emit      func(name string, data any)
	focusMain func()
}

// NotifyWindowWidth — ширина окна уведомлений (px). Высоту задаёт страница.
const NotifyWindowWidth = 360

// NotifyWindow — окно уведомлений (реализация в main.go поверх Wails).
type NotifyWindow interface {
	// ShowAt показывает окно заданного размера в правом нижнем углу рабочей
	// области основного монитора, не активируя его.
	ShowAt(width, height int)
	Hide()
}

// ToastRequest — одно уведомление. Приходит из фронта (ShowToast) и без
// изменений уходит в окно уведомлений событием vox:toast-show.
type ToastRequest struct {
	Kind    string `json:"kind"`    // message | group | friend | voice | call | info
	Title   string `json:"title"`   // имя собеседника / группы
	Body    string `json:"body"`    // превью
	PeerID  string `json:"peerId"`  // куда вести по клику (DM)
	GroupID string `json:"groupId"` // куда вести по клику (группа)
	Avatar  string `json:"avatar"`  // инициалы
	// /vox-avatar/<id>?v=<hash>, если у собеседника есть фото — вместо букв.
	AvatarURL string `json:"avatarUrl,omitempty"`
	Color     string `json:"color"` // цвет аватара (colorFromId)
	Theme     string `json:"theme"` // light | dark — тема главного окна
}

func NewNotifyService(log *slog.Logger, getSetting func(string) (string, error)) *NotifyService {
	return &NotifyService{log: log, getSetting: getSetting}
}

// SetWindow — окно уведомлений (вызывается из main.go после создания).
func (s *NotifyService) SetWindow(w NotifyWindow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.win = w
}

// SetEmitter — как слать события во все окна (app.Event.Emit).
func (s *NotifyService) SetEmitter(fn func(name string, data any)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emit = fn
}

// SetFocusMain — поднять и сфокусировать главное окно (клик по карточке).
func (s *NotifyService) SetFocusMain(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.focusMain = fn
}

func (s *NotifyService) mode() string {
	if s.getSetting == nil {
		return "custom"
	}
	v, err := s.getSetting("notify_mode")
	if err != nil || v == "" {
		return "custom"
	}
	return v
}

// ShowToast — показать уведомление согласно режиму. Вызывается фронтом.
// Пока открыта полноэкранная игра или презентация (userBusy), карточки не
// показываем: окно уведомлений TOPMOST и ложится поверх игры. Бейджи и звук
// остаются; входящий звонок (ShowIncomingCall) показывается всегда.
func (s *NotifyService) ShowToast(req ToastRequest) error {
	if userBusy() {
		s.log.Debug("toast suppressed: fullscreen app")
		return nil
	}
	switch s.mode() {
	case "off":
		return nil
	case "system":
		return s.pushSystem(req.Title, req.Body)
	}

	s.mu.Lock()
	emit, win := s.emit, s.win
	s.mu.Unlock()
	if emit == nil || win == nil {
		// Окно ещё не создано (ранний старт) — не терять уведомление.
		return s.pushSystem(req.Title, req.Body)
	}
	emit("vox:toast-show", req)
	return nil
}

// Resize — страница уведомлений пересчитала высоту стека: показать окно с
// этим размером (или обновить, если уже видно).
func (s *NotifyService) Resize(height int) error {
	s.mu.Lock()
	win := s.win
	s.mu.Unlock()
	if win == nil {
		return nil
	}
	if height < 40 {
		height = 40
	}
	if height > 600 {
		height = 600
	}
	win.ShowAt(NotifyWindowWidth, height)
	return nil
}

// Dismissed — стек пуст, окно можно спрятать.
func (s *NotifyService) Dismissed() error {
	s.mu.Lock()
	win := s.win
	s.mu.Unlock()
	if win != nil {
		win.Hide()
	}
	return nil
}

// OpenTarget — клик по карточке: открыть чат в главном окне и поднять его.
func (s *NotifyService) OpenTarget(peerID, groupID string) error {
	s.mu.Lock()
	emit, focus := s.emit, s.focusMain
	s.mu.Unlock()
	if emit != nil {
		emit("vox:open-chat", map[string]string{"peerId": peerID, "groupId": groupID})
	}
	if focus != nil {
		focus()
	}
	return nil
}

// IncomingCallInfo — карточка входящего звонка. Для приглашения в групповой
// звонок — группа и число участников («CHORT начал звонок · «Группа» · 3
// участника», макет Notifications).
type IncomingCallInfo struct {
	CallID       string
	Nickname     string
	PeerID       string
	Type         string // dm | group
	Busy         bool
	GroupID      string
	GroupName    string
	Participants int
}

// ShowIncomingCall — карточка входящего звонка в окне уведомлений (режим
// custom). false — режим system/off или окно ещё не готово: вызывающий
// покажет старое отдельное окно incoming-call.html.
func (s *NotifyService) ShowIncomingCall(info IncomingCallInfo) bool {
	if s.mode() != "custom" {
		return false
	}
	s.mu.Lock()
	emit, win := s.emit, s.win
	s.mu.Unlock()
	if emit == nil || win == nil {
		return false
	}
	theme := "dark"
	if s.getSetting != nil {
		if v, err := s.getSetting("ui_theme"); err == nil && v != "" {
			theme = v
		}
	}
	emit("vox:toast-call", map[string]any{
		"callId":       info.CallID,
		"nickname":     info.Nickname,
		"peerId":       info.PeerID,
		"type":         info.Type,
		"busy":         info.Busy,
		"groupId":      info.GroupID,
		"groupName":    info.GroupName,
		"participants": info.Participants,
		"theme":        theme,
	})
	return true
}

// LaterCall — «Позже» на карточке приглашения в групповой звонок: карточка
// уходит, рингтон в главном окне смолкает (оба слушают vox:toast-call-end).
// Приглашение остаётся в CallsService — в звонок можно зайти из группы.
func (s *NotifyService) LaterCall(callID string) error {
	s.mu.Lock()
	emit := s.emit
	s.mu.Unlock()
	if emit != nil {
		emit("vox:toast-call-end", map[string]any{"callId": callID})
	}
	return nil
}

// pushSystem — системный toast Windows (fallback и режим "system").
func (s *NotifyService) pushSystem(title, body string) error {
	n := toast.Notification{
		AppID: "Vox.Messenger",
		Title: sanitizeForXML(title),
		Body:  sanitizeForXML(body),
	}
	if err := n.Push(); err != nil {
		s.log.Error("system toast failed",
			slog.String("error", err.Error()))
		return err
	}
	return nil
}
