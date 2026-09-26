// Звук системы для демонстрации экрана (фаза 5e).
//
// WASAPI process loopback: всё, что играет система, КРОМЕ дерева процессов
// самого Vox (PROCESS_LOOPBACK_MODE_EXCLUDE_TARGET_PROCESS_TREE) — иначе
// зрители слышали бы голоса звонка. Нужна Windows build >= 20348; на более
// старых активация падает и тумблер в диалоге выключен. Полный loopback без
// исключения не используется никогда.
//
// Виртуальное устройство VAD\Process_Loopback открывается только через
// ActivateAudioInterfaceAsync (miniaudio зовёт его лишь в UWP-сборке, поэтому
// свой код). Формат 48 кГц стерео int16, WASAPI конвертирует сам
// (AUTOCONVERTPCM). Весь COM живёт в одном потоке захвата.

#ifdef _WIN32
#define COBJMACROS
#include <windows.h>
#include <audioclient.h>
#include <mmdeviceapi.h>
#endif

#include <stdlib.h>
#include <string.h>
#include "loopback.h"

#define LB_RATE 48000
#define LB_CH 2
#define LB_BUF_FRAMES (LB_RATE * 2) // 2 с кольцо

struct nx_loopback {
    int16_t* buf;             // interleaved L,R
    volatile int write_pos;   // в кадрах
    volatile int read_pos;
    volatile int buf_lock;
    volatile int active;      // поток захвата работает
    volatile int stop;
#ifdef _WIN32
    HANDLE thread;
    HANDLE ready;             // поток закончил инициализацию
    volatile int init_rc;
#endif
};

static void lb_lock(nx_loopback* lb) {
    while (__sync_lock_test_and_set(&lb->buf_lock, 1)) {}
}
static void lb_unlock(nx_loopback* lb) {
    __sync_lock_release(&lb->buf_lock);
}

// frames кадров в кольцо; samples == NULL — тишина. Полон — выбрасываем старое.
static void lb_push(nx_loopback* lb, const int16_t* samples, uint32_t frames) {
    lb_lock(lb);
    for (uint32_t i = 0; i < frames; i++) {
        int next = (lb->write_pos + 1) % LB_BUF_FRAMES;
        if (next == lb->read_pos) lb->read_pos = (lb->read_pos + 1) % LB_BUF_FRAMES;
        lb->buf[lb->write_pos * LB_CH] = samples ? samples[i * LB_CH] : 0;
        lb->buf[lb->write_pos * LB_CH + 1] = samples ? samples[i * LB_CH + 1] : 0;
        lb->write_pos = next;
    }
    lb_unlock(lb);
}

nx_loopback* nx_loopback_new(void) {
    nx_loopback* lb = (nx_loopback*)calloc(1, sizeof(nx_loopback));
    lb->buf = (int16_t*)calloc(LB_BUF_FRAMES * LB_CH, sizeof(int16_t));
    return lb;
}

int nx_loopback_read_frame(nx_loopback* lb, int16_t* out, int want) {
    if (!lb || !lb->active) return 0;
    lb_lock(lb);
    int avail = (lb->write_pos - lb->read_pos + LB_BUF_FRAMES) % LB_BUF_FRAMES;
    if (avail < want) {
        lb_unlock(lb);
        return 0;
    }
    for (int n = 0; n < want; n++) {
        out[n * LB_CH] = lb->buf[lb->read_pos * LB_CH];
        out[n * LB_CH + 1] = lb->buf[lb->read_pos * LB_CH + 1];
        lb->read_pos = (lb->read_pos + 1) % LB_BUF_FRAMES;
    }
    lb_unlock(lb);
    return want;
}

void nx_loopback_free(nx_loopback* lb) {
    if (!lb) return;
    nx_loopback_stop(lb);
    free(lb->buf);
    free(lb);
}

#ifdef _WIN32

// Свои GUID и структуры: в mingw нет audioclientactivationparams.h, а
// IID_* из uuid.lib тянуть ради пяти констант не хочется.
static const GUID NX_IID_IUnknown = {0x00000000, 0x0000, 0x0000, {0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}};
static const GUID NX_IID_IAgileObject = {0x94ea2b94, 0xe9cc, 0x49e0, {0xc0, 0xff, 0xee, 0x64, 0xca, 0x8f, 0x5b, 0x90}};
static const GUID NX_IID_IActivateCompletion = {0x41D949AB, 0x9862, 0x444A, {0x80, 0xF6, 0xC2, 0x61, 0x33, 0x4D, 0xA5, 0xEB}};
static const GUID NX_IID_IAudioClient = {0x1CB9AD4C, 0xDBFA, 0x4c32, {0xB1, 0x78, 0xC2, 0xF5, 0x68, 0xA7, 0x03, 0xB2}};
static const GUID NX_IID_IAudioCaptureClient = {0xC8ADBD64, 0xE71E, 0x48a0, {0xA4, 0xDE, 0x18, 0x5C, 0x39, 0x5C, 0xD3, 0x17}};

