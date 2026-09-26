#include <opus.h>

// CGO cannot call variadic C functions directly.
// These wrappers provide non-variadic access to opus_encoder_ctl.

int opus_encoder_ctl_set_bitrate(OpusEncoder *enc, opus_int32 bitrate) {
    return opus_encoder_ctl(enc, OPUS_SET_BITRATE(bitrate));
}
