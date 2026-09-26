package services

import (
	"fmt"
	"image"
	_ "image/gif"  // регистрация декодеров для image.Decode
	"image/jpeg"
	_ "image/png"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	xdraw "golang.org/x/image/draw"

	"vox_app/internal/logsafe"
)

// PreparedAttachment — то что фронт получает после PrepareAttachments.
// PreviewURL — относительный путь, готовый идти в <img src> (vox-local://token
// эмулируется через AssetBridge на /vox-local/<token>).
// Width/Height для image/* — нужны для адаптивного layout'а в picker-модалке
// (portrait vs landscape), 0 если не удалось декодировать.
type PreparedAttachment struct {
	Path       string `json:"path"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	Mime       string `json:"mime"`
	IsImage    bool   `json:"isImage"`
	PreviewURL string `json:"previewUrl"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
}

// previewMaxBytes — выше этого размера не отдаём оригинал картинки в превью,
// генерим уменьшенный JPEG. Open большого RAW/PNG в WebView может блокировать
// рендер на секунды и течь памятью.
const previewMaxBytes = 10 * 1024 * 1024

// previewMaxEdge — длинная сторона сгенерированного превью.
const previewMaxEdge = 600

// previewQuality — JPEG q для thumbnail.
const previewQuality = 75

// previewTokenTTL — токен превью живёт ровно столько, сколько модалка открыта.
// Caller должен вызвать RevokeLocalToken при закрытии модалки.
const previewTokenTTL = 30 * time.Minute

// PickAttachments открывает native multi-file picker. Возвращает абсолютные
// пути выбранных файлов; при отмене — пустой массив без ошибки.
//
// Wails-method: фронт зовёт после клика на скрепку, потом передаёт результат
// в PrepareAttachments чтобы получить превью и метаданные.
func (s *MessagingService) PickAttachments() ([]string, error) {
	const op = "MessagingService.PickAttachments"
	app := application.Get()
	if app == nil {
		return nil, fmt.Errorf("%s: no app instance", op)
	}
	dlg := app.Dialog.OpenFile().
		CanChooseFiles(true).
		CanChooseDirectories(false).
		SetTitle("Выберите файлы для отправки")
	paths, err := dlg.PromptForMultipleSelection()
	if err != nil {
		// Cancel — у Wails разные сообщения на разных OS, проверяем по подстроке.
		if strings.Contains(strings.ToLower(err.Error()), "cancel") {
			return []string{}, nil
		}
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if paths == nil {
		paths = []string{}
	}
	return paths, nil
}

// PrepareAttachments снимает stat+mime для каждого пути и регистрирует
// vox-local токены под превью. Возвращается в порядке входных путей; пути
// которые не открылись — пропускаются с warn-логом (битый удалённый файл,
// permission denied и т.п.).
//
// Caller (фронт) после Submit/Cancel модалки обязан вызвать RevokePreviewTokens
// чтобы убрать токены из реестра. Если не вызвать — токены протухнут сами через
// previewTokenTTL.
func (s *MessagingService) PrepareAttachments(paths []string) ([]PreparedAttachment, error) {
	if s.bridge == nil {
		return nil, fmt.Errorf("PrepareAttachments: asset bridge not configured")
	}
	out := make([]PreparedAttachment, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil {
			s.log.Warn("prepare attachment: stat",
				logsafe.Err("err", err))
			continue
		}
		if fi.IsDir() {
			s.log.Warn("prepare attachment: skip directory")
			continue
		}
		mt := mime.TypeByExtension(strings.ToLower(filepath.Ext(p)))
		if mt == "" {
			mt = "application/octet-stream"
		}
		// Парсим тип без параметров (image/jpeg; charset=… → image/jpeg).
		base := mt
		if i := strings.IndexByte(base, ';'); i >= 0 {
			base = strings.TrimSpace(base[:i])
		}
		isImage := strings.HasPrefix(base, "image/")

		pa := PreparedAttachment{
			Path:    p,
			Name:    filepath.Base(p),
			Size:    fi.Size(),
			Mime:    base,
			IsImage: isImage,
		}

		if isImage {
			// Размеры image — нужны для grid layout'а на фронте. DecodeConfig
			// читает только header (PNG IHDR / JPEG SOF), без декодирования
			// пиксельных данных — быстро даже для огромных RAW.
			if w, h, ok := decodeImageDims(p); ok {
				pa.Width = w
				pa.Height = h
			}
			// Для маленьких картинок отдаём оригинал — браузер декодирует один раз.
			// Для больших — генерим уменьшенный JPEG в os.TempDir, токен на него.
			previewPath := p
			previewMime := base
			if fi.Size() > previewMaxBytes {
				thumbPath, terr := makeImageThumb(p, previewMaxEdge, previewQuality)
				if terr != nil {
					s.log.Warn("prepare attachment: thumb",
						logsafe.Err("err", terr))
					// Падаем на оригинал — пусть WebView попробует.
				} else {
					previewPath = thumbPath
					previewMime = "image/jpeg"
				}
			}
			tok, terr := s.bridge.RegisterLocalToken(previewPath, previewMime, previewTokenTTL)
			if terr != nil {
				s.log.Warn("prepare attachment: token",
					logsafe.Err("err", terr))
			} else {
				pa.PreviewURL = "/vox-local/" + tok
			}
		}
		out = append(out, pa)
	}
	return out, nil
}

// RevokePreviewTokens — фронт вызывает после закрытия picker-модалки.
// Освобождает память реестра tokens сразу, не ждёт TTL.
func (s *MessagingService) RevokePreviewTokens(tokens []string) {
	if s.bridge == nil {
		return
	}
	for _, t := range tokens {
		if t == "" {
			continue
		}
		// Если t пришёл как полный URL "/vox-local/<token>" — обрежем.
		if strings.HasPrefix(t, "/vox-local/") {
			t = strings.TrimPrefix(t, "/vox-local/")
		}
		s.bridge.RevokeLocalToken(t)
	}
}

// makeImageThumb декодирует оригинал, ресайзит до maxEdge × maxEdge bound,
// кодирует в JPEG quality% и пишет в os.TempDir/vox-prep/<unique>.jpg.
// Возвращает абсолютный путь созданного thumbnail.
//
// Использует CatmullRom — компромис качество/CPU, на 8 MP картинке ~150ms.
func makeImageThumb(srcPath string, maxEdge, quality int) (string, error) {
	f, err := os.Open(srcPath)
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}

	bounds := img.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	dstW, dstH := scaleToBound(srcW, srcH, maxEdge)
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, xdraw.Over, nil)

	tmpDir := filepath.Join(os.TempDir(), "vox-prep")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}
	out, err := os.CreateTemp(tmpDir, "thumb-*.jpg")
	if err != nil {
		return "", fmt.Errorf("create tmp: %w", err)
	}
	if err := jpeg.Encode(out, dst, &jpeg.Options{Quality: quality}); err != nil {
		_ = out.Close()
		_ = os.Remove(out.Name())
		return "", fmt.Errorf("jpeg encode: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(out.Name())
		return "", fmt.Errorf("close: %w", err)
	}
	return out.Name(), nil
}

