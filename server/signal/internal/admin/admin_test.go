package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"messenger_signal/internal/logstore"
)

const testToken = "0123456789abcdef0123456789abcdef-test"

var testNow = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func newTestAPI(t *testing.T, opt Options) http.Handler {
	t.Helper()
	opt.Token = testToken
	opt.Now = func() time.Time { return testNow }
	if opt.Started.IsZero() {
		opt.Started = testNow.Add(-time.Hour)
	}
	opt.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(opt).Handler()
}

// req — подписанный запрос с X-NW-Enc: 1; ts — смещение от testNow.
// Зашифрованный ответ расшифровывается: в w.Body — открытый текст, в
// Content-Type — исходный тип из X-NW-Type.
func req(h http.Handler, method, uri string, body []byte, skew time.Duration) *httptest.ResponseRecorder {
	ts := strconv.FormatInt(testNow.Add(skew).UnixMilli(), 10)
	r := httptest.NewRequest(method, uri, bytes.NewReader(body))
	r.Header.Set("X-NW-Ts", ts)
	sig := Sign(testToken, method, r.URL.RequestURI(), ts, body)
	r.Header.Set("X-NW-Sig", sig)
	r.Header.Set("X-NW-Enc", "1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	openRecorder(w, sig)
	return w
}

// openRecorder расшифровывает тело ответа на месте (если оно зашифровано).
// Ошибка расшифровки — тело "DECRYPT FAILED: …", код 599: тест упадёт на коде.
func openRecorder(w *httptest.ResponseRecorder, sig string) {
	if w.Header().Get("X-NW-Enc") != "1" {
		return
	}
	key, _ := responseKey([]byte(testToken))
	o, err := newFrameOpener(bytes.NewReader(w.Body.Bytes()), key, sig)
	var plain []byte
	if err == nil {
		plain, err = io.ReadAll(o)
	}
	if err != nil {
		w.Code = 599
		plain = []byte("DECRYPT FAILED: " + err.Error())
	}
	w.Body = bytes.NewBuffer(plain)
	w.Header().Set("Content-Type", w.Header().Get("X-NW-Type"))
}

func TestAuth(t *testing.T) {
	h := newTestAPI(t, Options{NodeID: "abc", Clients: func() int { return 3 }})
	sign := func(method, uri, ts string, body []byte) string {
		return Sign(testToken, method, uri, ts, body)
	}
	ts := strconv.FormatInt(testNow.UnixMilli(), 10)
	cases := []struct {
		name   string
		mutate func(r *http.Request)
		want   int
	}{
		{"good", func(r *http.Request) {}, 200},
		{"no headers", func(r *http.Request) { r.Header.Del("X-NW-Ts"); r.Header.Del("X-NW-Sig") }, 401},
		{"no ts", func(r *http.Request) { r.Header.Del("X-NW-Ts") }, 401},
		{"no sig", func(r *http.Request) { r.Header.Del("X-NW-Sig") }, 401},
		{"bad sig", func(r *http.Request) { r.Header.Set("X-NW-Sig", sign("GET", "/admin/v1/status", ts, []byte("x"))) }, 401},
		{"non-hex sig", func(r *http.Request) { r.Header.Set("X-NW-Sig", "zz") }, 401},
		{"wrong token", func(r *http.Request) {
			r.Header.Set("X-NW-Sig", Sign(strings.Repeat("x", 40), "GET", "/admin/v1/status", ts, nil))
		}, 401},
		{"skew past", func(r *http.Request) {
			old := strconv.FormatInt(testNow.Add(-61*time.Second).UnixMilli(), 10)
			r.Header.Set("X-NW-Ts", old)
			r.Header.Set("X-NW-Sig", sign("GET", "/admin/v1/status", old, nil))
		}, 401},
		{"skew future", func(r *http.Request) {
			fut := strconv.FormatInt(testNow.Add(61*time.Second).UnixMilli(), 10)
			r.Header.Set("X-NW-Ts", fut)
			r.Header.Set("X-NW-Sig", sign("GET", "/admin/v1/status", fut, nil))
		}, 401},
		{"skew within", func(r *http.Request) {
			ok := strconv.FormatInt(testNow.Add(-59*time.Second).UnixMilli(), 10)
			r.Header.Set("X-NW-Ts", ok)
			r.Header.Set("X-NW-Sig", sign("GET", "/admin/v1/status", ok, nil))
		}, 200},
		{"query tampered", func(r *http.Request) { r.URL.RawQuery = "x=1"; r.RequestURI = "/admin/v1/status?x=1" }, 401},
		{"no enc header", func(r *http.Request) { r.Header.Del("X-NW-Enc") }, 400},
		{"enc header not 1", func(r *http.Request) { r.Header.Set("X-NW-Enc", "0") }, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/admin/v1/status", nil)
			r.Header.Set("X-NW-Ts", ts)
			r.Header.Set("X-NW-Sig", sign("GET", "/admin/v1/status", ts, nil))
			r.Header.Set("X-NW-Enc", "1")
			c.mutate(r)
			// свежий API на каждый случай — без влияния кеша повторов
			h := newTestAPI(t, Options{NodeID: "abc"})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != c.want {
				t.Fatalf("code %d, want %d: %s", w.Code, c.want, w.Body)
			}
			if c.want == 200 {
				// после проверки подписи — только шифрованный поток
				if w.Header().Get("X-NW-Enc") != "1" || w.Header().Get("Content-Type") != "application/octet-stream" ||
					w.Header().Get("X-NW-Type") != "application/json" || w.Header().Get("Content-Length") != "" {
					t.Fatalf("headers %v", w.Header())
				}
				if bytes.Contains(w.Body.Bytes(), []byte("abc")) {
					t.Fatal("plaintext in encrypted body")
				}
				openRecorder(w, r.Header.Get("X-NW-Sig"))
				if w.Code != 200 || !strings.Contains(w.Body.String(), `"node_id":"abc"`) {
					t.Fatalf("decrypted %d %s", w.Code, w.Body)
				}
				return
			}
			// ошибки до проверки подписи — открытый JSON
			if ct := w.Header().Get("Content-Type"); ct != "application/json" || w.Header().Get("X-NW-Enc") != "" {
				t.Fatalf("content-type %q enc %q", ct, w.Header().Get("X-NW-Enc"))
			}
		})
	}

	t.Run("replay", func(t *testing.T) {
		r1 := httptest.NewRequest("GET", "/admin/v1/status", nil)
		r1.Header.Set("X-NW-Ts", ts)
		r1.Header.Set("X-NW-Sig", sign("GET", "/admin/v1/status", ts, nil))
		r1.Header.Set("X-NW-Enc", "1")
		r2 := r1.Clone(r1.Context())
		w1, w2 := httptest.NewRecorder(), httptest.NewRecorder()
		h.ServeHTTP(w1, r1)
		h.ServeHTTP(w2, r2)
		if w1.Code != 200 || w2.Code != 401 {
			t.Fatalf("first %d, second %d", w1.Code, w2.Code)
		}
	})

	// Узел перезапущен: кеш повторов пуст, подписи до старта не принимаются.
	t.Run("signed before restart", func(t *testing.T) {
		hr := newTestAPI(t, Options{Started: testNow.Add(-10 * time.Second)})
		if w := req(hr, "GET", "/admin/v1/status", nil, -20*time.Second); w.Code != 401 {
			t.Fatalf("до старта: code %d", w.Code)
		}
		if w := req(hr, "GET", "/admin/v1/status", nil, -5*time.Second); w.Code != 200 {
			t.Fatalf("после старта: code %d", w.Code)
		}
	})

	t.Run("body too large", func(t *testing.T) {
		w := req(h, "PUT", "/admin/v1/releases", bytes.Repeat([]byte("a"), maxBody+1), 0)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("code %d", w.Code)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		w := httptest.NewRecorder()
		Disabled().ServeHTTP(w, httptest.NewRequest("GET", "/admin/v1/status", nil))
		if w.Code != 404 {
			t.Fatalf("code %d", w.Code)
		}
	})
}

