package services

import (
	"log/slog"
	"strconv"
	"time"
	"vox_app/internal/audio"
)

// Live-настройка шумодава во время голосового вызова.
//
// Эти методы вызываются из AudioService при изменении пользователем уровня
// или силы NS в UI — чтобы изменение применилось мгновенно к текущему capture
// без рестарта канала.
//
// Если capture не активен (пользователь не в канале) — методы no-op,
// настройка применится при следующем JoinGroupVoice из settings.

// ApplyNoiseSuppression проксирует уровень шумодава на текущий live capture.
//
// ВАЖНО: при переключении на NSDeep ленивая загрузка DFN3 модели выполняется
// АСИНХРОННО в отдельной goroutine вне s.mu. Синхронная загрузка блокировала
// audioLoop на ~450 мс (парсинг tar.gz + инициализация tract-onnx), из-за чего
// capture-сэмплы накапливались в ring-буфере и pipeline сдвигался на эти 450 мс
// на всё последующее время сессии (см. max_loop и wire_max в логах).
//
// До завершения async-загрузки уровень NSDeep работает как RNNoise (C callback
// видит cap->ns_df == NULL и делает fallback). Это приемлемый компромисс:
// ~450 мс на мягкий переход без фризов RT-потока.
func (s *VoiceService) ApplyNoiseSuppression(level audio.NoiseSuppressionLevel) {
	s.mu.Lock()
	cap := s.capture
	s.mu.Unlock()
	if cap == nil {
		return
	}
	// SetNoiseSuppression меняет только volatile int в C — атомарно, без lock.
	cap.SetNoiseSuppression(level)

	// Async-загрузка модели если нужно. s.dfnLoadWG гарантирует что leaveInternal
	// дождётся завершения перед освобождением capture.
	needLoad := level == audio.NSDeep && !cap.DFModelLoaded()
	if needLoad {
		s.dfnLoadWG.Add(1)
		go func() {
			defer s.dfnLoadWG.Done()
			start := time.Now()
			loadDFModelIfPossible(s.log, cap)
			s.log.Info("DFN3 async load complete",
				slog.Duration("took", time.Since(start)),
				slog.Bool("dfn_loaded", cap.DFModelLoaded()))
		}()
	}

	s.log.Info("noise suppression applied to live capture",
		slog.Int("level", int(level)),
		slog.Bool("dfn_loaded", cap.DFModelLoaded()),
		slog.Bool("dfn_loading_async", needLoad))
}

// ApplyDFNStrength обновляет DFN3 atten_lim (максимальное подавление в dB)
// на текущем live capture, если он активен. No-op если capture не запущен
// или DFN3 модель не загружена (применится на следующем JoinGroupVoice из
// settings).
func (s *VoiceService) ApplyDFNStrength(db float32) {
	s.mu.Lock()
	cap := s.capture
	s.mu.Unlock()
	if cap == nil {
		return
	}
	cap.SetDFAttenLim(db)
	s.log.Info("DFN strength applied to live capture",
		slog.Float64("db", float64(db)),
		slog.Bool("dfn_loaded", cap.DFModelLoaded()))
}

// --- Neural NS (единый селектор: off / fe / gtcrn, взаимоисключающий) ---
//
// FastEnhancer и GTCRN — две разные ML-модели. Одновременно активен только
// один: при смене режима выгружается текущий и загружается новый. Это нужно
// для чистого A/B сравнения голоса.

// SetNeuralMode переключает режим: "off" | "fe" | "gtcrn".
// Async: старая модель Close, новая стартует загрузкой в фоне. audioLoop
// видит fastEnhancer==nil && gtcrn==nil во время перехода → bypass.
func (s *VoiceService) SetNeuralMode(mode string) {
	switch mode {
	case "off", "fe", "gtcrn":
		// ok
	default:
		s.log.Warn("SetNeuralMode: invalid mode", slog.String("mode", mode))
		return
	}
	s.mu.Lock()
	if s.neuralMode == mode {
		s.mu.Unlock()
		return
	}
	s.neuralMode = mode
	s.persistNeuralSettings()
	// Деактивация активной модели — НЕ Close, она остаётся в кэше для будущих
	// переключений. audioLoop просто видит nil и bypass'ит.
	if mode != "fe" {
		s.fastEnhancer = nil
	}
	if mode != "gtcrn" {
		s.gtcrn = nil
	}
	// Активация новой модели: cache hit → мгновенный swap, miss → async load.
	if s.capture != nil && s.activeGroupID != "" {
		switch mode {
		case "fe":
			if s.fastEnhancer == nil {
				s.startFastEnhancerLoadLocked()
			}
		case "gtcrn":
			if s.gtcrn == nil {
				s.startGTCRNLoadLocked()
			}
		}
	}
	s.mu.Unlock()
	s.log.Info("neural NS mode changed", slog.String("mode", mode))
}

