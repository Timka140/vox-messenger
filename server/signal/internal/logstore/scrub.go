package logstore

// Чистка клиентских логов при приёме: клиенты 0080 и старше писали в лог
// тексты сообщений, имена файлов и ники. Правила — копия
// app/internal/logsafe (модули app и signal раздельные), держать в синхроне.

import (
	"bytes"
	"encoding/json"
	"regexp"
)

const redacted = "‹вырезано›"

var blockedKeys = map[string]bool{
	"content": true, "text": true, "body": true, "draft": true, "caption": true,
	"nickname": true, "nick": true, "group_name": true,
	"file_name": true, "filename": true, "file_path": true,
	"title": true, "invite": true,
}

var peerKeys = map[string]bool{
	"peer": true, "from": true, "to": true, "friend": true, "sender": true, "author": true,
}

var (
	shortID  = regexp.MustCompile(`^[0-9a-fA-F]{4,64}(…|\.\.\.)?$`)
	winHome  = regexp.MustCompile(`(?i)\b[a-z]:[\\/]users[\\/][^\\/"\s]+`)
	unixHome = regexp.MustCompile(`(/home|/Users)/[^/"\s]+`)
	oldText  = regexp.MustCompile(`\b(text|caption)=".*$`)
	oldNames = regexp.MustCompile(`^(selectPeer|selectGroup): .*? id=`)
	oldTail  = regexp.MustCompile(`^(nickname saved|friend request sent to):? .*$`)
)

// Старые клиенты писали личное и под общими ключами: название группы в
// "group"/"name", путь к файлу в "path".
var oldClientKeys = map[string]bool{"name": true, "path": true}

func scrubString(s string) string {
	s = winHome.ReplaceAllString(s, "%USERPROFILE%")
	s = unixHome.ReplaceAllString(s, "~")
	s = oldText.ReplaceAllString(s, `$1=`+redacted)
	s = oldNames.ReplaceAllString(s, `$1: `+redacted+` id=`)
	s = oldTail.ReplaceAllString(s, `$1 `+redacted)
	return s
}

func scrubValue(key, v string) string {
	switch {
	case v == "":
		return v
	case blockedKeys[key] || oldClientKeys[key]:
		return redacted
	case peerKeys[key] || key == "group":
		if shortID.MatchString(v) {
			return v
		}
		return redacted
	}
	return scrubString(v)
}

// scrubLog чистит JSONL-лог клиента построчно; строки без личного не меняются.
func scrubLog(data []byte) []byte {
	lines := bytes.Split(data, []byte("\n"))
	for i, l := range lines {
		lines[i] = scrubLine(l)
	}
	return bytes.Join(lines, []byte("\n"))
}

func scrubLine(line []byte) []byte {
	if len(bytes.TrimSpace(line)) == 0 {
		return line
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		if s := scrubString(string(line)); s != string(line) {
			return []byte(s)
		}
		return line
	}
	if !scrubMap(m) {
		return line
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return []byte(`{"msg":"` + redacted + `"}`)
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func scrubMap(m map[string]any) bool {
	changed := false
	for k, v := range m {
		switch x := v.(type) {
		case string:
			if nv := scrubValue(k, x); nv != x {
				m[k] = nv
				changed = true
			}
		case map[string]any:
			if scrubMap(x) {
				changed = true
			}
		case []any:
			if blockedKeys[k] || peerKeys[k] {
				m[k] = redacted
				changed = true
				continue
			}
			for i, e := range x {
				if s, ok := e.(string); ok {
					if ns := scrubString(s); ns != s {
						x[i] = ns
						changed = true
					}
				} else if mm, ok := e.(map[string]any); ok && scrubMap(mm) {
					changed = true
				}
			}
		}
	}
	return changed
}
