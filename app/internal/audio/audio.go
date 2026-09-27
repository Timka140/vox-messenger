// Package audio provides cross-platform audio device enumeration and capture/playback
// via miniaudio (WASAPI on Windows, CoreAudio on macOS, PulseAudio/ALSA on Linux).
package audio

/*
#cgo CFLAGS: -std=c11
#cgo linux CFLAGS: -D_GNU_SOURCE
#cgo linux LDFLAGS: -lpthread -lm -ldl
#cgo darwin LDFLAGS: -framework CoreAudio -framework AudioToolbox -framework CoreFoundation
#cgo windows LDFLAGS: -lole32

#include "miniaudio.h"
#include <stdlib.h>
#include <string.h>
#include <stdio.h>
#include <stdint.h>
#include <math.h>

// --- Кросс-платформенный монотонный таймер в наносекундах ---
// Windows: QueryPerformanceCounter (нет зависимости от winpthread).
// POSIX: clock_gettime(CLOCK_MONOTONIC).
#ifdef _WIN32
#include <windows.h>
static inline int64_t nx_now_ns(void) {
    static LARGE_INTEGER freq = {0};
    if (freq.QuadPart == 0) QueryPerformanceFrequency(&freq);
    LARGE_INTEGER c;
    QueryPerformanceCounter(&c);
    // (c / freq) * 1e9 без overflow: делим на freq заранее, потом умножаем.
    return (int64_t)((c.QuadPart * 1000000000LL) / freq.QuadPart);
}
#else
#include <time.h>
static inline int64_t nx_now_ns(void) {
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return (int64_t)t.tv_sec * 1000000000LL + (int64_t)t.tv_nsec;
}
#endif

// --- Noise suppression ---
//
// Многоуровневый шумодав. Pipeline в callback захвата:
//   in[48k f32] → HPF 80Hz (биквад) → gain → 480-sample accumulator →
//                 [optional RNNoise frame process] → LPF 7kHz (биквад) →
//                 ÷3 децимация → 16k int16 → ring buffer
//
// Уровни (volatile int ns_level, читается audio thread, пишется Wails thread):
//   0 = off    — legacy путь: gain → naive decimate (alias-генератор, для A/B)
//   1 = light  — HPF + gain + LPF + decimate (всегда работает, даже без RNNoise)
//   2 = rnn    — light + RNNoise (требует -DNX_USE_RNNOISE и вендоринга xiph/rnnoise)
//   3 = deep   — резерв под DeepFilterNet (пока fallback на rnn/light)
//
// Frame size = 480 samples = 10 ms @ 48 kHz, ровно то, что хочет RNNoise.

#define NX_NS_OFF   0
#define NX_NS_LIGHT 1
#define NX_NS_RNN   2
#define NX_NS_DEEP  3
#define NX_NS_FRAME 480
#define NX_DECIMATE 3   // 48000 / 16000

typedef struct {
    float b0, b1, b2, a1, a2;
    float z1, z2;
} nx_biquad;

// RBJ cookbook: HPF biquad. fs = sample rate, fc = cutoff Hz, Q = 0.707 (Butterworth).
static void nx_biquad_set_hpf(nx_biquad* f, float fc, float fs, float Q) {
    float w = 2.0f * 3.14159265358979f * fc / fs;
    float cw = cosf(w);
    float sw = sinf(w);
    float alpha = sw / (2.0f * Q);
    float a0 = 1.0f + alpha;
    f->b0 = ((1.0f + cw) * 0.5f) / a0;
    f->b1 = -(1.0f + cw) / a0;
    f->b2 = ((1.0f + cw) * 0.5f) / a0;
    f->a1 = (-2.0f * cw) / a0;
    f->a2 = (1.0f - alpha) / a0;
    f->z1 = 0.0f; f->z2 = 0.0f;
}

// RBJ cookbook: LPF biquad. Используется как anti-alias перед децимацией ÷3 (Nyquist 8 kHz).
static void nx_biquad_set_lpf(nx_biquad* f, float fc, float fs, float Q) {
    float w = 2.0f * 3.14159265358979f * fc / fs;
    float cw = cosf(w);
    float sw = sinf(w);
    float alpha = sw / (2.0f * Q);
    float a0 = 1.0f + alpha;
    f->b0 = ((1.0f - cw) * 0.5f) / a0;
    f->b1 = (1.0f - cw) / a0;
    f->b2 = ((1.0f - cw) * 0.5f) / a0;
    f->a1 = (-2.0f * cw) / a0;
    f->a2 = (1.0f - alpha) / a0;
    f->z1 = 0.0f; f->z2 = 0.0f;
}

// Transposed Direct Form II — численно стабильна для float32.
static inline float nx_biquad_process(nx_biquad* f, float x) {
    float y = f->b0 * x + f->z1;
    f->z1 = f->b1 * x - f->a1 * y + f->z2;
    f->z2 = f->b2 * x - f->a2 * y;
    return y;
}

// --- RNNoise hook (опциональный, build tag nx_rnnoise) ---
//
// Чтобы включить: положить исходники xiph/rnnoise в app/internal/audio/rnnoise/,
// собрать с тегом `-tags nx_rnnoise` (см. audio_rnnoise.go).
// Без тега — функции no-op, NX_NS_RNN деградирует до NX_NS_LIGHT.

#ifdef NX_USE_RNNOISE
#include "rnnoise/include/rnnoise.h"
static void* nx_rnnoise_create(void) { return rnnoise_create(NULL); }
static void  nx_rnnoise_destroy(void* st) { if (st) rnnoise_destroy((DenoiseState*)st); }
static void  nx_rnnoise_process_frame(void* st, float* frame_480_f32) {
    // RNNoise ожидает диапазон [-32768, 32767], не [-1, 1].
    float scaled[NX_NS_FRAME];
    for (int i = 0; i < NX_NS_FRAME; i++) scaled[i] = frame_480_f32[i] * 32768.0f;
    rnnoise_process_frame((DenoiseState*)st, scaled, scaled);
    for (int i = 0; i < NX_NS_FRAME; i++) frame_480_f32[i] = scaled[i] * (1.0f / 32768.0f);
}
static int   nx_rnnoise_available(void) { return 1; }
#else
static void* nx_rnnoise_create(void) { return NULL; }
static void  nx_rnnoise_destroy(void* st) { (void)st; }
static void  nx_rnnoise_process_frame(void* st, float* frame) { (void)st; (void)frame; }
static int   nx_rnnoise_available(void) { return 0; }
#endif

// --- DeepFilterNet3 hook (опциональный, build tag nx_dfn) ---
//
// Линкуется со staticlib third_party/libdf (см. audio_dfn.go). Без тега — функции
// no-op, NX_NS_DEEP деградирует до NX_NS_RNN (если RNNoise доступна).
//
// df_process_frame НЕ in-place safe: upstream читает input через ArrayView2
// и пишет в output через ArrayViewMut2, один буфер для обоих даст UB aliasing.
// Поэтому держим отдельный scratch ns_frame_out[NX_NS_FRAME].
//
// Модель грузится из .tar.gz файла через df_create. Загрузка/выгрузка
// выполняется ТОЛЬКО вне RT-контекста (из Go до Start или при SetNS другом).

#ifdef NX_USE_DFN
// Forward-declare C ABI из libnx_df.a (third_party/libdf/include/nx_df.h).
typedef struct DFState DFState;
extern DFState* df_create(const char* path, float atten_lim);
extern size_t   df_get_frame_length(DFState* st);
extern float    df_process_frame(DFState* st, float* input, float* output);
extern void     df_set_atten_lim(DFState* st, float lim_db);
extern void     df_set_post_filter_beta(DFState* st, float beta);
extern void     df_free(DFState* st);

static void* nx_dfn_create(const char* model_path) {
    // atten_lim — максимальное attenuation в dB которое DFN3 может применить
    // к фрейму. Это нативный параметр DeepFilterNet, реализованный в libDF на
    // уровне gain computation: любой bin gain clampится так, чтобы суммарное
    // ослабление не превышало lim_db.
    //
    //   100.0f → None в upstream = unlimited (модель может давить до -inf)
    //                              → AGRESSIVE, ест согласные/слоги/гласные
    //    30.0f → max -30 dB       → умеренно агрессивный
    //    12.0f → max -12 dB       → мягкий, баланс шум/речь (~WebRTC ANR)
    //     6.0f → max  -6 dB       → очень мягкий
    //     0.0f → passthrough (НЕ передавать!)
    //
    // Дефолт при первом создании — 100 (= None/unlimited в upstream).
    // Runtime значение перезаписывается через df_set_atten_lim сразу после
    // загрузки модели (см. voice.go JoinGroupVoice — читает settings).
    return (void*)df_create(model_path, 100.0f);
}
static void nx_dfn_destroy(void* st) { if (st) df_free((DFState*)st); }
static void nx_dfn_process_frame(void* st, float* in_480, float* out_480) {
    (void)df_process_frame((DFState*)st, in_480, out_480);
}
static int  nx_dfn_available(void) { return 1; }
#else
static void* nx_dfn_create(const char* path) { (void)path; return NULL; }
static void  nx_dfn_destroy(void* st) { (void)st; }
static void  nx_dfn_process_frame(void* st, float* in_480, float* out_480) {
    (void)st;
    // No-op: скопировать вход в выход, чтобы pipeline остался консистентным.
    for (int i = 0; i < NX_NS_FRAME; i++) out_480[i] = in_480[i];
}
static int   nx_dfn_available(void) { return 0; }
#endif

// --- Device enumeration ---

typedef struct {
    char name[256];
    char id_str[64];
    int is_default;
} nx_device_info;

typedef struct {
    nx_device_info* devices;
    int count;
} nx_device_list;

static nx_device_list nx_list_devices(int type) {
    nx_device_list result = {0};

    ma_context ctx;
    if (ma_context_init(NULL, 0, NULL, &ctx) != MA_SUCCESS) {
        return result;
    }

    ma_device_info* infos;
    ma_uint32 count;
    ma_device_info* defaultInfo = NULL; // unused but required
    ma_uint32 defaultCount = 0;

    ma_result r;
    if (type == 0) { // capture
        r = ma_context_get_devices(&ctx, NULL, NULL, &infos, &count);
    } else { // playback
        r = ma_context_get_devices(&ctx, &infos, &count, NULL, NULL);
    }

    if (r != MA_SUCCESS || count == 0) {
        ma_context_uninit(&ctx);
        return result;
    }

    result.devices = (nx_device_info*)calloc(count, sizeof(nx_device_info));
    result.count = (int)count;

    for (ma_uint32 i = 0; i < count; i++) {
        strncpy(result.devices[i].name, infos[i].name, 255);
        result.devices[i].name[255] = '\0';
        snprintf(result.devices[i].id_str, 64, "%u", i);
        result.devices[i].is_default = infos[i].isDefault ? 1 : 0;
    }

    ma_context_uninit(&ctx);
    return result;
}

static void nx_free_device_list(nx_device_list* list) {
    if (list->devices) {
        free(list->devices);
        list->devices = NULL;
        list->count = 0;
    }
}

// --- Capture (microphone) ---

// Recording buffer: 16kHz 16-bit mono PCM (voice quality, ~32KB/sec)
#define REC_SAMPLE_RATE 16000
#define REC_MAX_SECONDS 1800
#define REC_BUF_SIZE (REC_SAMPLE_RATE * REC_MAX_SECONDS)

typedef struct {
    ma_device device;
    ma_context context;
    float gain;
    float peak_level;
    int active;
    int initialized;
    // Recording buffer (16-bit PCM at 16kHz for voice messages)
    int16_t* rec_buf;
    volatile int rec_pos;       // current write position in samples
    int recording;              // 1 = recording to buffer
    volatile int rec_full;      // 1 = buffer full, recording auto-stopped
    int stream_pos;             // read position for streaming (voice chat)
    volatile int buf_lock;      // spinlock: 0=free, 1=locked
    // --- Noise suppression state ---
    volatile int ns_level;      // NX_NS_OFF/LIGHT/RNN/DEEP, безопасно меняется из Go
    nx_biquad hpf;              // 80 Hz high-pass, режет rumble/DC/гудение
    nx_biquad lpf;              // 7 kHz low-pass, anti-alias перед децимацией ÷3
    float ns_frame[NX_NS_FRAME];// 10ms accumulator @ 48k для frame-based NS
    float ns_frame_out[NX_NS_FRAME]; // scratch для DFN3 (in/out должны быть разными буферами)
    int ns_frame_pos;
    int decim_phase;            // персистентная фаза децимации, не сбрасывается между фреймами
    void* ns_rnnoise;           // DenoiseState* или NULL если не скомпилирован
    void* ns_df;                // DFState* (nx_dfn). NULL если модель не загружена или тег выключен.
    // --- DFN perf counters (диагностика latency) ---
    volatile int64_t dfn_frame_count;    // сколько DFN-фреймов обработано
    volatile int64_t dfn_total_ns;       // суммарное время в наносекундах
    volatile int64_t dfn_max_ns;         // максимум одного фрейма
    volatile int64_t dfn_over_budget;    // сколько раз DFN превысил 10 мс бюджет
    // --- NS A/B effectiveness counters ---
    // Считаем dry_rms (до NS) и wet_rms (после NS) на каждом 10ms фрейме,
    // классифицируем по dry_rms (silent <100, soft 100..800, loud ≥800),
    // суммируем ratio = wet/dry. Go-сторона забирает через nx_capture_ns_stats_take()
    // и пишет CSV в vox-ns-stats.log. Диагностика "VAD ест согласные" — если
    // soft heavy% (ratio<0.3) высокое, NS режет тихую речь.
    volatile int64_t ns_total_frames;
    volatile int64_t ns_silent_n;        // dry_rms < 100
    volatile int64_t ns_silent_heavy;    // и ratio < 0.1 (хорошо: тишина задавлена)
    volatile double  ns_silent_sum_ratio;
    volatile int64_t ns_soft_n;          // 100 ≤ dry_rms < 800
    volatile int64_t ns_soft_heavy;      // и ratio < 0.3 (плохо: убитая согласная)
    volatile double  ns_soft_sum_ratio;
    volatile int64_t ns_loud_n;          // dry_rms ≥ 800
    volatile int64_t ns_loud_heavy;      // и ratio < 0.5 (очень плохо: убитая гласная)
    volatile double  ns_loud_sum_ratio;
} nx_capture;

typedef struct {
    int64_t total_frames;
    int64_t silent_n;
    int64_t silent_heavy;
    double  silent_sum_ratio;
    int64_t soft_n;
    int64_t soft_heavy;
    double  soft_sum_ratio;
    int64_t loud_n;
    int64_t loud_heavy;
    double  loud_sum_ratio;
} nx_ns_stats;

static void nx_lock(nx_capture* cap) {
    while (__sync_lock_test_and_set(&cap->buf_lock, 1)) {}
}
static void nx_unlock(nx_capture* cap) {
    __sync_lock_release(&cap->buf_lock);
}

// Записать один 16k int16 sample в recording buffer (под спинлоком).
// Возвращает 0 при переполнении (recording = автостоп).
static inline void nx_write_rec_sample(nx_capture* cap, float v) {
    if (v > 1.0f) v = 1.0f;
    if (v < -1.0f) v = -1.0f;
    int16_t s = (int16_t)(v * 32767.0f);
    nx_lock(cap);
    if (cap->rec_pos < REC_BUF_SIZE) {
        cap->rec_buf[cap->rec_pos++] = s;
    } else {
        cap->recording = 0;
        cap->rec_full = 1;
    }
    nx_unlock(cap);
}

static void nx_capture_callback(ma_device* device, void* output, const void* input, ma_uint32 frameCount) {
    nx_capture* cap = (nx_capture*)device->pUserData;
    if (!cap || !cap->active || !input) return;

    const float* samples = (const float*)input;
    int level = cap->ns_level;
    float gain = cap->gain;
    float peak = 0.0f;

    // --- NS_OFF: legacy путь, голая децимация без фильтров (для A/B-теста). ---
    if (level == NX_NS_OFF) {
        for (ma_uint32 i = 0; i < frameCount; i++) {
            float v = samples[i] * gain;
            float a = v < 0 ? -v : v;
            if (a > peak) peak = a;
            if (cap->recording) {
                if (cap->decim_phase == 0) {
                    nx_write_rec_sample(cap, v);
                }
                cap->decim_phase++;
                if (cap->decim_phase >= NX_DECIMATE) cap->decim_phase = 0;
            }
        }
        cap->peak_level = peak;
        return;
    }

    // --- NS_LIGHT / NS_RNN / NS_DEEP: HPF → gain → 10ms frame → [RNN] → LPF → ÷3 ---
    for (ma_uint32 i = 0; i < frameCount; i++) {
        float v = nx_biquad_process(&cap->hpf, samples[i]) * gain;
        cap->ns_frame[cap->ns_frame_pos++] = v;
        if (cap->ns_frame_pos < NX_NS_FRAME) continue;

        // === NS A/B: dry_rms ДО прогона модели (для stats counters) ===
        // DFN3 ограничен atten_lim=12 dB на уровне модели (см. nx_dfn_create),
        // RNNoise сам по себе не агрессивен → wet/dry mix больше не нужен.
        // Оставляем только замер dry_rms для классификации фреймов в stats.
        double sum_sq_dry = 0.0;
        for (int j = 0; j < NX_NS_FRAME; j++) {
            double s = (double)cap->ns_frame[j];
            sum_sq_dry += s * s;
        }
        float dry_rms = (float)(sqrt(sum_sq_dry / NX_NS_FRAME) * 32768.0);

        // Полный 10ms фрейм @ 48 kHz готов — прогоняем через NS-модель.
        // NX_NS_DEEP с загруженной DFN моделью → DFN3 (самое сильное).
        // NX_NS_DEEP без модели или NX_NS_RNN → RNNoise (если доступен).
        if (level == NX_NS_DEEP && cap->ns_df != NULL) {
            int64_t t0 = nx_now_ns();
            nx_dfn_process_frame(cap->ns_df, cap->ns_frame, cap->ns_frame_out);
            int64_t dt_ns = nx_now_ns() - t0;
            cap->dfn_frame_count++;
            cap->dfn_total_ns += dt_ns;
            if (dt_ns > cap->dfn_max_ns) cap->dfn_max_ns = dt_ns;
            if (dt_ns > 10000000LL) cap->dfn_over_budget++; // >10 ms = не укладываемся
            for (int j = 0; j < NX_NS_FRAME; j++) cap->ns_frame[j] = cap->ns_frame_out[j];
        } else if ((level == NX_NS_RNN || level == NX_NS_DEEP) && cap->ns_rnnoise != NULL) {
            nx_rnnoise_process_frame(cap->ns_rnnoise, cap->ns_frame);
        }

        // === NS A/B: wet_rms ПОСЛЕ прогона + классификация по dry_rms ===
        // (Для NS_LIGHT NS-модель не запускалась → dry_rms == wet_rms, ratio == 1.)
        double sum_sq_wet = 0.0;
        for (int j = 0; j < NX_NS_FRAME; j++) {
            double s = (double)cap->ns_frame[j];
            sum_sq_wet += s * s;
        }
        float wet_rms = (float)(sqrt(sum_sq_wet / NX_NS_FRAME) * 32768.0);
        double ratio = (dry_rms > 1.0f) ? ((double)wet_rms / (double)dry_rms) : 0.0;

        cap->ns_total_frames++;
        if (dry_rms < 100.0f) {
            cap->ns_silent_n++;
            cap->ns_silent_sum_ratio += ratio;
            if (ratio < 0.1) cap->ns_silent_heavy++;
        } else if (dry_rms < 800.0f) {
            cap->ns_soft_n++;
            cap->ns_soft_sum_ratio += ratio;
            if (ratio < 0.3) cap->ns_soft_heavy++;
        } else {
            cap->ns_loud_n++;
            cap->ns_loud_sum_ratio += ratio;
            if (ratio < 0.5) cap->ns_loud_heavy++;
        }

        // Output stage: LPF (anti-alias) → peak → ÷3 → запись.
        for (int j = 0; j < NX_NS_FRAME; j++) {
            float vv = nx_biquad_process(&cap->lpf, cap->ns_frame[j]);
            float a = vv < 0 ? -vv : vv;
            if (a > peak) peak = a;
            if (cap->recording) {
                if (cap->decim_phase == 0) {
                    nx_write_rec_sample(cap, vv);
                }
                cap->decim_phase++;
                if (cap->decim_phase >= NX_DECIMATE) cap->decim_phase = 0;
            }
        }
        cap->ns_frame_pos = 0;
    }
    cap->peak_level = peak;
}

static nx_capture* nx_capture_new(void) {
    nx_capture* cap = (nx_capture*)calloc(1, sizeof(nx_capture));
    cap->gain = 1.0f;
    cap->rec_buf = (int16_t*)calloc(REC_BUF_SIZE, sizeof(int16_t));
    // NS defaults: light on, биквады инициализированы под 48 kHz.
    cap->ns_level = NX_NS_LIGHT;
    nx_biquad_set_hpf(&cap->hpf, 80.0f,   48000.0f, 0.707f);
    nx_biquad_set_lpf(&cap->lpf, 7000.0f, 48000.0f, 0.707f);
    cap->ns_frame_pos = 0;
    cap->decim_phase = 0;
    cap->ns_rnnoise = nx_rnnoise_create(); // NULL без -DNX_USE_RNNOISE
    return cap;
}

static void nx_capture_set_ns_level(nx_capture* cap, int level) {
    if (!cap) return;
    if (level < 0 || level > NX_NS_DEEP) level = NX_NS_LIGHT;
    cap->ns_level = level;
    // Сбрасываем накопитель, чтобы при переключении не остался хвост от старого режима.
    cap->ns_frame_pos = 0;
}

static int nx_capture_rnnoise_available(void) {
    return nx_rnnoise_available();
}

// Загрузить DFN3 модель из .tar.gz. Вызывать ВНЕ RT-контекста (до Start или
// когда ns_level != NX_NS_DEEP). Возвращает 0 при успехе, != 0 при ошибке.
static int nx_capture_df_load(nx_capture* cap, const char* model_path) {
    if (!cap) return -1;
    if (cap->ns_df) {
        nx_dfn_destroy(cap->ns_df);
        cap->ns_df = NULL;
    }
    void* st = nx_dfn_create(model_path);
    if (!st) return -2;
    cap->ns_df = st;
    return 0;
}

static void nx_capture_df_unload(nx_capture* cap) {
    if (!cap) return;
    if (cap->ns_df) {
        nx_dfn_destroy(cap->ns_df);
        cap->ns_df = NULL;
    }
}

static int nx_capture_df_available(void) {
    return nx_dfn_available();
}

static int nx_capture_df_loaded(nx_capture* cap) {
    return (cap && cap->ns_df) ? 1 : 0;
}

// Runtime обновление DFN3 atten_lim. Ноль — passthrough (не передавать),
// 6..30 — разумный диапазон, 100 — unlimited (agressive). No-op если модель
// не загружена или бинарь собран без tag nx_dfn.
static void nx_capture_df_set_atten_lim(nx_capture* cap, float lim_db) {
#ifdef NX_USE_DFN
    if (!cap || !cap->ns_df) return;
    df_set_atten_lim((DFState*)cap->ns_df, lim_db);
#else
    (void)cap; (void)lim_db;
#endif
}

// Снимок DFN perf counters + reset. Возвращает через out-указатели.
// frames = сколько фреймов обработано за период, avg_ns/max_ns = время в нс,
// over_budget = сколько раз один фрейм превысил 10 мс (RT-бюджет 48 kHz @ 10 ms hop).
static void nx_capture_df_stats_take(nx_capture* cap,
                                     int64_t* out_frames,
                                     int64_t* out_avg_ns,
                                     int64_t* out_max_ns,
                                     int64_t* out_over_budget) {
    if (!cap) {
        *out_frames = 0; *out_avg_ns = 0; *out_max_ns = 0; *out_over_budget = 0;
        return;
    }
    int64_t frames = cap->dfn_frame_count;
    int64_t total  = cap->dfn_total_ns;
    int64_t maxns  = cap->dfn_max_ns;
    int64_t over   = cap->dfn_over_budget;
    cap->dfn_frame_count = 0;
    cap->dfn_total_ns = 0;
    cap->dfn_max_ns = 0;
    cap->dfn_over_budget = 0;
    *out_frames = frames;
    *out_avg_ns = frames > 0 ? (total / frames) : 0;
    *out_max_ns = maxns;
    *out_over_budget = over;
}

// Снимок NS A/B-счётчиков + reset. Один писатель (callback thread), один
// читатель (Go audioLoop) — несинхронизированный read-modify-write приемлем
// для диагностики (можем потерять учёт нескольких фреймов на стыке reset'а).
static void nx_capture_ns_stats_take(nx_capture* cap, nx_ns_stats* out) {
    if (!out) return;
    if (!cap) {
        out->total_frames = 0;
        out->silent_n = 0; out->silent_heavy = 0; out->silent_sum_ratio = 0.0;
        out->soft_n = 0;   out->soft_heavy = 0;   out->soft_sum_ratio = 0.0;
        out->loud_n = 0;   out->loud_heavy = 0;   out->loud_sum_ratio = 0.0;
        return;
    }
    out->total_frames     = cap->ns_total_frames;
    out->silent_n         = cap->ns_silent_n;
    out->silent_heavy     = cap->ns_silent_heavy;
    out->silent_sum_ratio = cap->ns_silent_sum_ratio;
    out->soft_n           = cap->ns_soft_n;
    out->soft_heavy       = cap->ns_soft_heavy;
    out->soft_sum_ratio   = cap->ns_soft_sum_ratio;
    out->loud_n           = cap->ns_loud_n;
    out->loud_heavy       = cap->ns_loud_heavy;
    out->loud_sum_ratio   = cap->ns_loud_sum_ratio;

    cap->ns_total_frames     = 0;
    cap->ns_silent_n         = 0;
    cap->ns_silent_heavy     = 0;
    cap->ns_silent_sum_ratio = 0.0;
    cap->ns_soft_n           = 0;
    cap->ns_soft_heavy       = 0;
    cap->ns_soft_sum_ratio   = 0.0;
    cap->ns_loud_n           = 0;
    cap->ns_loud_heavy       = 0;
    cap->ns_loud_sum_ratio   = 0.0;
}

static int nx_capture_start(nx_capture* cap, int device_index) {
    if (cap->active) return 0;

    if (ma_context_init(NULL, 0, NULL, &cap->context) != MA_SUCCESS) {
        return -1;
    }

    ma_device_info* infos;
    ma_uint32 count;
    if (ma_context_get_devices(&cap->context, NULL, NULL, &infos, &count) != MA_SUCCESS) {
        ma_context_uninit(&cap->context);
        return -2;
    }

    ma_device_config config = ma_device_config_init(ma_device_type_capture);
    config.capture.format = ma_format_f32;
    config.capture.channels = 1;
    config.sampleRate = 48000;
    config.dataCallback = nx_capture_callback;
    config.pUserData = cap;

    if (device_index >= 0 && (ma_uint32)device_index < count) {
        config.capture.pDeviceID = &infos[device_index].id;
    }

    if (ma_device_init(&cap->context, &config, &cap->device) != MA_SUCCESS) {
        ma_context_uninit(&cap->context);
        return -3;
    }

    if (ma_device_start(&cap->device) != MA_SUCCESS) {
        ma_device_uninit(&cap->device);
        ma_context_uninit(&cap->context);
        return -4;
    }

    cap->active = 1;
    cap->initialized = 1;
    return 0;
}

static void nx_capture_stop(nx_capture* cap) {
    if (!cap->active) return;
    cap->active = 0;        // mark inactive BEFORE stopping (prevents race with callback/peak reads)
    cap->recording = 0;
    ma_device_stop(&cap->device);
    ma_device_uninit(&cap->device);
    ma_context_uninit(&cap->context);
    cap->peak_level = 0.0f;
}

static void nx_capture_free(nx_capture* cap) {
    if (!cap) return;
    nx_capture_stop(cap);
    if (cap->ns_rnnoise) {
        nx_rnnoise_destroy(cap->ns_rnnoise);
        cap->ns_rnnoise = NULL;
    }
    if (cap->ns_df) {
        nx_dfn_destroy(cap->ns_df);
        cap->ns_df = NULL;
    }
    if (cap->rec_buf) {
        free(cap->rec_buf);
        cap->rec_buf = NULL;
    }
    free(cap);
}

static void nx_capture_set_gain(nx_capture* cap, float gain) {
    if (cap) cap->gain = gain;
}

static float nx_capture_get_peak(nx_capture* cap) {
    if (!cap || !cap->active) return 0.0f;
    float p = cap->peak_level;
    cap->peak_level = 0.0f;
    return p;
}

static void nx_capture_start_recording(nx_capture* cap) {
    if (!cap) return;
    cap->rec_pos = 0;
    cap->rec_full = 0;
    cap->recording = 1;
}

static void nx_capture_stop_recording(nx_capture* cap) {
    if (!cap) return;
    cap->recording = 0;
}

static int nx_capture_rec_samples(nx_capture* cap) {
    if (!cap) return 0;
    return cap->rec_pos;
}

static int nx_capture_rec_full(nx_capture* cap) {
    if (!cap) return 0;
    return cap->rec_full;
}

// Build a WAV file in memory from the recorded PCM buffer.
// Returns malloc'd buffer (caller must free) and sets *out_size.
static void* nx_capture_get_wav(nx_capture* cap, int* out_size) {
    if (!cap || cap->rec_pos == 0) {
        *out_size = 0;
        return NULL;
    }
    int data_size = cap->rec_pos * 2; // 16-bit = 2 bytes per sample
    int wav_size = 44 + data_size;
    unsigned char* wav = (unsigned char*)malloc(wav_size);

    // RIFF header
    memcpy(wav, "RIFF", 4);
    *(uint32_t*)(wav + 4) = (uint32_t)(wav_size - 8);
    memcpy(wav + 8, "WAVE", 4);
    // fmt chunk
    memcpy(wav + 12, "fmt ", 4);
    *(uint32_t*)(wav + 16) = 16;             // chunk size
    *(uint16_t*)(wav + 20) = 1;              // PCM format
    *(uint16_t*)(wav + 22) = 1;              // mono
    *(uint32_t*)(wav + 24) = REC_SAMPLE_RATE;// sample rate
    *(uint32_t*)(wav + 28) = REC_SAMPLE_RATE * 2; // byte rate
    *(uint16_t*)(wav + 32) = 2;              // block align
    *(uint16_t*)(wav + 34) = 16;             // bits per sample
    // data chunk
    memcpy(wav + 36, "data", 4);
    *(uint32_t*)(wav + 40) = (uint32_t)data_size;
    memcpy(wav + 44, cap->rec_buf, data_size);

    *out_size = wav_size;
    return wav;
}
// Start streaming mode: reset stream read position to current write position.
static void nx_capture_stream_start(nx_capture* cap) {
    if (!cap) return;
    cap->stream_pos = cap->rec_pos;
    cap->rec_pos = 0;
    cap->recording = 1;
}

// Stop streaming mode.
static void nx_capture_stream_stop(nx_capture* cap) {
    if (!cap) return;
    cap->recording = 0;
    cap->stream_pos = 0;
}

// Read available samples since last stream read.
// Returns number of samples copied into out_buf (max out_max samples).
static int nx_capture_stream_read(nx_capture* cap, int16_t* out_buf, int out_max) {
    if (!cap || !cap->recording) return 0;
    nx_lock(cap);
    int avail = cap->rec_pos - cap->stream_pos;
    if (avail <= 0) { nx_unlock(cap); return 0; }
    if (avail > out_max) avail = out_max;
    memcpy(out_buf, cap->rec_buf + cap->stream_pos, avail * sizeof(int16_t));
    cap->stream_pos += avail;
    // Reset buffer when we've consumed most of it to avoid overflow
    if (cap->stream_pos > REC_BUF_SIZE / 2) {
        int remaining = cap->rec_pos - cap->stream_pos;
        if (remaining > 0) {
            memmove(cap->rec_buf, cap->rec_buf + cap->stream_pos, remaining * sizeof(int16_t));
        }
        cap->rec_pos = remaining > 0 ? remaining : 0;
        cap->stream_pos = 0;
    }
    nx_unlock(cap);
    return avail;
}

// --- Playback (speaker) ---

#define PB_SAMPLE_RATE 16000
#define PB_BUF_SIZE (PB_SAMPLE_RATE * 2) // 2 sec ring buffer

typedef struct {
    ma_device device;
    ma_context context;
    int active;
    int16_t* buf;
    volatile int write_pos;
    volatile int read_pos;
    volatile int buf_lock;
    float volume; // 0.0 - 1.4+
    int upsample_phase; // 0..2: persistent phase counter for 16kHz→48kHz upsampling
} nx_playback;

static void nx_pb_lock(nx_playback* pb) {
    while (__sync_lock_test_and_set(&pb->buf_lock, 1)) {}
}
static void nx_pb_unlock(nx_playback* pb) {
    __sync_lock_release(&pb->buf_lock);
}

static void nx_playback_callback(ma_device* device, void* output, const void* input, ma_uint32 frameCount) {
    nx_playback* pb = (nx_playback*)device->pUserData;
    if (!pb || !pb->active) {
        memset(output, 0, frameCount * sizeof(float));
        return;
    }
    float* out = (float*)output;
    nx_pb_lock(pb);
    for (ma_uint32 i = 0; i < frameCount; i++) {
        if (pb->read_pos != pb->write_pos) {
            // Upsample from 16kHz to 48kHz: repeat each sample 3 times
            // Phase counter persists across callbacks to avoid sample duplication at boundaries
            float val = (float)pb->buf[pb->read_pos] / 32768.0f * pb->volume;
            out[i] = val;
            pb->upsample_phase++;
            if (pb->upsample_phase >= 3) {
                pb->upsample_phase = 0;
                pb->read_pos = (pb->read_pos + 1) % PB_BUF_SIZE;
            }
        } else {
            out[i] = 0.0f;
            pb->upsample_phase = 0; // reset phase on underrun
        }
    }
    nx_pb_unlock(pb);
}

static nx_playback* nx_playback_new(void) {
    nx_playback* pb = (nx_playback*)calloc(1, sizeof(nx_playback));
    pb->buf = (int16_t*)calloc(PB_BUF_SIZE, sizeof(int16_t));
    pb->volume = 1.0f;
    return pb;
}

static int nx_playback_start(nx_playback* pb, int device_index) {
    if (pb->active) return 0;
    if (ma_context_init(NULL, 0, NULL, &pb->context) != MA_SUCCESS) return -1;

    ma_device_config config = ma_device_config_init(ma_device_type_playback);
    config.playback.format = ma_format_f32;
    config.playback.channels = 1;
    config.sampleRate = 48000;
    config.dataCallback = nx_playback_callback;
    config.pUserData = pb;

    if (device_index >= 0) {
        ma_device_info* infos;
        ma_uint32 count;
        if (ma_context_get_devices(&pb->context, &infos, &count, NULL, NULL) == MA_SUCCESS) {
            if ((ma_uint32)device_index < count) {
                config.playback.pDeviceID = &infos[device_index].id;
            }
        }
    }

    if (ma_device_init(&pb->context, &config, &pb->device) != MA_SUCCESS) {
        ma_context_uninit(&pb->context);
        return -2;
    }
    if (ma_device_start(&pb->device) != MA_SUCCESS) {
        ma_device_uninit(&pb->device);
        ma_context_uninit(&pb->context);
        return -3;
    }
    pb->active = 1;
    return 0;
}

static void nx_playback_stop(nx_playback* pb) {
    if (!pb || !pb->active) return;
    pb->active = 0;
    ma_device_stop(&pb->device);
    ma_device_uninit(&pb->device);
    ma_context_uninit(&pb->context);
}

static void nx_playback_free(nx_playback* pb) {
    if (!pb) return;
    nx_playback_stop(pb);
    if (pb->buf) free(pb->buf);
    free(pb);
}

// Write PCM samples (16kHz int16) to playback ring buffer
static void nx_playback_write(nx_playback* pb, const int16_t* samples, int count) {
    if (!pb || !pb->active) return;
    nx_pb_lock(pb);
    for (int i = 0; i < count; i++) {
        int next_pos = (pb->write_pos + 1) % PB_BUF_SIZE;
        // Overflow protection: если буфер полон — сдвигаем read_pos (drop oldest)
        if (next_pos == pb->read_pos) {
            pb->read_pos = (pb->read_pos + 1) % PB_BUF_SIZE;
        }
        pb->buf[pb->write_pos] = samples[i];
        pb->write_pos = next_pos;
    }
    nx_pb_unlock(pb);
}

static void nx_playback_set_volume(nx_playback* pb, float vol) {
    if (pb) pb->volume = vol;
}

// --- Loopback: звук системы для демонстрации экрана (фаза 5e) ---
// Реализация в loopback.c: process loopback miniaudio на десктопной Windows
// не работает (виртуальное устройство VAD\Process_Loopback открывается только
// через ActivateAudioInterfaceAsync, а miniaudio зовёт его лишь в UWP-сборке).
#include "loopback.h"

static void nx_spin_lock(volatile int* l) {
    while (__sync_lock_test_and_set(l, 1)) {}
}
static void nx_spin_unlock(volatile int* l) {
    __sync_lock_release(l);
}

// --- Воспроизведение 48 кГц стерео: звук чужой демонстрации (фаза 5e) ---
// Отдельное устройство на каждого показывающего — WASAPI shared mode сам
// смешивает его с голосовым playback.
#define PBS_RATE 48000
#define PBS_BUF_FRAMES (PBS_RATE) // 1 с кольцо

typedef struct {
    ma_device device;
    ma_context context;
    int active;
    int16_t* buf;             // interleaved L,R
    volatile int write_pos;   // в кадрах
    volatile int read_pos;
    volatile int buf_lock;
    float volume;
} nx_playback_st;

static void nx_playback_st_callback(ma_device* device, void* output, const void* input, ma_uint32 frameCount) {
    nx_playback_st* pb = (nx_playback_st*)device->pUserData;
    float* out = (float*)output;
    (void)input;
    if (!pb || !pb->active) {
        memset(output, 0, frameCount * 2 * sizeof(float));
        return;
    }
    nx_spin_lock(&pb->buf_lock);
    for (ma_uint32 i = 0; i < frameCount; i++) {
        if (pb->read_pos != pb->write_pos) {
            out[i * 2] = (float)pb->buf[pb->read_pos * 2] / 32768.0f * pb->volume;
            out[i * 2 + 1] = (float)pb->buf[pb->read_pos * 2 + 1] / 32768.0f * pb->volume;
            pb->read_pos = (pb->read_pos + 1) % PBS_BUF_FRAMES;
        } else {
            out[i * 2] = 0.0f;
            out[i * 2 + 1] = 0.0f;
        }
    }
    nx_spin_unlock(&pb->buf_lock);
}

static nx_playback_st* nx_playback_st_new(void) {
    nx_playback_st* pb = (nx_playback_st*)calloc(1, sizeof(nx_playback_st));
    pb->buf = (int16_t*)calloc(PBS_BUF_FRAMES * 2, sizeof(int16_t));
    pb->volume = 1.0f;
    return pb;
}

static int nx_playback_st_start(nx_playback_st* pb, int device_index) {
    if (pb->active) return 0;
    if (ma_context_init(NULL, 0, NULL, &pb->context) != MA_SUCCESS) return -1;
    ma_device_config config = ma_device_config_init(ma_device_type_playback);
    config.playback.format = ma_format_f32;
    config.playback.channels = 2;
    config.sampleRate = PBS_RATE;
    config.dataCallback = nx_playback_st_callback;
    config.pUserData = pb;
    if (device_index >= 0) {
        ma_device_info* infos;
        ma_uint32 count;
        if (ma_context_get_devices(&pb->context, &infos, &count, NULL, NULL) == MA_SUCCESS) {
            if ((ma_uint32)device_index < count) {
                config.playback.pDeviceID = &infos[device_index].id;
            }
        }
    }
    if (ma_device_init(&pb->context, &config, &pb->device) != MA_SUCCESS) {
        ma_context_uninit(&pb->context);
        return -2;
    }
    pb->active = 1;
    if (ma_device_start(&pb->device) != MA_SUCCESS) {
        pb->active = 0;
        ma_device_uninit(&pb->device);
        ma_context_uninit(&pb->context);
        return -3;
    }
    return 0;
}

static void nx_playback_st_stop(nx_playback_st* pb) {
    if (!pb || !pb->active) return;
    pb->active = 0;
    ma_device_stop(&pb->device);
    ma_device_uninit(&pb->device);
    ma_context_uninit(&pb->context);
}

static void nx_playback_st_free(nx_playback_st* pb) {
    if (!pb) return;
    nx_playback_st_stop(pb);
    if (pb->buf) free(pb->buf);
    free(pb);
}

// frames кадров interleaved int16 (L,R) в кольцо; при переполнении — выбрасываем старое.
static void nx_playback_st_write(nx_playback_st* pb, const int16_t* samples, int frames) {
    if (!pb || !pb->active) return;
    nx_spin_lock(&pb->buf_lock);
    for (int i = 0; i < frames; i++) {
        int next = (pb->write_pos + 1) % PBS_BUF_FRAMES;
        if (next == pb->read_pos) pb->read_pos = (pb->read_pos + 1) % PBS_BUF_FRAMES;
        pb->buf[pb->write_pos * 2] = samples[i * 2];
        pb->buf[pb->write_pos * 2 + 1] = samples[i * 2 + 1];
        pb->write_pos = next;
    }
    nx_spin_unlock(&pb->buf_lock);
}

static void nx_playback_st_set_volume(nx_playback_st* pb, float vol) {
    if (pb) pb->volume = vol;
}
*/
import "C"

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unsafe"
)

