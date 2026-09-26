package logsafe

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func logTo(buf *bytes.Buffer) *slog.Logger {
	return slog.New(Wrap(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("bad json %q: %v", b, err)
	}
	return m
}

func TestHandlerRedactsByKey(t *testing.T) {
	var buf bytes.Buffer
	logTo(&buf).Info("📤 DM ОТПРАВЛЕНО",
		slog.String("content", "привет, это секрет"),
		slog.String("to", "Alina_Local"),
		slog.String("peer", "76ba6979dacfcc1a"),
		slog.String("from", "4fe55da8…"),
		slog.String("file_name", "секрет.pdf"),
		slog.String("code", "message_too_large"),
		slog.String("path", `C:\Users\tim\Downloads\x.bin`),
		slog.Any("error", errors.New(`open C:\Users\tim\a.txt: denied`)),
		slog.Int("len", 18))
	m := decode(t, buf.Bytes())
	want := map[string]any{
		"content":   Redacted,
		"to":        Redacted,
		"peer":      "76ba6979dacfcc1a",
		"from":      "4fe55da8…",
		"file_name": Redacted,
		"code":      "message_too_large",
		"path":      `%USERPROFILE%\Downloads\x.bin`,
		"error":     `open %USERPROFILE%\a.txt: denied`,
		"len":       float64(18),
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
}

func TestHandlerWithAttrsAndGroups(t *testing.T) {
	var buf bytes.Buffer
	l := logTo(&buf).With(slog.String("nickname", "Timka")).WithGroup("g")
	l.Info("x", slog.Group("inner", slog.String("text", "секрет"), slog.String("id", "abc")))
	s := buf.String()
	if strings.Contains(s, "Timka") || strings.Contains(s, "секрет") {
		t.Fatalf("leak: %s", s)
	}
	if !strings.Contains(s, `"id":"abc"`) {
		t.Fatalf("lost id: %s", s)
	}
}

func TestScrubStringOldFrontend(t *testing.T) {
	cases := map[string]string{
		`[SEND] → peer:76ba6979dacfcc1a text="привет "мир""`:      `[SEND] → peer:76ba6979dacfcc1a text=` + Redacted,
		`📥 ATTACH MSG: id=1 from=ab atts=2 caption="фото с дачи"`: `📥 ATTACH MSG: id=1 from=ab atts=2 caption=` + Redacted,
		`selectPeer: Alina_Local id=76ba6979dacfcc1a online=true`: `selectPeer: ` + Redacted + ` id=76ba6979dacfcc1a online=true`,
		`selectGroup: Семья id=3b71aa00 members=4`:                `selectGroup: ` + Redacted + ` id=3b71aa00 members=4`,
		`nickname saved: Timka`:        `nickname saved ` + Redacted,
		`friend request sent to Alina`: `friend request sent to ` + Redacted,
		`loaded /home/tim/.config/vox/x and C:/Users/Тим/AppData/Local/vox`: `loaded ~/.config/vox/x and %USERPROFILE%/AppData/Local/vox`,
		`ice restart: impolite, peer=91be`:                                  `ice restart: impolite, peer=91be`,
	}
	for in, want := range cases {
		if got := ScrubString(in); got != want {
			t.Errorf("ScrubString(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestScrubLine(t *testing.T) {
	clean := []byte(`{"time":"2026-09-26T10:00:00Z","level":"INFO","msg":"datachannel open","peer":"91be0000"}`)
	if got := ScrubLine(clean); !bytes.Equal(got, clean) {
		t.Fatalf("clean line changed: %s", got)
	}
	dirty := []byte(`{"time":"t","level":"INFO","msg":"📤 DM <ok>","to":"Alina","content":"секрет","attrs":{"name":"x","friend":"CHORT"}}`)
	got := ScrubLine(dirty)
	if bytes.Contains(got, []byte("секрет")) || bytes.Contains(got, []byte("Alina")) || bytes.Contains(got, []byte("CHORT")) {
		t.Fatalf("leak: %s", got)
	}
	if !bytes.Contains(got, []byte("<ok>")) {
		t.Fatalf("html escaped or msg lost: %s", got)
	}
	notJSON := []byte(`panic: open C:\Users\tim\x`)
	if got := ScrubLine(notJSON); string(got) != `panic: open %USERPROFILE%\x` {
		t.Fatalf("not json: %s", got)
	}
}

func TestScrubFileKeepsLayout(t *testing.T) {
	in := []byte("{\"msg\":\"a\"}\n{\"msg\":\"b\",\"text\":\"секрет\"}\n")
	out := ScrubFile(in)
	if bytes.Contains(out, []byte("секрет")) {
		t.Fatalf("leak: %s", out)
	}
	if bytes.Count(out, []byte("\n")) != 2 || !bytes.HasPrefix(out, []byte("{\"msg\":\"a\"}\n")) {
		t.Fatalf("layout changed: %q", out)
	}
}