// GetNeuralMode возвращает текущий режим (для UI).
func (s *VoiceService) GetNeuralMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.neuralMode
}

// GetNeuralAvailable — true если бинарь собран с nx_onnx (shared для FE и GTCRN).
func (s *VoiceService) GetNeuralAvailable() bool {
	return audio.FastEnhancerAvailable()
}

// --- FastEnhancer параметры (размер модели, mix) ---

// SetFastEnhancerSize меняет размер модели (t/b/s/m/l). Если режим == "fe" и
// сессия активна — hot-swap async: старая модель работает до конца загрузки.
func (s *VoiceService) SetFastEnhancerSize(size string) {
	switch size {
	case "t", "b", "s", "m", "l":
		// ok
	default:
		s.log.Warn("SetFastEnhancerSize: invalid size", slog.String("size", size))
		return
	}
	s.mu.Lock()
	feSize := audio.FastEnhancerSize(size)
	if s.feSize == feSize {
		s.mu.Unlock()
		return
	}
	s.feSize = feSize
	s.persistNeuralSettings()
	if s.neuralMode == "fe" && s.capture != nil && s.activeGroupID != "" {
		s.startFastEnhancerLoadLocked()
	}
	s.mu.Unlock()
}

// SetFastEnhancerMix wet/dry slider (0..1).
func (s *VoiceService) SetFastEnhancerMix(mix float32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.feMix = mix
	s.persistNeuralSettings()
	if s.fastEnhancer != nil {
		s.fastEnhancer.SetMix(mix)
	}
}

func (s *VoiceService) GetFastEnhancerSize() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.feSize)
}

func (s *VoiceService) GetFastEnhancerMix() float32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.feMix
}

// --- GTCRN параметры ---

// SetGTCRNVariant — dns3 | simple. Hot-swap если активен.
func (s *VoiceService) SetGTCRNVariant(variant string) {
	switch variant {
	case "dns3", "simple":
		// ok
	default:
		s.log.Warn("SetGTCRNVariant: invalid variant", slog.String("variant", variant))
		return
	}
	s.mu.Lock()
	v := audio.GTCRNVariant(variant)
	if s.gtcrnVariant == v {
		s.mu.Unlock()
		return
	}
	s.gtcrnVariant = v
	s.persistNeuralSettings()
	if s.neuralMode == "gtcrn" && s.capture != nil && s.activeGroupID != "" {
		s.startGTCRNLoadLocked()
	}
	s.mu.Unlock()
}

// SetGTCRNMix wet/dry slider (0..1).
func (s *VoiceService) SetGTCRNMix(mix float32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gtcrnMix = mix
	s.persistNeuralSettings()
	if s.gtcrn != nil {
		s.gtcrn.SetMix(mix)
	}
}

func (s *VoiceService) GetGTCRNVariant() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.gtcrnVariant)
}

func (s *VoiceService) GetGTCRNMix() float32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gtcrnMix
}

// LoadNeuralNSSettings читает все neural-настройки из БД при старте сервиса
// и стартует prewarm всех моделей в фоне ДО первого Join — чтобы ORT
// NewSession не сериализовался с активным audio Run в audioLoop (через
// внутренний ORT mutex это давало max_loop 130+ мс пиков и шок очереди
// mixer'а на 100-150 мс).
func (s *VoiceService) LoadNeuralNSSettings() {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		// Запускаем prewarm в goroutine после освобождения s.mu — не блокирует
		// startup, к моменту первого Join кэш либо уже готов либо почти готов.
		if audio.FastEnhancerAvailable() {
			go s.prewarmNeuralCache()
		}
	}()

	// Миграция со старого fe_enabled → neural_mode. Если neural_mode не задан
	// но fe_enabled=1 → ставим "fe", чтобы не терять состояние после обновления.
	var mode string
	if s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'neural_mode'").Scan(&mode) == nil && mode != "" {
		s.neuralMode = mode
	} else {
		var en string
		if s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'fe_enabled'").Scan(&en) == nil && en == "1" {
			s.neuralMode = "fe"
		}
	}

	// FE: size / mix
	var sz string
	if s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'fe_size'").Scan(&sz) == nil && sz != "" {
		s.feSize = audio.FastEnhancerSize(sz)
	}
	var mix string
	if s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'fe_mix'").Scan(&mix) == nil {
		if v, err := strconv.ParseFloat(mix, 32); err == nil {
			s.feMix = float32(v)
		}
	}
	// GTCRN: variant / mix
	var gv string
	if s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'gtcrn_variant'").Scan(&gv) == nil && gv != "" {
		s.gtcrnVariant = audio.GTCRNVariant(gv)
	}
	var gmix string
	if s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'gtcrn_mix'").Scan(&gmix) == nil {
		if v, err := strconv.ParseFloat(gmix, 32); err == nil {
			s.gtcrnMix = float32(v)
		}
	}
	s.log.Info("neural NS settings loaded",
		slog.String("mode", s.neuralMode),
		slog.String("fe_size", string(s.feSize)),
		slog.Float64("fe_mix", float64(s.feMix)),
		slog.String("gtcrn_variant", string(s.gtcrnVariant)),
		slog.Float64("gtcrn_mix", float64(s.gtcrnMix)),
		slog.Bool("available", audio.FastEnhancerAvailable()))
}