// DeviceType represents audio device direction.
type DeviceType int

const (
	DeviceCapture  DeviceType = 0 // Microphone
	DevicePlayback DeviceType = 1 // Speaker
)

// DeviceInfo describes an audio device.
type DeviceInfo struct {
	Name      string `json:"name"`
	Index     int    `json:"index"`
	IsDefault bool   `json:"isDefault"`
}

// ListDevices returns available audio devices of the given type.
func ListDevices(dt DeviceType) []DeviceInfo {
	list := C.nx_list_devices(C.int(dt))
	defer C.nx_free_device_list(&list)

	if list.count == 0 {
		return nil
	}

	devices := unsafe.Slice(list.devices, list.count)
	result := make([]DeviceInfo, list.count)
	for i := 0; i < int(list.count); i++ {
		result[i] = DeviceInfo{
			Name:      C.GoString(&devices[i].name[0]),
			Index:     i,
			IsDefault: devices[i].is_default != 0,
		}
	}
	return result
}

// Capture manages microphone input.
type Capture struct {
	c *C.nx_capture
}

// NewCapture creates a new capture instance.
func NewCapture() *Capture {
	return &Capture{c: C.nx_capture_new()}
}

// Start begins capturing from the specified device (-1 for default).
func (cap *Capture) Start(deviceIndex int) error {
	rc := C.nx_capture_start(cap.c, C.int(deviceIndex))
	if rc != 0 {
		return fmt.Errorf("capture start failed: %d", rc)
	}
	return nil
}

