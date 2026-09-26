#ifndef NX_DF_H
#define NX_DF_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

// Opaque state — upstream name is DFState (libDF/src/capi.rs).
typedef struct DFState DFState;

// path: path to a DeepFilterNet3 .tar.gz model archive.
// atten_lim: attenuation limit in dB (0 = no limit).
// Returns NULL / aborts on failure (upstream uses .expect()).
DFState* df_create(const char* path, float atten_lim);

// Returns hop_size (expected 480 for DFN3 at 48 kHz / 10 ms).
size_t df_get_frame_length(DFState* st);

// Process one frame. `input` and `output` must be hop_size floats each.
// Not in-place safe — upstream writes through an ArrayViewMut2 built from
// the output pointer. Caller should use two distinct buffers.
// Returns local SNR (dB) of the current frame.
float df_process_frame(DFState* st, float* input, float* output);

void df_set_atten_lim(DFState* st, float lim_db);
void df_set_post_filter_beta(DFState* st, float beta);
void df_free(DFState* st);

#ifdef __cplusplus
}
#endif

#endif