// persistNeuralSettings — все neural-настройки одним блоком. Под s.mu.
func (s *VoiceService) persistNeuralSettings() {
	db := s.store.DB()
	db.Exec("INSERT OR REPLACE INTO settings (key, value) VALUES ('neural_mode', ?)", s.neuralMode)
	db.Exec("INSERT OR REPLACE INTO settings (key, value) VALUES ('fe_size', ?)", string(s.feSize))
	db.Exec("INSERT OR REPLACE INTO settings (key, value) VALUES ('fe_mix', ?)",
		strconv.FormatFloat(float64(s.feMix), 'f', 3, 32))
	db.Exec("INSERT OR REPLACE INTO settings (key, value) VALUES ('gtcrn_variant', ?)", string(s.gtcrnVariant))
	db.Exec("INSERT OR REPLACE INTO settings (key, value) VALUES ('gtcrn_mix', ?)",
		strconv.FormatFloat(float64(s.gtcrnMix), 'f', 3, 32))
}

// startFastEnhancerLoadLocked — активирует FE текущего размера. Если есть в
// кэше — мгновенный pointer swap (без ORT NewSession). Иначе ASYNC load с
// сохранением в кэш для будущих переключений. Под s.mu.
func (s *VoiceService) startFastEnhancerLoadLocked() {
	if cached, ok := s.feCache[s.feSize]; ok && cached != nil {
		// Мгновенный swap — модель уже прогрета, переключение без блокировки.
		cached.SetMix(s.feMix)
		s.fastEnhancer = cached
		s.log.Info("fastenhancer activated from cache", slog.String("size", string(s.feSize)))
		return
	}

	s.feLoadGen++
	gen := s.feLoadGen
	size := s.feSize
	mix := s.feMix
	s.feLoadWG.Add(1)
	go func() {
		defer s.feLoadWG.Done()
		start := time.Now()
		fe, err := audio.NewFastEnhancer(size, s.log)
		took := time.Since(start)
		if err != nil {
			s.log.Warn("fastenhancer async load failed",
				slog.String("size", string(size)),
				slog.Duration("took", took),
				slog.String("error", err.Error()))
			return
		}
		fe.SetMix(mix)
		s.mu.Lock()
		if s.feLoadGen != gen || s.capture == nil || s.activeGroupID == "" {
			s.mu.Unlock()
			fe.Close()
			s.log.Info("fastenhancer load discarded", slog.Duration("took", took))
			return
		}
		// Сохраняем в кэш всегда (даже если режим уже сменился — пригодится дальше)
		s.feCache[size] = fe
		// Активируем только если режим всё ещё "fe" и размер совпадает.
		if s.neuralMode == "fe" && s.feSize == size {
			s.fastEnhancer = fe
		}
		s.mu.Unlock()
		s.log.Info("fastenhancer async load complete",
			slog.String("size", string(size)), slog.Duration("took", took))
	}()
}

// startGTCRNLoadLocked — симметрично FE: cache-first activation, async load с кэшем.
func (s *VoiceService) startGTCRNLoadLocked() {
	if cached, ok := s.gtcrnCache[s.gtcrnVariant]; ok && cached != nil {
		cached.SetMix(s.gtcrnMix)
		s.gtcrn = cached
		s.log.Info("gtcrn activated from cache", slog.String("variant", string(s.gtcrnVariant)))
		return
	}

	s.feLoadGen++
	gen := s.feLoadGen
	variant := s.gtcrnVariant
	mix := s.gtcrnMix
	s.feLoadWG.Add(1)
	go func() {
		defer s.feLoadWG.Done()
		start := time.Now()
		g, err := audio.NewGTCRN(variant, s.log)
		took := time.Since(start)
		if err != nil {
			s.log.Warn("gtcrn async load failed",
				slog.String("variant", string(variant)),
				slog.Duration("took", took),
				slog.String("error", err.Error()))
			return
		}
		g.SetMix(mix)
		s.mu.Lock()
		if s.feLoadGen != gen || s.capture == nil || s.activeGroupID == "" {
			s.mu.Unlock()
			g.Close()
			s.log.Info("gtcrn load discarded", slog.Duration("took", took))
			return
		}
		s.gtcrnCache[variant] = g
		if s.neuralMode == "gtcrn" && s.gtcrnVariant == variant {
			s.gtcrn = g
		}
		s.mu.Unlock()
		s.log.Info("gtcrn async load complete",
			slog.String("variant", string(variant)), slog.Duration("took", took))
	}()
}