// Stop stops capturing.
func (cap *Capture) Stop() {
	C.nx_capture_stop(cap.c)
}

// Restart останавливает текущий capture и стартует заново на указанном
// устройстве. Используется при hot-swap микрофона (юзер переключил наушники
// → default device сменился). NS state (rnnoise/dfn) сохраняется.
// Между Stop и Start существует window 50-200ms где callback не пишет —
// voice loop получит тишину, не упадёт. Mutex не требуется: вызывается
// только из voice flow, который сам сериализует операции.
func (cap *Capture) Restart(deviceIndex int) error {
	C.nx_capture_stop(cap.c)
	rc := C.nx_capture_start(cap.c, C.int(deviceIndex))
	if rc != 0 {
		return fmt.Errorf("capture restart failed: %d", rc)
	}
	return nil
}

// Close frees all resources.
func (cap *Capture) Close() {
	C.nx_capture_free(cap.c)
	cap.c = nil
}

// SetGain sets the microphone gain multiplier (1.0 = normal).
func (cap *Capture) SetGain(gain float32) {
	C.nx_capture_set_gain(cap.c, C.float(gain))
}

// NoiseSuppressionLevel — уровень шумоподавления, применяемого в audio callback.
//
// Pipeline (подробности в C-комментарии nx_capture_callback):
//   Off    — голая децимация без фильтров (для A/B и низкого CPU)
//   Light  — HPF 80 Hz + anti-alias LPF 7 kHz (биквады, ~0 CPU, всегда доступен)
//   RNN    — Light + RNNoise (требует сборки с тегом nx_rnnoise и вендоринга xiph/rnnoise)
//   Deep   — резерв под DeepFilterNet, сейчас деградирует до RNN/Light
type NoiseSuppressionLevel int