// scaleToBound уменьшает (w,h) пропорционально, чтобы long edge == maxEdge.
// Если оба измерения уже ≤ maxEdge — возвращает оригинальные.
func scaleToBound(w, h, maxEdge int) (int, int) {
	if w <= maxEdge && h <= maxEdge {
		return w, h
	}
	if w >= h {
		return maxEdge, h * maxEdge / w
	}
	return w * maxEdge / h, maxEdge
}

// --- Отправка по path (без base64) ---

// compressMaxEdge — длинная сторона ужатого фото в "compress=true" режиме.
const compressMaxEdge = 1920

// compressQuality — JPEG q для пользовательских фото.
const compressQuality = 85

// putBlobsByPath обрабатывает каждый input:
//   - kind="photo" + Compress=true: image.Decode → resize → jpeg.Encode → BlobStore
//   - иначе: os.Open → io.Copy → BlobStore (стрим, RAM = 1 MiB chunk buffer)
//
// Возвращает stored []storedBlob и параллельный массив "виртуальных" AttachmentInput
// для attachBlobsToMsgTx — переиспользует существующий tx-helper.
func (s *MessagingService) putBlobsByPath(items []AttachByPathInput) ([]storedBlob, []AttachmentInput, error) {
	stored := make([]storedBlob, len(items))
	fakeAtts := make([]AttachmentInput, len(items))

	for i, it := range items {
		name := it.Name
		if name == "" {
			name = filepath.Base(it.Path)
		}

		var (
			sha    string
			size   int64
			mt     string
			width  int
			height int
		)

		needCompress := it.Kind == "photo" && it.Compress
		if needCompress {
			s2, sz, w, h, err := s.compressImageToBlob(it.Path, compressMaxEdge, compressQuality)
			if err != nil {
				// Если не смогли декодировать — падаем на raw-путь, лучше отдать
				// оригинал чем фейлить отправку. compress="best effort".
				s.log.Warn("attach: compress failed, falling back to raw",
					logsafe.Err("err", err))
				needCompress = false
			} else {
				sha = s2
				size = sz
				width = w
				height = h
				mt = "image/jpeg"
				name = renameToJPG(name)
			}
		}
		if !needCompress {
			s2, sz, err := s.streamFileToBlob(it.Path)
			if err != nil {
				return stored, fakeAtts, fmt.Errorf("att %d (%s): %w", i, name, err)
			}
			sha = s2
			size = sz
			mt = mimeOf(it.Path)
			// Для не-сжатых картинок — попробуем считать размеры (для grid layout
			// на фронте). Если декодер не справится — оставим 0,0.
			if it.Kind == "photo" {
				if w, h, ok := decodeImageDims(it.Path); ok {
					width, height = w, h
				}
			}
		}

		// existed-флаг: WasNew=true только если этот sha впервые лёг на диск.
		// Не идеально определять это после Put (упустим если кто-то параллельно
		// положил тот же blob), но cleanup orphan'ов — disk-scan, не критично.
		existed := s.blobs.Exists(sha) // да, Put уже создал — здесь Exists всегда true
		_ = existed                    // используем wasNew=true пессимистично — disk-GC доберёт orphan если что
		stored[i] = storedBlob{
			Sha:    sha,
			Bytes:  size,
			Mime:   mt,
			WasNew: true,
		}
		fakeAtts[i] = AttachmentInput{
			Kind:   it.Kind,
			Name:   name,
			Mime:   mt,
			Size:   size,
			Width:  width,
			Height: height,
			// Data не используется в attachBlobsToMsgTx — только Width/Height/Name/Kind.
		}
	}
	return stored, fakeAtts, nil
}

