package admin

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"messenger_signal/internal/logstore"
)

const (
	maxLine      = 1 << 20  // длиннее — строка обрезается
	maxScan      = 64 << 20 // потолок прочитанного за запрос
	maxQuery     = 200      // длина q (и regex)
	defaultSize  = 200
	maxSize      = 1000
	maxErrors    = 1000
	histBuckets  = 60
	levelRaw     = "RAW"
	levelDebug   = "DEBUG"
	levelError   = "ERROR"
	readerBufLen = 64 << 10
)

// --- чтение строк ---

// lineReader — построчное чтение с учётом смещений; строки длиннее maxLine
// обрезаются, остаток до \n пропускается.
type lineReader struct {
	r   *bufio.Reader
	off int64
	buf []byte
}

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{r: bufio.NewReaderSize(r, readerBufLen)}
}

// next — очередная строка без \r\n и смещение её начала. Буфер переиспользуется.
func (lr *lineReader) next() ([]byte, int64, error) {
	start := lr.off
	lr.buf = lr.buf[:0]
	for {
		chunk, err := lr.r.ReadSlice('\n')
		lr.off += int64(len(chunk))
		if room := maxLine - len(lr.buf); room > 0 {
			lr.buf = append(lr.buf, chunk[:min(len(chunk), room)]...)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if lr.off == start {
				return nil, start, err
			}
		}
		break
	}
	line := bytes.TrimRight(lr.buf, "\r\n")
	return line, start, nil
}

// --- разбор записи ---

type entry struct {
	t     time.Time
	level string
	comp  string
	msg   string
	attrs map[string]any
	text  string // для RAW — строка целиком
}

// normLevel сводит уровни slog (в т.ч. "DEBUG-4", "ERROR+2") к четырём основным.
func normLevel(s string) string {
	u := strings.ToUpper(strings.TrimSpace(s))
	switch {
	case strings.HasPrefix(u, "DEBUG"):
		return "DEBUG"
	case strings.HasPrefix(u, "INFO"):
		return "INFO"
	case strings.HasPrefix(u, "WARN"):
		return "WARN"
	case strings.HasPrefix(u, "ERR"):
		return "ERROR"
	}
	return u
}

func parseLine(line []byte) entry {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil || m == nil {
		return entry{level: levelRaw, text: string(line)}
	}
	lv, ok := m["level"].(string)
	if !ok || lv == "" {
		return entry{level: levelRaw, text: string(line)}
	}
	e := entry{level: normLevel(lv)}
	if ts, ok := m["time"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			e.t = t
		}
	}
	e.msg, _ = m["msg"].(string)
	if c, ok := m["comp"].(string); ok {
		e.comp = c
	} else if c, ok := m["component"].(string); ok {
		e.comp = c
	}
	for k, v := range m {
		switch k {
		case "time", "level", "msg", "comp", "component":
			continue
		}
		if e.attrs == nil {
			e.attrs = make(map[string]any, len(m))
		}
		e.attrs[k] = v
	}
	return e
}

// searchText — по чему ищет q: сообщение, компонент, атрибуты k=v; RAW — вся строка.
func (e *entry) searchText() string {
	if e.level == levelRaw {
		return e.text
	}
	var b strings.Builder
	b.WriteString(e.msg)
	if e.comp != "" {
		b.WriteByte(' ')
		b.WriteString(e.comp)
	}
	keys := make([]string, 0, len(e.attrs))
	for k := range e.attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteByte(' ')
		b.WriteString(k)
		b.WriteByte('=')
		if s, ok := e.attrs[k].(string); ok {
			b.WriteString(s)
		} else {
			v, _ := json.Marshal(e.attrs[k])
			b.Write(v)
		}
	}
	return b.String()
}

// --- фильтр ---

type filter struct {
	from, to time.Time
	levels   map[string]bool // nil — все, кроме DEBUG
	comps    map[string]bool // nil — все
	q        string          // в нижнем регистре, если !caseSens
	re       *regexp.Regexp
	caseSens bool
	at       time.Time // обрезано до минуты; zero — нет
}

func splitSet(s string, upper bool) map[string]bool {
	var set map[string]bool
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if upper {
			p = strings.ToUpper(p)
		}
		if set == nil {
			set = make(map[string]bool)
		}
		set[p] = true
	}
	return set
}