const (
	NSOff   NoiseSuppressionLevel = 0
	NSLight NoiseSuppressionLevel = 1
	NSRNN   NoiseSuppressionLevel = 2
	NSDeep  NoiseSuppressionLevel = 3
)

// SetNoiseSuppression меняет уровень шумодава. Безопасно вызывать в любой момент,
// в том числе пока capture работает — флаг volatile, читается audio thread.
// Frame accumulator сбрасывается, чтобы не остался хвост от предыдущего режима.
func (cap *Capture) SetNoiseSuppression(level NoiseSuppressionLevel) {
	if cap.c == nil {
		return
	}
	C.nx_capture_set_ns_level(cap.c, C.int(level))
}

// RNNoiseAvailable возвращает true, если бинарь собран с тегом nx_rnnoise
// и RNNoise реально доступна. Без неё уровни RNN/Deep работают как Light.
func RNNoiseAvailable() bool {
	return C.nx_capture_rnnoise_available() != 0
}

// DFNAvailable возвращает true, если бинарь собран с тегом nx_dfn и
// DeepFilterNet3 (libnx_df.a) подлинкован. Без него NSDeep падает до RNN/Light.
func DFNAvailable() bool {
	return C.nx_capture_df_available() != 0
}

// LoadDFModel грузит DeepFilterNet3 модель из .tar.gz архива. Путь — к файлу,
// например "models/DeepFilterNet3.tar.gz". После успешной загрузки при уровне
// NSDeep капчер использует DFN3 вместо RNNoise.
//
// ВАЖНО: вызывать вне RT-контекста (до Start или когда ns_level != NSDeep),
// иначе возможна гонка с audio callback.
func (cap *Capture) LoadDFModel(path string) error {
	if cap.c == nil {
		return fmt.Errorf("capture: not initialized")
	}
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	rc := C.nx_capture_df_load(cap.c, cpath)
	if rc != 0 {
		return fmt.Errorf("LoadDFModel(%q): rc=%d", path, int(rc))
	}
	return nil
}