// streamFileToBlob: open → BlobStore.PutFromReader. RAM = 1 MiB writer buffer.
func (s *MessagingService) streamFileToBlob(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", 0, fmt.Errorf("stat: %w", err)
	}
	sha, err := s.blobs.PutFromReader(f)
	if err != nil {
		return "", 0, fmt.Errorf("blob put: %w", err)
	}
	return sha, fi.Size(), nil
}

// compressImageToBlob: image.Decode → CatmullRom resize → jpeg.Encode → BlobStore.Writer.
// Стримит сжатый результат прямо в writer'а — без temp-файла.
func (s *MessagingService) compressImageToBlob(srcPath string, maxEdge, quality int) (string, int64, int, int, error) {
	f, err := os.Open(srcPath)
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("decode: %w", err)
	}

	bounds := img.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	dstW, dstH := scaleToBound(srcW, srcH, maxEdge)
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, xdraw.Over, nil)

	w, err := s.blobs.NewWriter()
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("blob writer: %w", err)
	}
	cw := &countingWriter{dst: w}
	if err := jpeg.Encode(cw, dst, &jpeg.Options{Quality: quality}); err != nil {
		_ = w.Abort()
		return "", 0, 0, 0, fmt.Errorf("jpeg encode: %w", err)
	}
	sha, err := w.Finalize()
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("blob finalize: %w", err)
	}
	return sha, cw.n, dstW, dstH, nil
}

// countingWriter — подсчитывает байты, прошедшие через writer (нужно для size
// сжатого JPEG, оригинальный fi.Size() не подходит).
type countingWriter struct {
	dst interface {
		Write([]byte) (int, error)
	}
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.dst.Write(p)
	c.n += int64(n)
	return n, err
}

// decodeImageDims пытается прочитать только размеры (image.DecodeConfig — без
// загрузки пикселей). Используется для grid-layout на фронте.
func decodeImageDims(path string) (int, int, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, false
	}
	return cfg.Width, cfg.Height, true
}

// mimeOf — mime по расширению; fallback на octet-stream.
func mimeOf(path string) string {
	mt := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if mt == "" {
		return "application/octet-stream"
	}
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	return mt
}

// renameToJPG меняет расширение на .jpg (для compress=true: исходник был .png/.heic,
// в БД уезжает уже как jpeg — имя файла должно отражать новый формат, иначе
// receiver откроет по mime image/jpeg, но Save As предложит .png).
func renameToJPG(name string) string {
	ext := filepath.Ext(name)
	if ext == "" {
		return name + ".jpg"
	}
	return strings.TrimSuffix(name, ext) + ".jpg"
}