func parseFilter(v map[string][]string) (*filter, error) {
	get := func(k string) string {
		if x := v[k]; len(x) > 0 {
			return x[0]
		}
		return ""
	}
	f := &filter{}
	for _, k := range []string{"from", "to", "at"} {
		s := get(k)
		if s == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil, fmt.Errorf("%s: нужен RFC3339", k)
		}
		switch k {
		case "from":
			f.from = t
		case "to":
			f.to = t
		case "at":
			f.at = t.Truncate(time.Minute)
		}
	}
	f.levels = splitSet(get("levels"), true)
	f.comps = splitSet(get("comp"), false)
	f.caseSens = get("case") == "1"
	q := get("q")
	if len(q) > maxQuery {
		return nil, fmt.Errorf("q длиннее %d символов", maxQuery)
	}
	if q != "" {
		if get("regex") == "1" {
			pat := q
			if !f.caseSens {
				pat = "(?i)" + pat
			}
			re, err := regexp.Compile(pat)
			if err != nil {
				return nil, fmt.Errorf("regex: %v", err)
			}
			f.re = re
		} else if f.caseSens {
			f.q = q
		} else {
			f.q = strings.ToLower(q)
		}
	}
	return f, nil
}

// levelOK: по умолчанию всё, кроме DEBUG (RAW тоже); явный список — только он.
func (f *filter) levelOK(lv string) bool {
	if f.levels == nil {
		return lv != levelDebug
	}
	return f.levels[lv]
}

func (f *filter) compOK(c string) bool { return f.comps == nil || f.comps[c] }

// timeOK: строки без времени (и без предыдущего времени) проходят.
func (f *filter) timeOK(t time.Time) bool {
	if t.IsZero() {
		return true
	}
	if !f.from.IsZero() && t.Before(f.from) {
		return false
	}
	if !f.to.IsZero() && t.After(f.to) {
		return false
	}
	return true
}

func (f *filter) queryOK(e *entry) bool {
	if f.re == nil && f.q == "" {
		return true
	}
	text := e.searchText()
	if f.re != nil {
		return f.re.MatchString(text)
	}
	if !f.caseSens {
		text = strings.ToLower(text)
	}
	return strings.Contains(text, f.q)
}

// --- проход по файлу ---

type histPoint struct {
	t   int64 // unix ms
	err bool
}

type errPos struct {
	Line int    `json:"line"`
	T    string `json:"t,omitempty"`
}

type scanResult struct {
	total     int
	levels    map[string]int
	comps     map[string]int
	hist      []histPoint
	errors    []errPos
	atIdx     int // -1 — at не задан или не найден
	truncated bool
}