// UnloadDFModel выгружает DFN3 модель. После этого NSDeep деградирует до RNN/Light.
func (cap *Capture) UnloadDFModel() {
	if cap.c != nil {
		C.nx_capture_df_unload(cap.c)
	}
}

// DFModelLoaded возвращает true если DFN3 модель сейчас загружена в капчер.
func (cap *Capture) DFModelLoaded() bool {
	if cap.c == nil {
		return false
	}
	return C.nx_capture_df_loaded(cap.c) != 0
}

// SetDFAttenLim меняет максимальное attenuation в dB для DFN3 в runtime.
// 6.0 = очень мягкий, 12.0 = мягкий, 30.0 = по умолчанию, 60.0 = агрессивный,
// 100.0 = unlimited. No-op если DFN3 модель не загружена или бинарь собран
// без tag nx_dfn.
func (cap *Capture) SetDFAttenLim(limDb float32) {
	if cap.c == nil {
		return
	}
	C.nx_capture_df_set_atten_lim(cap.c, C.float(limDb))
}

// DFNStats — diagnostic-срез производительности DFN3 в RT-callback.
type DFNStats struct {
	Frames     int64 // фреймов обработано с прошлого снимка
	AvgNs      int64 // среднее время одного df_process_frame
	MaxNs      int64 // максимум одного фрейма
	OverBudget int64 // сколько раз фрейм превысил 10 мс (RT-бюджет)
}