func TestStatus(t *testing.T) {
	h := newTestAPI(t, Options{
		NodeID:  "abc",
		Clients: func() int { return 7 },
		Cluster: func() ClusterInfo {
			return ClusterInfo{Enabled: true, Self: "ws://n1/ws", Peers: []ClusterPeer{{Addr: "ws://n2/ws", ID: "ff", Connected: true}}}
		},
		Groups: func() (int, bool) { return 2, true },
	})
	w := req(h, "GET", "/admin/v1/status", nil, 0)
	if w.Code != 200 {
		t.Fatalf("code %d: %s", w.Code, w.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["node_id"] != "abc" || got["clients_online"] != 7.0 || got["groups"] != 2.0 {
		t.Fatalf("status %v", got)
	}
	if _, ok := got["releases"]; ok {
		t.Fatal("releases без хранилища должен опускаться")
	}
	if _, ok := got["log_sources"]; ok {
		t.Fatal("log_sources без хранилища должен опускаться")
	}
	if c, _ := got["cluster"].(map[string]any); c["enabled"] != true || len(c["peers"].([]any)) != 1 {
		t.Fatalf("cluster %v", got["cluster"])
	}
	if _, err := time.Parse(time.RFC3339, got["started_at"].(string)); err != nil {
		t.Fatal(err)
	}
}

// --- логи ---

const testSource = "Nick#tag"

var testLog = strings.Join([]string{
	`{"time":"2026-09-26T11:00:00Z","level":"DEBUG","msg":"dbg one","comp":"network"}`,
	`{"time":"2026-09-26T11:00:10Z","level":"INFO","msg":"hello World","comp":"network","peer":"abc"}`,
	`{"time":"2026-09-26T11:01:00Z","level":"WARN","msg":"slow","component":"voice","ms":150}`,
	`not json at all`,
	`{"time":"2026-09-26T11:02:00Z","level":"ERROR","msg":"boom failed","comp":"voice"}`,
	``,
	`{"time":"2026-09-26T11:59:00Z","level":"INFO","msg":"bye world","comp":"network","version":"v0.00.000.0082","os":"windows"}`,
}, "\r\n") + "\n"

func newLogsAPI(t *testing.T) (http.Handler, string) {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, testSource)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dir, "2026-09-26T11-00-00.jsonl"), []byte(testLog), 0644))
	must(os.WriteFile(filepath.Join(dir, "2026-09-25T10-00-00.jsonl"), []byte(`{"time":"2026-09-25T10:00:00Z","level":"INFO","msg":"old"}`+"\n"), 0644))
	must(os.WriteFile(filepath.Join(base, "secret.txt"), []byte("SECRET"), 0644))
	must(os.WriteFile(filepath.Join(dir, "..hidden"), []byte("SECRET"), 0644))
	st := logstore.New(base, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return newTestAPI(t, Options{Logs: st}), base
}

