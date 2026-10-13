// Package logsafe не пускает личное в лог: тексты сообщений, имена и пути
// файлов, ники, названия групп.
//
// Главное — не писать личное в местах вызова (короткие id вместо ников,
// длина вместо текста). Этот пакет — страховка поверх:
//   - Wrap — обёртка slog.Handler, вычищает запись перед любым обработчиком;
//   - ScrubFile — то же для готового JSONL-лога перед отправкой на сервер
//     (закрывает файлы, записанные старыми версиями).
//
// Такие же правила продублированы на сервере: server/signal/internal/logstore/scrub.go.
package logsafe

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"strings"
)

// Redacted — метка вместо вырезанного значения.
const Redacted = "‹вырезано›"

// blockedKeys — значения этих ключей не пишутся никогда.
var blockedKeys = map[string]bool{
	"content": true, "text": true, "body": true, "draft": true, "caption": true,
	"nickname": true, "nick": true, "nick_tag": true, "group_name": true,
	"file_name": true, "filename": true, "file_path": true,
	"title": true, "invite": true,
}

// peerKeys — ключи собеседника: допустим только короткий id, ник вырезается.
var peerKeys = map[string]bool{
	"peer": true, "from": true, "to": true, "friend": true, "sender": true, "author": true,
}

// shortID — hex-id (обрезанный pubkey, sha), как у safeShort/safeShort8.
var shortID = regexp.MustCompile(`^[0-9a-fA-F]{4,64}(…|\.\.\.)?$`)

var (
	winHome  = regexp.MustCompile(`(?i)\b[a-z]:[\\/]users[\\/][^\\/"\s]+`)
	unixHome = regexp.MustCompile(`(/home|/Users)/[^/"\s]+`)
	// Строки фронта старых версий: текст сообщения/подписи в конце строки,
	// ники и названия групп в известных местах.
	oldText  = regexp.MustCompile(`\b(text|caption)=".*$`)
	oldNames = regexp.MustCompile(`^(selectPeer|selectGroup): .*? id=`)
	oldTail  = regexp.MustCompile(`^(nickname saved|friend request sent to):? .*$`)
)

// Peer — короткий id собеседника для лога вместо ника.
func Peer(id string) string {
	if len(id) > 16 {
		return id[:16]
	}
	return id
}

// Len — длина текста вместо самого текста.
func Len(key, s string) slog.Attr {
	return slog.Int(key, len([]rune(s)))
}

// Err — текст ошибки без путей к файлам пользователя (*fs.PathError,
// *os.LinkError кладут полный путь в Error()).
func Err(key string, err error) slog.Attr {
	if err == nil {
		return slog.String(key, "")
	}
	return slog.String(key, errText(err))
}

func errText(err error) string {
	s := err.Error()
	var pe *fs.PathError
	if errors.As(err, &pe) && pe.Path != "" {
		s = strings.ReplaceAll(s, pe.Path, "‹путь›")
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		for _, p := range []string{le.Old, le.New} {
			if p != "" {
				s = strings.ReplaceAll(s, p, "‹путь›")
			}
		}
	}
	return ScrubString(s)
}

// ScrubString убирает из свободного текста домашние каталоги и личное
// из известных строк старых версий.
func ScrubString(s string) string {
	s = winHome.ReplaceAllString(s, "%USERPROFILE%")
	s = unixHome.ReplaceAllString(s, "~")
	s = oldText.ReplaceAllString(s, `$1=`+Redacted)
	s = oldNames.ReplaceAllString(s, `$1: `+Redacted+` id=`)
	s = oldTail.ReplaceAllString(s, `$1 `+Redacted)
	return s
}

// scrubValue чистит строковое значение с учётом ключа.
func scrubValue(key string, v string) string {
	switch {
	case blockedKeys[key]:
		if v == "" {
			return v
		}
		return Redacted
	case peerKeys[key]:
		if v == "" || shortID.MatchString(v) {
			return v
		}
		return Redacted
	}
	return ScrubString(v)
}

func scrubAttr(a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve()
	switch a.Value.Kind() {
	case slog.KindGroup:
		as := a.Value.Group()
		out := make([]slog.Attr, len(as))
		for i, x := range as {
			out[i] = scrubAttr(x)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	case slog.KindString:
		return slog.String(a.Key, scrubValue(a.Key, a.Value.String()))
	case slog.KindAny:
		if blockedKeys[a.Key] || peerKeys[a.Key] {
			return slog.String(a.Key, Redacted)
		}
		if err, ok := a.Value.Any().(error); ok {
			return slog.String(a.Key, errText(err))
		}
	}
	return a
}

type handler struct {
	inner slog.Handler
}

// Wrap возвращает обработчик, который чистит каждую запись перед inner.
func Wrap(inner slog.Handler) slog.Handler {
	return &handler{inner: inner}
}

func (h *handler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, ScrubString(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(scrubAttr(a))
		return true
	})
	return h.inner.Handle(ctx, nr)
}

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(as))
	for i, a := range as {
		out[i] = scrubAttr(a)
	}
	return &handler{inner: h.inner.WithAttrs(out)}
}

func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{inner: h.inner.WithGroup(name)}
}
