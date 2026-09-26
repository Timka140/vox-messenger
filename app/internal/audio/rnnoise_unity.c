/*
 * RNNoise unity build for Nexus Messenger.
 *
 * CGO компилирует только .c из директории пакета. Чтобы не таскать ~10 файлов
 * RNNoise в корень audio/ и не плодить build-step с librnnoise.a, мы делаем
 * unity build: один файл, который #include всех нужных .c из vendored xiph/rnnoise.
 *
 * Содержимое активно ТОЛЬКО при -DNX_USE_RNNOISE (см. audio_rnnoise.go с тегом
 * nx_rnnoise). Без тега это пустой translation unit — никакого оверхеда.
 *
 * Проверено: только один дубликат static-имени (compute_pitch_gain в pitch.c
 * под #ifdef FIXED_POINT / #else — реального конфликта нет).
 */

#ifdef NX_USE_RNNOISE

#include "rnnoise/src/denoise.c"
#include "rnnoise/src/rnn.c"
#include "rnnoise/src/pitch.c"
#include "rnnoise/src/kiss_fft.c"
#include "rnnoise/src/celt_lpc.c"
#include "rnnoise/src/nnet.c"
#include "rnnoise/src/nnet_default.c"
#include "rnnoise/src/parse_lpcnet_weights.c"
#include "rnnoise/src/rnnoise_data.c"
#include "rnnoise/src/rnnoise_tables.c"

#endif /* NX_USE_RNNOISE */