func logsURL(params string) string {
	u := "/admin/v1/logs/Nick%23tag"
	if params != "" {
		u += "?" + params
	}
	return u
}

func TestLogsFilter(t *testing.T) {
	h, _ := newLogsAPI(t)
	cases := []struct {
		name    string
		params  string
		total   int
		msgs    []string // msg (или text для RAW) строк страницы
		page    int
		pages   int
		errLine []int
	}{
		{"default no debug", "", 5, []string{"hello World", "slow", "not json at all", "boom failed", "bye world"}, 0, 1, []int{3}},
		{"levels debug", "levels=DEBUG", 1, []string{"dbg one"}, 0, 1, nil},
		{"levels lower", "levels=warn,error", 2, []string{"slow", "boom failed"}, 0, 1, []int{1}},
		{"levels raw", "levels=RAW", 1, []string{"not json at all"}, 0, 1, nil},
		{"q case-insensitive", "q=WORLD", 2, []string{"hello World", "bye world"}, 0, 1, nil},
		{"q case-sensitive", "q=World&case=1", 1, []string{"hello World"}, 0, 1, nil},
		{"q attrs", "q=peer%3Dabc", 1, []string{"hello World"}, 0, 1, nil},
		{"regex", "regex=1&q=%5Eb(oom%7Cye)", 2, []string{"boom failed", "bye world"}, 0, 1, []int{0}},
		{"regex case", "regex=1&case=1&q=%5EHello", 0, nil, 0, 0, nil},
		{"comp", "comp=voice", 2, []string{"slow", "boom failed"}, 0, 1, []int{1}},
		{"from", "from=2026-09-26T11:01:00Z", 4, []string{"slow", "not json at all", "boom failed", "bye world"}, 0, 1, []int{2}},
		{"to", "to=2026-09-26T11:01:30Z", 3, []string{"hello World", "slow", "not json at all"}, 0, 1, nil},
		{"paging", "size=2&page=1", 5, []string{"not json at all", "boom failed"}, 1, 3, []int{3}},
		{"page past end", "size=2&page=9", 5, nil, 9, 3, []int{3}},
		{"at", "size=2&at=2026-09-26T11:59:30Z", 5, []string{"bye world"}, 2, 3, []int{3}},
		{"at minute", "size=2&at=2026-09-26T11:02:45Z", 5, []string{"not json at all", "boom failed"}, 1, 3, []int{3}},
		{"old file", "file=2026-09-25T10-00-00.jsonl", 1, []string{"old"}, 0, 1, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := req(h, "GET", logsURL(c.params), nil, 0)
			if w.Code != 200 {
				t.Fatalf("code %d: %s", w.Code, w.Body)
			}
			var resp logsResp
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Total != c.total || resp.Page != c.page || resp.Pages != c.pages {
				t.Fatalf("total/page/pages = %d/%d/%d, want %d/%d/%d", resp.Total, resp.Page, resp.Pages, c.total, c.page, c.pages)
			}
			var msgs []string
			for _, l := range resp.Lines {
				if l.Level == levelRaw {
					msgs = append(msgs, l.Text)
				} else {
					msgs = append(msgs, l.Msg)
				}
			}
			if strings.Join(msgs, "|") != strings.Join(c.msgs, "|") {
				t.Fatalf("lines %q, want %q", msgs, c.msgs)
			}
			var errLines []int
			for _, e := range resp.Errors {
				errLines = append(errLines, e.Line)
			}
			if len(errLines) != len(c.errLine) || (len(errLines) > 0 && errLines[0] != c.errLine[0]) {
				t.Fatalf("errors %v, want %v", errLines, c.errLine)
			}
			if resp.Source != testSource || len(resp.Files) != 2 {
				t.Fatalf("source %q files %v", resp.Source, resp.Files)
			}
		})
	}
}