#define NX_ACTIVATION_TYPE_PROCESS_LOOPBACK 1
#define NX_PROCESS_LOOPBACK_MODE_EXCLUDE 1
#define NX_STREAMFLAGS_AUTOCONVERTPCM 0x80000000
#define NX_STREAMFLAGS_SRC_DEFAULT_QUALITY 0x08000000

typedef struct {
    DWORD TargetProcessId;
    int ProcessLoopbackMode;
} nx_process_loopback_params;

typedef struct {
    int ActivationType;
    nx_process_loopback_params params;
} nx_activation_params;

// Раскладка PROPVARIANT с VT_BLOB (x64: vt+3 reserved = 8 байт, дальше BLOB).
typedef struct {
    unsigned short vt;
    unsigned short reserved1, reserved2, reserved3;
    struct {
        ULONG cbSize;
        BYTE* pBlobData;
    } blob;
} nx_propvariant_blob;

typedef HRESULT (WINAPI *nx_pfn_activate)(LPCWSTR, REFIID, void*, IActivateAudioInterfaceCompletionHandler*, IActivateAudioInterfaceAsyncOperation**);

// Обработчик завершения активации: COM-объект на куче, освобождается по refcount.
typedef struct {
    IActivateAudioInterfaceCompletionHandlerVtbl* lpVtbl;
    volatile LONG refs;
    HANDLE done;
} nx_act_handler;

static ULONG STDMETHODCALLTYPE h_addref(IActivateAudioInterfaceCompletionHandler* This) {
    return (ULONG)InterlockedIncrement(&((nx_act_handler*)This)->refs);
}

static ULONG STDMETHODCALLTYPE h_release(IActivateAudioInterfaceCompletionHandler* This) {
    nx_act_handler* h = (nx_act_handler*)This;
    LONG n = InterlockedDecrement(&h->refs);
    if (n == 0) {
        CloseHandle(h->done);
        free(h);
    }
    return (ULONG)n;
}

static HRESULT STDMETHODCALLTYPE h_qi(IActivateAudioInterfaceCompletionHandler* This, REFIID riid, void** ppv) {
    if (IsEqualGUID(riid, &NX_IID_IUnknown) || IsEqualGUID(riid, &NX_IID_IActivateCompletion) || IsEqualGUID(riid, &NX_IID_IAgileObject)) {
        *ppv = This;
        h_addref(This);
        return S_OK;
    }
    *ppv = NULL;
    return E_NOINTERFACE;
}

static HRESULT STDMETHODCALLTYPE h_completed(IActivateAudioInterfaceCompletionHandler* This, IActivateAudioInterfaceAsyncOperation* op) {
    (void)op;
    SetEvent(((nx_act_handler*)This)->done);
    return S_OK;
}

static IActivateAudioInterfaceCompletionHandlerVtbl nx_act_vtbl = {h_qi, h_addref, h_release, h_completed};

// Активация IAudioClient на VAD\Process_Loopback с исключением своего процесса.
static int lb_activate(IAudioClient** out) {
    static nx_pfn_activate activate = NULL;
    if (!activate) {
        HMODULE mm = LoadLibraryW(L"mmdevapi.dll");
        if (mm) activate = (nx_pfn_activate)(void*)GetProcAddress(mm, "ActivateAudioInterfaceAsync");
        if (!activate) return -10;
    }

    nx_activation_params ap;
    memset(&ap, 0, sizeof(ap));
    ap.ActivationType = NX_ACTIVATION_TYPE_PROCESS_LOOPBACK;
    ap.params.TargetProcessId = GetCurrentProcessId();
    ap.params.ProcessLoopbackMode = NX_PROCESS_LOOPBACK_MODE_EXCLUDE;
    nx_propvariant_blob pv;
    memset(&pv, 0, sizeof(pv));
    pv.vt = VT_BLOB;
    pv.blob.cbSize = sizeof(ap);
    pv.blob.pBlobData = (BYTE*)&ap;

    nx_act_handler* h = (nx_act_handler*)calloc(1, sizeof(nx_act_handler));
    h->lpVtbl = &nx_act_vtbl;
    h->refs = 1;
    h->done = CreateEventW(NULL, FALSE, FALSE, NULL);
    IActivateAudioInterfaceCompletionHandler* handler = (IActivateAudioInterfaceCompletionHandler*)h;

    IActivateAudioInterfaceAsyncOperation* op = NULL;
    HRESULT hr = activate(L"VAD\\Process_Loopback", &NX_IID_IAudioClient, &pv, handler, &op);
    if (FAILED(hr)) {
        h_release(handler);
        return -11;
    }
    if (WaitForSingleObject(h->done, 5000) != WAIT_OBJECT_0) {
        IActivateAudioInterfaceAsyncOperation_Release(op);
        h_release(handler);
        return -12;
    }
    HRESULT act = E_FAIL;
    IUnknown* unk = NULL;
    hr = IActivateAudioInterfaceAsyncOperation_GetActivateResult(op, &act, &unk);
    IActivateAudioInterfaceAsyncOperation_Release(op);
    h_release(handler);
    if (FAILED(hr) || FAILED(act) || !unk) {
        if (unk) IUnknown_Release(unk);
        return -13;
    }
    hr = IUnknown_QueryInterface(unk, &NX_IID_IAudioClient, (void**)out);
    IUnknown_Release(unk);
    return FAILED(hr) ? -14 : 0;
}