// scanFile — один проход: счётчики, гистограмма и onMatch для каждой
// строки отфильтрованной выдачи. RAW-строки наследуют время предыдущей записи.
func scanFile(r io.Reader, f *filter, onMatch func(idx int, e *entry, line []byte, start int64) error) (scanResult, error) {
	res := scanResult{levels: map[string]int{}, comps: map[string]int{}, atIdx: -1}
	lr := newLineReader(r)
	var lastT time.Time
	for {
		if lr.off >= maxScan {
			res.truncated = true
			break
		}
		line, start, err := lr.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return res, err
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		e := parseLine(line)
		if e.t.IsZero() {
			e.t = lastT
		} else {
			lastT = e.t
		}
		if !f.timeOK(e.t) {
			continue
		}
		lOK, cOK := f.levelOK(e.level), f.compOK(e.comp)
		if !lOK && !cOK {
			continue
		}
		qOK := f.queryOK(&e)
		if cOK && qOK {
			res.levels[e.level]++
		}
		if lOK && qOK && e.comp != "" {
			res.comps[e.comp]++
		}
		if !lOK || !cOK {
			continue
		}
		// Гистограмма — без учёта q.
		if !e.t.IsZero() {
			res.hist = append(res.hist, histPoint{t: e.t.UnixMilli(), err: e.level == levelError})
		}
		if !qOK {
			continue
		}
		idx := res.total
		res.total++
		if e.level == levelError && len(res.errors) < maxErrors {
			res.errors = append(res.errors, errPos{Line: idx, T: fmtTime(e.t)})
		}
		if !f.at.IsZero() && res.atIdx < 0 && !e.t.IsZero() && !e.t.Before(f.at) {
			res.atIdx = idx
		}
		if onMatch != nil {
			if err := onMatch(idx, &e, line, start); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

type histBucket struct {
	T   string `json:"t"`
	N   int    `json:"n"`
	Err int    `json:"err"`
}

// histogram — 60 корзин по [from|min, to|max]. Нет ни данных, ни границ — пусто.
func histogram(points []histPoint, from, to time.Time) []histBucket {
	var lo, hi int64
	if len(points) > 0 {
		lo, hi = points[0].t, points[0].t
		for _, p := range points {
			lo, hi = min(lo, p.t), max(hi, p.t)
		}
	}
	if !from.IsZero() {
		lo = from.UnixMilli()
	}
	if !to.IsZero() {
		hi = to.UnixMilli()
	}
	if len(points) == 0 && (from.IsZero() || to.IsZero()) {
		return []histBucket{}
	}
	span := hi - lo
	if span <= 0 {
		span = int64(time.Minute / time.Millisecond)
	}
	width := (span + histBuckets) / histBuckets // округление вверх, ≥ 1 мс
	out := make([]histBucket, histBuckets)
	for i := range out {
		out[i].T = time.UnixMilli(lo + int64(i)*width).UTC().Format(time.RFC3339)
	}
	for _, p := range points {
		i := int((p.t - lo) / width)
		if i < 0 || i >= histBuckets {
			if p.t < lo || p.t > hi {
				continue
			}
			i = histBuckets - 1
		}
		out[i].N++
		if p.err {
			out[i].Err++
		}
	}
	return out
}

// --- ручки ---

type outLine struct {
	Line  int            `json:"line"`
	T     string         `json:"t,omitempty"`
	Level string         `json:"level"`
	Comp  string         `json:"comp,omitempty"`
	Msg   string         `json:"msg,omitempty"`
	Attrs map[string]any `json:"attrs,omitempty"`
	Text  string         `json:"text,omitempty"`
}

type logsResp struct {
	Source     string         `json:"source"`
	File       string         `json:"file"`
	Files      []string       `json:"files"`
	Total      int            `json:"total"`
	Page       int            `json:"page"`
	Size       int            `json:"size"`
	Pages      int            `json:"pages"`
	Levels     map[string]int `json:"levels"`
	Components map[string]int `json:"components"`
	Histogram  []histBucket   `json:"histogram"`
	Errors     []errPos       `json:"errors"`
	Lines      []outLine      `json:"lines"`
	Truncated  bool           `json:"truncated,omitempty"` // файл прочитан не целиком (maxScan)
}

// openLog — файл источника из query `file` (по умолчанию самый новый).
func (a *API) openLog(w http.ResponseWriter, r *http.Request) (*os.File, string, []logstore.FileInfo, bool) {
	if a.opt.Logs == nil {
		writeErr(w, http.StatusServiceUnavailable, "logs not configured")
		return nil, "", nil, false
	}
	source := r.PathValue("source")
	files, err := a.opt.Logs.SourceFiles(source)
	if err != nil {
		writeErr(w, http.StatusNotFound, "source not found")
		return nil, "", nil, false
	}
	path, name, err := a.opt.Logs.FilePath(source, r.URL.Query().Get("file"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "file not found")
		return nil, "", nil, false
	}
	fh, err := os.Open(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "file not found")
		return nil, "", nil, false
	}
	return fh, name, files, true
}

type lineRef struct {
	off int64
	n   int32
	t   int64 // unix ms унаследованного времени; 0 — нет
}

func (a *API) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f, err := parseFilter(q)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	page, size := 0, defaultSize
	if s := q.Get("page"); s != "" {
		if page, err = strconv.Atoi(s); err != nil || page < 0 {
			writeErr(w, http.StatusBadRequest, "page")
			return
		}
	}
	if s := q.Get("size"); s != "" {
		if size, err = strconv.Atoi(s); err != nil || size < 1 {
			writeErr(w, http.StatusBadRequest, "size")
			return
		}
		size = min(size, maxSize)
	}

	fh, name, files, ok := a.openLog(w, r)
	if !ok {
		return
	}
	defer fh.Close()

	// Запоминаем только смещения совпавших строк; страницу дочитываем после
	// прохода — номер страницы при `at` известен лишь в конце.
	var refs []lineRef
	res, err := scanFile(fh, f, func(idx int, e *entry, line []byte, start int64) error {
		var t int64
		if !e.t.IsZero() {
			t = e.t.UnixMilli()
		}
		refs = append(refs, lineRef{off: start, n: int32(len(line)), t: t})
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read failed")
		return
	}

	pages := (res.total + size - 1) / size
	if !f.at.IsZero() {
		if res.atIdx >= 0 {
			page = res.atIdx / size
		} else {
			page = max(pages-1, 0)
		}
	}

	lines := []outLine{}
	for i := page * size; i < min((page+1)*size, len(refs)); i++ {
		ref := refs[i]
		buf := make([]byte, ref.n)
		if _, err := fh.ReadAt(buf, ref.off); err != nil && !errors.Is(err, io.EOF) {
			writeErr(w, http.StatusInternalServerError, "read failed")
			return
		}
		e := parseLine(buf)
		ol := outLine{Line: i, Level: e.level, Comp: e.comp, Msg: e.msg, Attrs: e.attrs, Text: e.text}
		if ref.t != 0 {
			ol.T = time.UnixMilli(ref.t).UTC().Format(time.RFC3339)
		}
		lines = append(lines, ol)
	}

	names := make([]string, len(files))
	for i, fi := range files {
		names[i] = fi.Name
	}
	errs := res.errors
	if errs == nil {
		errs = []errPos{}
	}
	writeJSON(w, http.StatusOK, logsResp{
		Source:     r.PathValue("source"),
		File:       name,
		Files:      names,
		Total:      res.total,
		Page:       page,
		Size:       size,
		Pages:      pages,
		Levels:     res.levels,
		Components: res.comps,
		Histogram:  histogram(res.hist, f.from, f.to),
		Errors:     errs,
		Lines:      lines,
		Truncated:  res.truncated,
	})
}

// safeFilename — имя для Content-Disposition.
var unsafeFileRe = regexp.MustCompile(`[^a-zA-Z0-9._#-]`)

func (a *API) handleLogsDownload(w http.ResponseWriter, r *http.Request) {
	f, err := parseFilter(r.URL.Query())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	f.at = time.Time{}
	fh, name, _, ok := a.openLog(w, r)
	if !ok {
		return
	}
	defer fh.Close()

	fn := unsafeFileRe.ReplaceAllString(r.PathValue("source")+"_"+strings.TrimSuffix(name, ".jsonl"), "_") + ".jsonl"
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="`+fn+`"`)
	w.Header().Set("Cache-Control", "no-store")
	bw := bufio.NewWriterSize(w, 64<<10)
	_, err = scanFile(fh, f, func(idx int, e *entry, line []byte, start int64) error {
		if _, err := bw.Write(line); err != nil {
			return err
		}
		return bw.WriteByte('\n')
	})
	if err != nil {
		a.log.Debug("выгрузка логов прервана", "error", err.Error())
		return
	}
	_ = bw.Flush()
}

// --- источники ---

// fileStats — счётчики файла, кешируются по mtime+size.
type fileStats struct {
	mtime   time.Time
	size    int64
	lines   int
	errors  int
	warns   int
	version string
	os      string
}

type sourceResp struct {
	Name     string              `json:"name"`
	Files    []logstore.FileInfo `json:"files"`
	LastSeen string              `json:"last_seen,omitempty"`
	Lines    int                 `json:"lines"`
	Errors   int                 `json:"errors"`
	Warns    int                 `json:"warns"`
	Version  string              `json:"version,omitempty"`
	OS       string              `json:"os,omitempty"`
}

func (a *API) handleLogSources(w http.ResponseWriter, r *http.Request) {
	if a.opt.Logs == nil {
		writeErr(w, http.StatusServiceUnavailable, "logs not configured")
		return
	}
	out := []sourceResp{}
	for _, si := range a.opt.Logs.ListSources() {
		files, err := a.opt.Logs.SourceFiles(si.Name)
		if err != nil {
			continue
		}
		sr := sourceResp{Name: si.Name, Files: files}
		if !si.LastPushAt.IsZero() {
			sr.LastSeen = si.LastPushAt.UTC().Format(time.RFC3339)
		}
		if len(files) > 0 {
			if path, _, err := a.opt.Logs.FilePath(si.Name, files[0].Name); err == nil {
				if st, ok := a.fileStats(path); ok {
					sr.Lines, sr.Errors, sr.Warns = st.lines, st.errors, st.warns
					sr.Version, sr.OS = st.version, st.os
				}
			}
		}
		out = append(out, sr)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": out})
}

// fileStats — счётчики текущего файла источника (из кеша, если файл не менялся).
func (a *API) fileStats(path string) (fileStats, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return fileStats{}, false
	}
	a.statsMu.Lock()
	c, ok := a.stats[path]
	a.statsMu.Unlock()
	if ok && c.mtime.Equal(st.ModTime()) && c.size == st.Size() {
		return c, true
	}

	fh, err := os.Open(path)
	if err != nil {
		return fileStats{}, false
	}
	defer fh.Close()
	fs := fileStats{mtime: st.ModTime(), size: st.Size()}
	lr := newLineReader(fh)
	for lr.off < maxScan {
		line, _, err := lr.next()
		if err != nil {
			break
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		fs.lines++
		var rec struct {
			Level      string `json:"level"`
			Version    string `json:"version"`
			AppVersion string `json:"app_version"`
			OS         string `json:"os"`
			GOOS       string `json:"goos"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		switch normLevel(rec.Level) {
		case "ERROR":
			fs.errors++
		case "WARN":
			fs.warns++
		}
		if rec.Version != "" {
			fs.version = rec.Version
		} else if rec.AppVersion != "" {
			fs.version = rec.AppVersion
		}
		if rec.OS != "" {
			fs.os = rec.OS
		} else if rec.GOOS != "" {
			fs.os = rec.GOOS
		}
	}

	a.statsMu.Lock()
	if len(a.stats) > 1000 {
		a.stats = make(map[string]fileStats)
	}
	a.stats[path] = fs
	a.statsMu.Unlock()
	return fs, true
}