// prewarmNeuralCache последовательно загружает все 5 FE размеров и 2 GTCRN
// варианта в кэш — выполняется в фоновой goroutine из onVoiceJoin. Каждая
// модель идёт после предыдущей чтобы не было одновременных NewSession()
// которые конкурируют за ORT runtime resources. Активная модель пропускается
// (уже грузится через startXxxLoadLocked отдельно).
//
// Итого ~2-3 секунды фоновой работы, после чего любое переключение моделей
// в UI = мгновенный pointer swap без ORT contention.
func (s *VoiceService) prewarmNeuralCache() {
	// Захватываем generation на старте — если leaveInternal обнулит за время
	// prewarm, мы не должны добавлять модели в новый кэш.
	s.mu.Lock()
	startGen := s.feLoadGen
	mix := s.feMix
	gMix := s.gtcrnMix
	s.mu.Unlock()

	prewarmStart := time.Now()
	loaded := 0

	prewarmFE := func(size audio.FastEnhancerSize) {
		s.mu.Lock()
		_, alreadyCached := s.feCache[size]
		s.mu.Unlock()
		if alreadyCached {
			return
		}
		fe, err := audio.NewFastEnhancer(size, s.log)
		if err != nil {
			s.log.Warn("prewarm FE failed", slog.String("size", string(size)), slog.String("error", err.Error()))
			return
		}
		fe.SetMix(mix)
		s.mu.Lock()
		// Discard ТОЛЬКО если Shutdown поднял gen (s.feCache был пересоздан).
		// Активность сессии не проверяем — кэш живёт всю жизнь процесса,
		// чтобы первый Join не упирался в холодный ORT NewSession.
		if s.feLoadGen != startGen {
			s.mu.Unlock()
			fe.Close()
			return
		}
		s.feCache[size] = fe
		s.mu.Unlock()
		loaded++
	}

	prewarmGTCRN := func(variant audio.GTCRNVariant) {
		s.mu.Lock()
		_, alreadyCached := s.gtcrnCache[variant]
		s.mu.Unlock()
		if alreadyCached {
			return
		}
		g, err := audio.NewGTCRN(variant, s.log)
		if err != nil {
			s.log.Warn("prewarm GTCRN failed", slog.String("variant", string(variant)), slog.String("error", err.Error()))
			return
		}
		g.SetMix(gMix)
		s.mu.Lock()
		if s.feLoadGen != startGen {
			s.mu.Unlock()
			g.Close()
			return
		}
		s.gtcrnCache[variant] = g
		s.mu.Unlock()
		loaded++
	}

	// Порядок: лёгкие → тяжёлые. Если юзер быстро переключает в UI, лёгкие
	// быстрее окажутся в кэше.
	for _, sz := range []audio.FastEnhancerSize{audio.FETiny, audio.FEBase, audio.FESmall, audio.FEMedium, audio.FELarge} {
		prewarmFE(sz)
	}
	for _, v := range []audio.GTCRNVariant{audio.GTCRNDNS3, audio.GTCRNSimple} {
		prewarmGTCRN(v)
	}

	s.log.Info("neural NS prewarm complete",
		slog.Int("loaded", loaded),
		slog.Duration("took", time.Since(prewarmStart)))
}

// ApplyMicGain обновляет mic gain multiplier на текущем live capture голосового
// канала. Вызывается из AudioService.SetMicGain когда пользователь двигает
// слайдер во время активного звонка — иначе изменение применилось бы только
// после leave/rejoin. SetGain в C-коде меняет только volatile float поле,
// race-safe без lock.
func (s *VoiceService) ApplyMicGain(gain float32) {
	s.mu.Lock()
	cap := s.capture
	s.mu.Unlock()
	if cap == nil {
		return
	}
	cap.SetGain(gain)
	s.log.Info("mic gain applied to live capture",
		slog.Float64("gain", float64(gain)))
}