// TakeDFNStats атомарно считывает счётчики и сбрасывает их. Безопасно звать
// из любого потока. Возвращает ноль-значения если capture не инициализирован
// или DFN не работал.
func (cap *Capture) TakeDFNStats() DFNStats {
	if cap.c == nil {
		return DFNStats{}
	}
	var frames, avg, max, over C.int64_t
	C.nx_capture_df_stats_take(cap.c, &frames, &avg, &max, &over)
	return DFNStats{
		Frames:     int64(frames),
		AvgNs:      int64(avg),
		MaxNs:      int64(max),
		OverBudget: int64(over),
	}
}

// NSStats — снимок A/B-эффективности шумодава за период.
//
// Каждый 10ms-фрейм классифицируется по dry_rms (RMS до NS):
//   - silent: dry_rms < 100 (тишина/окружение, NS должен давить)
//   - soft:   100..800 (тихая речь, согласные — NS НЕ должен давить)
//   - loud:   ≥ 800 (гласные, громкая речь — NS пропускает почти без изменений)
//
// SumRatio — сумма ratio = wet_rms / dry_rms по всем фреймам класса.
// Heavy — сколько фреймов класса было ослаблено критически (silent: <0.1,
// soft: <0.3, loud: <0.5).
//
// Высокий soft.Heavy% = "VAD ест согласные/слоги" — диагноз для жалоб
// "части слов пропадают с шумодавом".
type NSStats struct {
	TotalFrames    int64
	SilentN        int64
	SilentHeavy    int64
	SilentSumRatio float64
	SoftN          int64
	SoftHeavy      int64
	SoftSumRatio   float64
	LoudN          int64
	LoudHeavy      int64
	LoudSumRatio   float64
}