func TestLogsDetails(t *testing.T) {
	h, _ := newLogsAPI(t)
	w := req(h, "GET", logsURL(""), nil, 0)
	var resp logsResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.File != "2026-09-26T11-00-00.jsonl" || resp.Files[0] != resp.File {
		t.Fatalf("file %q files %v", resp.File, resp.Files)
	}
	if len(resp.Histogram) != histBuckets {
		t.Fatalf("histogram len %d", len(resp.Histogram))
	}
	n, errs := 0, 0
	for _, b := range resp.Histogram {
		n += b.N
		errs += b.Err
	}
	if n != 5 || errs != 1 {
		t.Fatalf("histogram n=%d err=%d", n, errs)
	}
	// Счётчики уровней не зависят от фильтра уровней: DEBUG виден.
	want := map[string]int{"DEBUG": 1, "INFO": 2, "WARN": 1, "ERROR": 1, "RAW": 1}
	for k, v := range want {
		if resp.Levels[k] != v {
			t.Fatalf("levels %v", resp.Levels)
		}
	}
	if resp.Components["network"] != 2 || resp.Components["voice"] != 2 {
		t.Fatalf("components %v", resp.Components)
	}
	// Строки: attrs, comp из component, время у RAW наследуется.
	l := resp.Lines[0]
	if l.Line != 0 || l.Comp != "network" || l.Attrs["peer"] != "abc" || l.T != "2026-09-26T11:00:10Z" {
		t.Fatalf("line0 %+v", l)
	}
	if resp.Lines[1].Comp != "voice" || resp.Lines[1].Attrs["ms"] != 150.0 {
		t.Fatalf("line1 %+v", resp.Lines[1])
	}
	if raw := resp.Lines[2]; raw.Level != "RAW" || raw.Text != "not json at all" || raw.T != "2026-09-26T11:01:00Z" {
		t.Fatalf("raw %+v", raw)
	}

	// Гистограмма без учёта q: q сужает lines, но не корзины.
	w = req(h, "GET", logsURL("q=boom"), nil, 0)
	resp = logsResp{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	n = 0
	for _, b := range resp.Histogram {
		n += b.N
	}
	if resp.Total != 1 || n != 5 || len(resp.Histogram) != histBuckets {
		t.Fatalf("q: total %d hist n %d", resp.Total, n)
	}

	for _, bad := range []string{"regex=1&q=(", "q=" + strings.Repeat("a", maxQuery+1), "from=yesterday", "size=0", "page=-1"} {
		if w := req(h, "GET", logsURL(bad), nil, 0); w.Code != 400 {
			t.Fatalf("%s: code %d", bad, w.Code)
		}
	}
}

func TestLogsDownload(t *testing.T) {
	h, _ := newLogsAPI(t)
	w := req(h, "GET", "/admin/v1/logs/Nick%23tag/download?q=world&page=1&size=1", nil, 0)
	if w.Code != 200 {
		t.Fatalf("code %d: %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("content-type %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Fatalf("disposition %q", cd)
	}
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "hello World") || !strings.Contains(lines[1], "bye world") {
		t.Fatalf("body %q", w.Body.String())
	}
}

func TestLogSources(t *testing.T) {
	h, _ := newLogsAPI(t)
	w := req(h, "GET", "/admin/v1/logs/sources", nil, 0)
	if w.Code != 200 {
		t.Fatalf("code %d", w.Code)
	}
	var resp struct {
		Sources []sourceResp `json:"sources"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Sources) != 1 {
		t.Fatalf("sources %+v", resp.Sources)
	}
	s := resp.Sources[0]
	if s.Name != testSource || len(s.Files) != 2 || s.Files[0].Name != "2026-09-26T11-00-00.jsonl" {
		t.Fatalf("source %+v", s)
	}
	if s.Lines != 6 || s.Errors != 1 || s.Warns != 1 || s.Version != "v0.00.000.0082" || s.OS != "windows" || s.LastSeen == "" {
		t.Fatalf("stats %+v", s)
	}
}

func TestLogsTraversal(t *testing.T) {
	h, _ := newLogsAPI(t)
	uris := []string{
		"/admin/v1/logs/..",
		"/admin/v1/logs/%2E%2E",
		"/admin/v1/logs/..%2F..",
		"/admin/v1/logs/..%5C..",
		"/admin/v1/logs/secret.txt",
		"/admin/v1/logs/nope",
		"/admin/v1/logs/Nick%23tag?file=..%2Fsecret.txt",
		"/admin/v1/logs/Nick%23tag?file=..%5C..%5Csecret.txt",
		"/admin/v1/logs/Nick%23tag?file=..hidden",
		"/admin/v1/logs/Nick%23tag?file=%2Fetc%2Fpasswd",
		"/admin/v1/logs/Nick%23tag?file=.",
		"/admin/v1/logs/Nick%23tag/download?file=..%2Fsecret.txt",
		"/admin/v1/logs/..%2F../download",
	}
	for _, u := range uris {
		w := req(h, "GET", u, nil, 0)
		if w.Code == 200 || strings.Contains(w.Body.String(), "SECRET") {
			t.Fatalf("%s: code %d body %q", u, w.Code, w.Body.String())
		}
	}
}

func TestHistogramBounds(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	pts := []histPoint{{t: from.UnixMilli()}, {t: to.UnixMilli(), err: true}}
	h := histogram(pts, from, to)
	if len(h) != histBuckets || h[0].N != 1 || h[histBuckets-1].N != 1 || h[histBuckets-1].Err != 1 {
		t.Fatalf("hist %+v", h)
	}
	if got := histogram(nil, time.Time{}, time.Time{}); len(got) != 0 {
		t.Fatalf("empty hist %d", len(got))
	}
	if got := histogram([]histPoint{{t: 5}}, time.Time{}, time.Time{}); len(got) != histBuckets || got[0].N != 1 {
		t.Fatalf("single point %+v", got[0])
	}
}

func TestLineReaderLongLine(t *testing.T) {
	long := strings.Repeat("x", maxLine+500)
	lr := newLineReader(strings.NewReader(long + "\nnext\nlast"))
	l1, off1, err := lr.next()
	if err != nil || len(l1) != maxLine || off1 != 0 {
		t.Fatalf("l1 len %d err %v", len(l1), err)
	}
	l2, off2, _ := lr.next()
	if string(l2) != "next" || off2 != int64(len(long)+1) {
		t.Fatalf("l2 %q off %d", l2, off2)
	}
	l3, _, _ := lr.next()
	if string(l3) != "last" {
		t.Fatalf("l3 %q", l3)
	}
	if _, _, err := lr.next(); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}
