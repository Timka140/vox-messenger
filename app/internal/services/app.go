package services

import (
	"log/slog"
	"strings"

	toast "git.sr.ht/~jackmordaunt/go-toast"
)

type AppService struct {
	log          *slog.Logger
	recoveryMode bool

	// «Диагностика» (app_logs.go): уровень логов на ходу и путь к логу.
	logLevel *slog.LevelVar
	logBase  slog.Level
	logPath  string
}

func NewAppService(log *slog.Logger) *AppService {
	return &AppService{log: log}
}

// SetRecoveryMode устанавливает флаг восстановления (вызывается из main.go)
func (s *AppService) SetRecoveryMode(v bool) {
	s.recoveryMode = v
}

// IsRecoveryMode возвращает true если БД была пересоздана из-за утери ключа
func (s *AppService) IsRecoveryMode() bool {
	return s.recoveryMode
}

// sanitizeForXML удаляет символы вне BMP (emoji и пр.), которые ломают Windows XML-парсер toast notifications.
func sanitizeForXML(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r <= 0xFFFF && r != 0xFFFE && r != 0xFFFF {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (s *AppService) Notify(title, message string) error {
	n := toast.Notification{
		AppID: "Vox.Messenger",
		Title: sanitizeForXML(title),
		Body:  sanitizeForXML(message),
	}
	if err := n.Push(); err != nil {
		s.log.Error("toast notification failed",
			slog.String("error", err.Error()))
		return err
	}
	return nil
}

func (s *AppService) LogFrontend(level, component, msg string) {
	switch level {
	case "ERR":
		s.log.Error(msg, slog.String("component", component))
	case "WRN":
		s.log.Warn(msg, slog.String("component", component))
	case "DBG":
		s.log.Debug(msg, slog.String("component", component))
	default:
		s.log.Info(msg, slog.String("component", component))
	}
}