// TakeNSStats атомарно считывает A/B-счётчики шумодава и сбрасывает их.
func (cap *Capture) TakeNSStats() NSStats {
	if cap.c == nil {
		return NSStats{}
	}
	var s C.nx_ns_stats
	C.nx_capture_ns_stats_take(cap.c, &s)
	return NSStats{
		TotalFrames:    int64(s.total_frames),
		SilentN:        int64(s.silent_n),
		SilentHeavy:    int64(s.silent_heavy),
		SilentSumRatio: float64(s.silent_sum_ratio),
		SoftN:          int64(s.soft_n),
		SoftHeavy:      int64(s.soft_heavy),
		SoftSumRatio:   float64(s.soft_sum_ratio),
		LoudN:          int64(s.loud_n),
		LoudHeavy:      int64(s.loud_heavy),
		LoudSumRatio:   float64(s.loud_sum_ratio),
	}
}

// PeakLevel returns the current peak level (0.0-1.0) and resets it.
func (cap *Capture) PeakLevel() float32 {
	return float32(C.nx_capture_get_peak(cap.c))
}

// IsActive returns whether capture is running.
func (cap *Capture) IsActive() bool {
	return cap.c != nil && cap.c.active != 0
}

// StartRecording begins recording audio to an internal buffer (16kHz 16-bit mono).
func (cap *Capture) StartRecording() {
	C.nx_capture_start_recording(cap.c)
}

// StopRecording stops recording and returns the audio as WAV bytes.
func (cap *Capture) StopRecording() []byte {
	C.nx_capture_stop_recording(cap.c)
	var size C.int
	ptr := C.nx_capture_get_wav(cap.c, &size)
	if ptr == nil || size == 0 {
		return nil
	}
	defer C.free(ptr)
	return C.GoBytes(ptr, size)
}

// RecordedDuration returns the duration of the current recording in milliseconds.
func (cap *Capture) RecordedDuration() int {
	samples := int(C.nx_capture_rec_samples(cap.c))
	return samples * 1000 / 16000 // 16kHz sample rate
}

// IsRecording returns whether recording is active.
func (cap *Capture) IsRecording() bool {
	return cap.c != nil && cap.c.recording != 0
}

// IsRecordingFull returns true if the recording buffer reached the maximum duration.
func (cap *Capture) IsRecordingFull() bool {
	return cap.c != nil && C.nx_capture_rec_full(cap.c) != 0
}

// MaxRecordingSeconds returns the maximum recording duration in seconds.
func MaxRecordingSeconds() int {
	return 1800 // REC_MAX_SECONDS
}

// StreamStart begins streaming mode: captures audio and allows reading chunks.
func (cap *Capture) StreamStart() {
	C.nx_capture_stream_start(cap.c)
}

// StreamStop stops streaming mode.
func (cap *Capture) StreamStop() {
	C.nx_capture_stream_stop(cap.c)
}

