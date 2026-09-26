// Звук системы для демонстрации экрана (фаза 5e), реализация в loopback.c.
#ifndef NX_LOOPBACK_H
#define NX_LOOPBACK_H

#include <stdint.h>

typedef struct nx_loopback nx_loopback;

nx_loopback* nx_loopback_new(void);
// 0 — захват идёт; иначе код ошибки (-100 — не Windows).
int nx_loopback_start(nx_loopback* lb);
void nx_loopback_stop(nx_loopback* lb);
void nx_loopback_free(nx_loopback* lb);
// Ровно want кадров interleaved int16 L,R или 0, если в кольце меньше.
int nx_loopback_read_frame(nx_loopback* lb, int16_t* out, int want);

#endif
