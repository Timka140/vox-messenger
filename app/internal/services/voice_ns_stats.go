package services

import (
	"fmt"
	"log/slog"
	"math"
	"vox_app/internal/audio"
	"os"
	"time"
)

// NS A/B stats CSV log — диагностика "VAD ест согласные".
//
// Активен ТОЛЬКО при voiceDebug == true (build tag nx_voicedebug). Без тега
// все вызовы здесь — no-op'ы compile-time eliminated.
//
// Файл пишется рядом с vox-app.log:
//
//	vox-ns-stats.log    — текущий звонок
//	vox-ns-stats.log.1  — предыдущий
//	vox-ns-stats.log.2..4
//
// На каждый JoinGroupVoice делается ротация (.N → .N+1, .4 удаляется),
// затем создаётся новый файл с CSV-заголовком. Закрывается в leaveInternal.
//
// Формат CSV — одна строка каждые 200ms (10 фреймов audioLoop). Поля:
//
//	t_ms              — миллисекунды от начала звонка
//	total             — фреймов в интервале (≈10)
//	silent_n          — фреймов класса silent (dry_rms < 100)
//	silent_heavy      — из них с ratio < 0.1 (NS убил тишину — хорошо)
//	silent_avg_db     — средняя attenuation в dB по классу
//	soft_n            — soft (100..800), сюда падают согласные/слоги
//	soft_heavy        — из них с ratio < 0.3 (NS убил часть слова — ПЛОХО)
//	soft_avg_db       — средняя attenuation
//	loud_n            — loud (≥800), гласные/громкая речь
//	loud_heavy        — из них с ratio < 0.5 (NS убил гласную — очень плохо)
//	loud_avg_db
//
// Пустая ячейка avg_db = в классе не было фреймов.

const (
	nsStatsLogName    = "vox-ns-stats.log"
	nsStatsLogKeep    = 4 // держим current + .1..4 = 5 файлов max
	nsStatsLogPeriod  = 10 // тиков audioLoop между записями (10 × 20ms = 200ms)
)

// rotateNSStatsLogFiles делает shift .N → .N+1, удаляет .keep, освобождая
// место для нового current. Идемпотентна — отсутствующие файлы пропускаются.
func rotateNSStatsLogFiles() {
	// Удаляем самый старый
	oldest := fmt.Sprintf("%s.%d", nsStatsLogName, nsStatsLogKeep)
	_ = os.Remove(oldest)
	// Сдвигаем .N → .N+1 с конца
	for i := nsStatsLogKeep - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", nsStatsLogName, i)
		dst := fmt.Sprintf("%s.%d", nsStatsLogName, i+1)
		if _, err := os.Stat(src); err == nil {
			_ = os.Rename(src, dst)
		}
	}
	// current → .1
	if _, err := os.Stat(nsStatsLogName); err == nil {
		_ = os.Rename(nsStatsLogName, nsStatsLogName+".1")
	}
}

// openNSStatsLog ротирует и открывает новый CSV-файл для звонка. Пишет
// шапку с метаданными и колонками. Возвращает *os.File и время старта.
// При ошибке возвращает nil — caller должен это переносить молча.
func openNSStatsLog(log *slog.Logger, nsLevel audio.NoiseSuppressionLevel) (*os.File, time.Time) {
	rotateNSStatsLogFiles()
	f, err := os.OpenFile(nsStatsLogName, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		log.Warn("ns-stats log open failed", slog.String("error", err.Error()))
		return nil, time.Time{}
	}
	start := time.Now()
	fmt.Fprintf(f, "# NS A/B stats\n")
	fmt.Fprintf(f, "# start: %s\n", start.Format(time.RFC3339))
	fmt.Fprintf(f, "# ns_level: %d (%s)\n", int(nsLevel), nsLevelName(nsLevel))
	fmt.Fprintf(f, "# interval_ms: %d\n", nsStatsLogPeriod*20)
	fmt.Fprintf(f, "# silent: dry_rms<100 (тишина); soft: 100..800 (согласные); loud: ≥800 (гласные)\n")
	fmt.Fprintf(f, "# heavy thresholds: silent ratio<0.1; soft ratio<0.3; loud ratio<0.5\n")
	fmt.Fprintf(f, "t_ms,total,silent_n,silent_heavy,silent_avg_db,soft_n,soft_heavy,soft_avg_db,loud_n,loud_heavy,loud_avg_db\n")
	log.Info("ns-stats log opened",
		slog.String("file", nsStatsLogName),
		slog.Int("ns_level", int(nsLevel)))
	return f, start
}

// closeNSStatsLog безопасно закрывает файл (nil-tolerant).
func closeNSStatsLog(f *os.File) {
	if f != nil {
		_ = f.Close()
	}
}

// writeNSStatsRow форматирует одну CSV-строку из NSStats и пишет в файл.
// avg в dB вычисляется из sum_ratio/count: 20*log10(avg_ratio). Пустые
// классы выводятся как пустые ячейки, не "0" — иначе график потом ложно
// показывает "−inf dB" в местах где данных нет.
func writeNSStatsRow(f *os.File, start time.Time, st audio.NSStats) {
	if f == nil {
		return
	}
	tMs := time.Since(start).Milliseconds()
	silentDB := avgRatioToDB(st.SilentSumRatio, st.SilentN)
	softDB := avgRatioToDB(st.SoftSumRatio, st.SoftN)
	loudDB := avgRatioToDB(st.LoudSumRatio, st.LoudN)
	fmt.Fprintf(f, "%d,%d,%d,%d,%s,%d,%d,%s,%d,%d,%s\n",
		tMs,
		st.TotalFrames,
		st.SilentN, st.SilentHeavy, silentDB,
		st.SoftN, st.SoftHeavy, softDB,
		st.LoudN, st.LoudHeavy, loudDB,
	)
}

func avgRatioToDB(sumRatio float64, n int64) string {
	if n == 0 {
		return ""
	}
	avg := sumRatio / float64(n)
	if avg <= 0 {
		return "-inf"
	}
	db := 20 * math.Log10(avg)
	return fmt.Sprintf("%.1f", db)
}

func nsLevelName(lvl audio.NoiseSuppressionLevel) string {
	switch lvl {
	case audio.NSOff:
		return "OFF"
	case audio.NSLight:
		return "LIGHT"
	case audio.NSRNN:
		return "RNN"
	case audio.NSDeep:
		return "DEEP"
	default:
		return "?"
	}
}