// StreamRead reads available PCM samples into a buffer.
// Returns the PCM data as []byte (16-bit LE samples).
// chunkSamples is the max number of samples to read (e.g., 320 for 20ms @ 16kHz).
func (cap *Capture) StreamRead(chunkSamples int) []byte {
	if cap.c == nil {
		return nil
	}
	buf := make([]int16, chunkSamples)
	n := int(C.nx_capture_stream_read(cap.c, (*C.int16_t)(unsafe.Pointer(&buf[0])), C.int(chunkSamples)))
	if n <= 0 {
		return nil
	}
	// Convert int16 slice to bytes
	out := make([]byte, n*2)
	for i := 0; i < n; i++ {
		out[i*2] = byte(buf[i])
		out[i*2+1] = byte(buf[i] >> 8)
	}
	return out
}

// --- Playback ---

// Playback manages speaker output for voice chat.
type Playback struct {
	p *C.nx_playback
}

// NewPlayback creates a new playback instance.
func NewPlayback() *Playback {
	return &Playback{p: C.nx_playback_new()}
}

// Start begins playback on the specified device (-1 for default).
func (pb *Playback) Start(deviceIndex int) error {
	rc := C.nx_playback_start(pb.p, C.int(deviceIndex))
	if rc != 0 {
		return fmt.Errorf("playback start failed: %d", rc)
	}
	return nil
}

// Stop stops playback.
func (pb *Playback) Stop() {
	if pb.p != nil {
		C.nx_playback_stop(pb.p)
	}
}

// Restart останавливает текущий playback и стартует заново на указанном
// устройстве. Используется при hot-swap динамика (юзер переключил наушники).
// Между Stop и Start fragment ≤ 200ms где Write игнорируется — слышен
// микропровал, jitter buffer накопит и продолжит без дальнейшего сбоя.
func (pb *Playback) Restart(deviceIndex int) error {
	if pb.p == nil {
		return fmt.Errorf("playback closed")
	}
	C.nx_playback_stop(pb.p)
	rc := C.nx_playback_start(pb.p, C.int(deviceIndex))
	if rc != 0 {
		return fmt.Errorf("playback restart failed: %d", rc)
	}
	return nil
}

// Close frees all resources.
func (pb *Playback) Close() {
	if pb.p != nil {
		C.nx_playback_free(pb.p)
		pb.p = nil
	}
}

// Write writes PCM samples (16-bit LE, 16kHz mono) to the playback buffer.
func (pb *Playback) Write(pcmData []byte) {
	if pb.p == nil || len(pcmData) < 2 {
		return
	}
	samples := len(pcmData) / 2
	C.nx_playback_write(pb.p, (*C.int16_t)(unsafe.Pointer(&pcmData[0])), C.int(samples))
}

// SetVolume sets the playback volume (0.0 = mute, 1.0 = normal, 1.4 = 140%).
func (pb *Playback) SetVolume(vol float32) {
	if pb.p != nil {
		C.nx_playback_set_volume(pb.p, C.float(vol))
	}
}

// IsActive returns whether playback is running.
func (pb *Playback) IsActive() bool {
	return pb.p != nil && pb.p.active != 0
}

// --- Звук системы для демонстрации экрана (фаза 5e) ---

const (
	LoopbackRate      = 48000
	LoopbackChannels  = 2
	LoopbackFrameSize = 960 // 20 мс на канал
)

// Loopback — захват звука системы без звука самого Vox (см. nx_loopback_start).
type Loopback struct {
	l *C.nx_loopback
}

func NewLoopback() *Loopback {
	return &Loopback{l: C.nx_loopback_new()}
}

// Start — ошибка на не-Windows и на Windows старше build 20348 (там нельзя
// исключить собственный процесс из захвата).
func (lb *Loopback) Start() error {
	if rc := C.nx_loopback_start(lb.l); rc != 0 {
		return fmt.Errorf("loopback start failed: %d", rc)
	}
	return nil
}

func (lb *Loopback) Stop() {
	if lb.l != nil {
		C.nx_loopback_stop(lb.l)
	}
}

func (lb *Loopback) Close() {
	if lb.l != nil {
		C.nx_loopback_free(lb.l)
		lb.l = nil
	}
}

// ReadFrame — ровно 20 мс interleaved int16 LE (960×2 сэмпла) или nil, если
// в буфере ещё меньше.
func (lb *Loopback) ReadFrame() []byte {
	if lb.l == nil {
		return nil
	}
	buf := make([]byte, LoopbackFrameSize*LoopbackChannels*2)
	n := C.nx_loopback_read_frame(lb.l, (*C.int16_t)(unsafe.Pointer(&buf[0])), C.int(LoopbackFrameSize))
	if n <= 0 {
		return nil
	}
	return buf
}

// LoopbackSupported — пробное открытие loopback с исключением процесса.
func LoopbackSupported() bool {
	lb := NewLoopback()
	defer lb.Close()
	if err := lb.Start(); err != nil {
		return false
	}
	lb.Stop()
	return true
}

// StereoPlayback — вывод 48 кГц стерео (звук чужой демонстрации).
type StereoPlayback struct {
	p *C.nx_playback_st
}

func NewStereoPlayback() *StereoPlayback {
	return &StereoPlayback{p: C.nx_playback_st_new()}
}

// Start — на устройстве deviceIndex (-1 — по умолчанию).
func (pb *StereoPlayback) Start(deviceIndex int) error {
	if rc := C.nx_playback_st_start(pb.p, C.int(deviceIndex)); rc != 0 {
		return fmt.Errorf("stereo playback start failed: %d", rc)
	}
	return nil
}

func (pb *StereoPlayback) Close() {
	if pb.p != nil {
		C.nx_playback_st_free(pb.p)
		pb.p = nil
	}
}

// Write — interleaved int16 LE (L,R).
func (pb *StereoPlayback) Write(pcm []byte) {
	if pb.p == nil || len(pcm) < 4 {
		return
	}
	C.nx_playback_st_write(pb.p, (*C.int16_t)(unsafe.Pointer(&pcm[0])), C.int(len(pcm)/4))
}

// SetVolume — 0.0 тишина, 1.0 как есть, до 2.0.
func (pb *StereoPlayback) SetVolume(vol float32) {
	if pb.p != nil {
		C.nx_playback_st_set_volume(pb.p, C.float(vol))
	}
}

// GenerateWaveform создаёт пики амплитуды из WAV данных для визуализации.
// Возвращает строку "25,50,80,..." где значения 0-100.
func GenerateWaveform(wavData []byte, bars int) string {
	if bars <= 0 {
		bars = 32
	}
	// WAV header = 44 bytes, 16-bit samples = 2 bytes each
	if len(wavData) < 46 {
		return ""
	}
	pcmData := wavData[44:]
	totalSamples := len(pcmData) / 2
	if totalSamples == 0 {
		return ""
	}

	samplesPerBar := totalSamples / bars
	if samplesPerBar == 0 {
		samplesPerBar = 1
	}

	peaks := make([]float64, bars)
	globalMax := float64(0)

	for i := 0; i < bars; i++ {
		start := i * samplesPerBar
		end := start + samplesPerBar
		if end > totalSamples {
			end = totalSamples
		}
		maxAmp := float64(0)
		for j := start; j < end; j++ {
			sample := int16(binary.LittleEndian.Uint16(pcmData[j*2 : j*2+2]))
			amp := math.Abs(float64(sample))
			if amp > maxAmp {
				maxAmp = amp
			}
		}
		if maxAmp > globalMax {
			globalMax = maxAmp
		}
		peaks[i] = maxAmp
	}

	// Normalize to 0-100
	parts := make([]string, bars)
	for i, p := range peaks {
		if globalMax > 0 {
			parts[i] = strconv.Itoa(int(p / globalMax * 100))
		} else {
			parts[i] = "0"
		}
	}
	return strings.Join(parts, ",")
}