static DWORD WINAPI lb_thread(LPVOID arg) {
    nx_loopback* lb = (nx_loopback*)arg;
    HRESULT hr = CoInitializeEx(NULL, COINIT_MULTITHREADED);
    int com = SUCCEEDED(hr);
    IAudioClient* client = NULL;
    IAudioCaptureClient* cap = NULL;
    HANDLE ev = NULL;

    int rc = lb_activate(&client);
    if (rc == 0) {
        WAVEFORMATEX wf;
        memset(&wf, 0, sizeof(wf));
        wf.wFormatTag = WAVE_FORMAT_PCM;
        wf.nChannels = LB_CH;
        wf.nSamplesPerSec = LB_RATE;
        wf.wBitsPerSample = 16;
        wf.nBlockAlign = LB_CH * 2;
        wf.nAvgBytesPerSec = LB_RATE * wf.nBlockAlign;
        DWORD flags = AUDCLNT_STREAMFLAGS_LOOPBACK | AUDCLNT_STREAMFLAGS_EVENTCALLBACK |
                      NX_STREAMFLAGS_AUTOCONVERTPCM | NX_STREAMFLAGS_SRC_DEFAULT_QUALITY;
        hr = IAudioClient_Initialize(client, AUDCLNT_SHAREMODE_SHARED, flags, 200000, 0, &wf, NULL);
        if (FAILED(hr)) rc = -15;
    }
    if (rc == 0) {
        ev = CreateEventW(NULL, FALSE, FALSE, NULL);
        if (FAILED(IAudioClient_SetEventHandle(client, ev))) rc = -16;
    }
    if (rc == 0 && FAILED(IAudioClient_GetService(client, &NX_IID_IAudioCaptureClient, (void**)&cap))) rc = -17;
    if (rc == 0 && FAILED(IAudioClient_Start(client))) rc = -18;

    lb->init_rc = rc;
    if (rc == 0) lb->active = 1;
    SetEvent(lb->ready);

    if (rc == 0) {
        while (!lb->stop) {
            WaitForSingleObject(ev, 100);
            UINT32 packet = 0;
            while (!lb->stop && SUCCEEDED(IAudioCaptureClient_GetNextPacketSize(cap, &packet)) && packet > 0) {
                BYTE* data = NULL;
                UINT32 frames = 0;
                DWORD bflags = 0;
                if (FAILED(IAudioCaptureClient_GetBuffer(cap, &data, &frames, &bflags, NULL, NULL))) break;
                lb_push(lb, (bflags & AUDCLNT_BUFFERFLAGS_SILENT) ? NULL : (const int16_t*)data, frames);
                IAudioCaptureClient_ReleaseBuffer(cap, frames);
            }
        }
        IAudioClient_Stop(client);
    }
    lb->active = 0;
    if (cap) IAudioCaptureClient_Release(cap);
    if (client) IAudioClient_Release(client);
    if (ev) CloseHandle(ev);
    if (com) CoUninitialize();
    return 0;
}

int nx_loopback_start(nx_loopback* lb) {
    if (lb->thread) return lb->active ? 0 : -1;
    lb->stop = 0;
    lb->read_pos = 0;
    lb->write_pos = 0;
    lb->init_rc = -1;
    lb->ready = CreateEventW(NULL, TRUE, FALSE, NULL);
    lb->thread = CreateThread(NULL, 0, lb_thread, lb, 0, NULL);
    if (!lb->thread) {
        CloseHandle(lb->ready);
        lb->ready = NULL;
        return -2;
    }
    if (WaitForSingleObject(lb->ready, 10000) != WAIT_OBJECT_0) {
        nx_loopback_stop(lb);
        return -3;
    }
    if (lb->init_rc != 0) {
        int rc = lb->init_rc;
        nx_loopback_stop(lb);
        return rc;
    }
    return 0;
}

void nx_loopback_stop(nx_loopback* lb) {
    if (!lb || !lb->thread) return;
    lb->stop = 1;
    WaitForSingleObject(lb->thread, INFINITE);
    CloseHandle(lb->thread);
    lb->thread = NULL;
    CloseHandle(lb->ready);
    lb->ready = NULL;
}

#else

int nx_loopback_start(nx_loopback* lb) {
    (void)lb;
    return -100;
}

void nx_loopback_stop(nx_loopback* lb) {
    (void)lb;
}

#endif
