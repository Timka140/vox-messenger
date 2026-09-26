package services

import (
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ChatLinkItem — ссылка из текста сообщения (вкладка «Ссылки» в медиа).
type ChatLinkItem struct {
	MsgID     string `json:"msgId"`
	URL       string `json:"url"`
	Host      string `json:"host"`
	Text      string `json:"text"`      // текст сообщения без самой ссылки, до 140 рун
	Timestamp int64  `json:"timestamp"` // ms
}

// Схема обязательна (или www.): голое «example.com» в тексте — слишком много
// ложных срабатываний на именах файлов и версиях.
var linkRe = regexp.MustCompile(`(?i)\b(?:https?://|www\.)[^\s<>"']+`)

// Хвостовая пунктуация — конец предложения, а не часть адреса.
const linkTrimTail = ".,;:!?)]}«»\"'…"

// splitLink делит совпадение linkRe на адрес и хвостовую пунктуацию.
// «(см. https://x.y/a_(b)).» — закрывающую скобку возвращаем в адрес, пока
// в нём есть непарная открывающая.
func splitLink(m string) (link, tail string) {
	link = strings.TrimRight(m, linkTrimTail)
	tail = m[len(link):]
	for strings.Count(link, "(") > strings.Count(link, ")") && strings.HasPrefix(tail, ")") {
		link += ")"
		tail = tail[1:]
	}
	if strings.HasPrefix(strings.ToLower(link), "www.") {
		link = "https://" + link
	}
	return link, tail
}

// linkHost — домен ссылки без «www.»; "" для не-http(s) и мусора.
func linkHost(link string) string {
	u, err := url.Parse(link)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return strings.TrimPrefix(u.Hostname(), "www.")
}

// extractLinks находит ссылки в тексте: без дублей, www. дополняется https://.
func extractLinks(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range linkRe.FindAllString(text, -1) {
		link, _ := splitLink(m)
		if linkHost(link) == "" || seen[link] {
			continue
		}
		seen[link] = true
		out = append(out, link)
	}
	return out
}

// linkContext — подпись ссылки: текст сообщения, где каждая ссылка заменена
// своим доменом («две ссылки: github.com и archdaily.com»), схлопнутые
// пробелы, до 140 рун. "" — в сообщении, кроме ссылок, ничего нет (фронт
// тогда показывает сам адрес).
func linkContext(text string) string {
	rest := strings.Join(strings.Fields(linkRe.ReplaceAllString(text, " ")), " ")
	if strings.Trim(rest, " -—:·.,;!?()[]«»\"'…") == "" {
		return ""
	}
	s := linkRe.ReplaceAllStringFunc(text, func(m string) string {
		link, tail := splitLink(m)
		if h := linkHost(link); h != "" {
			return h + tail
		}
		return m
	})
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > 140 {
		r := []rune(s)
		s = string(r[:140]) + "…"
	}
	return s
}

// GetChatLinks возвращает ссылки из сообщений чата, новые сверху.
// Ровно один из peerID / groupID. limit — сколько сообщений со ссылками
// просмотреть (по умолчанию и максимум 500).
func (s *MessagingService) GetChatLinks(peerID, groupID string, limit int) ([]ChatLinkItem, error) {
	const op = "MessagingService.GetChatLinks"
	if (peerID == "") == (groupID == "") {
		return nil, fmt.Errorf("%s: exactly one of peerID/groupID required", op)
	}
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	const filter = ` AND deleted = 0 AND type NOT IN ('voice', 'call')
		AND (content LIKE '%://%' OR content LIKE '%www.%')
		ORDER BY created_at DESC LIMIT ?`
	var query string
	var id string
	if peerID != "" {
		query = `SELECT id, content, created_at FROM messages WHERE peer_id = ?` + filter
		id = peerID
	} else {
		query = `SELECT id, content, created_at FROM group_messages WHERE group_id = ?` + filter
		id = groupID
	}
	rows, err := s.store.DB().Query(query, id, limit)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer rows.Close()

	out := []ChatLinkItem{}
	for rows.Next() {
		var msgID, content string
		var createdAt int64
		if err := rows.Scan(&msgID, &content, &createdAt); err != nil {
			s.log.Warn(op+": scan", slog.String("err", err.Error()))
			continue
		}
		links := extractLinks(content)
		if len(links) == 0 {
			continue
		}
		ctx := linkContext(content)
		for _, l := range links {
			host := ""
			if u, err := url.Parse(l); err == nil {
				host = strings.TrimPrefix(u.Hostname(), "www.")
			}
			out = append(out, ChatLinkItem{MsgID: msgID, URL: l, Host: host, Text: ctx, Timestamp: createdAt * 1000})
		}
	}
	return out, rows.Err()
}
