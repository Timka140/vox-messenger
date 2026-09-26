package logstore

import (
	"bytes"
	"encoding/base64"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Строки в формате логов клиентов 0080 и старше.
const oldClientLog = `{"time":"t","level":"INFO","msg":"📤 DM ОТПРАВЛЕНО","to":"Alina_Local","peer":"76ba6979dacfcc1a","msg_id":"m1","content":"встречаемся в 8 у метро"}
{"time":"t","level":"INFO","msg":"📥 GROUP ПОЛУЧЕНО","group":"Семья","group_id":"3b71aa00","from":"CHORT","content":"секрет"}
{"time":"t","level":"INFO","msg":"📦 streaming manifest prepared","file":"9c1e0000","name":"паспорт.pdf","bytes":10}
{"time":"t","level":"INFO","msg":"attachment saved","att_id":"a1","path":"C:\\Users\\tim\\Downloads\\паспорт.pdf"}
{"time":"t","level":"INFO","msg":"[SEND] → peer:76ba6979dacfcc1a text=\"привет\"","component":"frontend"}
{"time":"t","level":"INFO","msg":"selectPeer: Alina_Local id=76ba6979dacfcc1a online=true","component":"frontend.ui"}
{"time":"t","level":"INFO","msg":"datachannel open","peer":"91be0000"}`

func TestScrubLogOldClient(t *testing.T) {
	out := string(scrubLog([]byte(oldClientLog)))
	for _, leak := range []string{"Alina", "метро", "Семья", "CHORT", "секрет", "паспорт", `\\tim`, "привет"} {
		if strings.Contains(out, leak) {
			t.Errorf("leak %q in:\n%s", leak, out)
		}
	}
	for _, keep := range []string{"76ba6979dacfcc1a", "3b71aa00", "9c1e0000", `{"time":"t","level":"INFO","msg":"datachannel open","peer":"91be0000"}`} {
		if !strings.Contains(out, keep) {
			t.Errorf("lost %q in:\n%s", keep, out)
		}
	}
	if strings.Count(out, "\n") != strings.Count(oldClientLog, "\n") {
		t.Errorf("line count changed")
	}
}

func TestSaveChunkScrubsAssembled(t *testing.T) {
	dir := t.TempDir()
	st := &Store{baseDir: dir, chunks: map[string]*chunkBuffer{}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	raw := []byte(oldClientLog)
	// режем посередине строки с текстом — как делает клиент по 48 КБ
	cut := bytes.Index(raw, []byte("метро")) + 3
	parts := [][]byte{raw[:cut], raw[cut:]}
	for i, p := range parts {
		st.SaveChunk("Test3b#07cd", i, len(parts), base64.StdEncoding.EncodeToString(p))
	}
	files, _ := os.ReadDir(filepath.Join(dir, "Test3b#07cd"))
	if len(files) != 1 {
		t.Fatalf("files: %d", len(files))
	}
	got, _ := os.ReadFile(filepath.Join(dir, "Test3b#07cd", files[0].Name()))
	if bytes.Contains(got, []byte("метро")) || bytes.Contains(got, []byte("Alina")) {
		t.Fatalf("leak:\n%s", got)
	}
}
