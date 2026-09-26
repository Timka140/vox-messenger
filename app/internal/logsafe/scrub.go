package logsafe

import (
	"bytes"
	"encoding/json"
)

// ScrubFile чистит JSONL-лог построчно. Строки, в которых нечего вырезать,
// остаются байт в байт (порядок полей не меняется).
func ScrubFile(data []byte) []byte {
	lines := bytes.Split(data, []byte("\n"))
	for i, l := range lines {
		lines[i] = ScrubLine(l)
	}
	return bytes.Join(lines, []byte("\n"))
}

// ScrubLine чистит одну строку лога. JSON разбирается и собирается заново
// только если что-то вырезано; не-JSON строка проходит через ScrubString.
func ScrubLine(line []byte) []byte {
	if len(bytes.TrimSpace(line)) == 0 {
		return line
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		s := ScrubString(string(line))
		if s == string(line) {
			return line
		}
		return []byte(s)
	}
	if !scrubMap(m) {
		return line
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return []byte(`{"msg":"` + Redacted + `"}`)
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// scrubMap чистит значения на месте, возвращает true, если что-то изменилось.
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
				m[k] = Redacted
				changed = true
				continue
			}
			for i, e := range x {
				if s, ok := e.(string); ok {
					if ns := ScrubString(s); ns != s {
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
