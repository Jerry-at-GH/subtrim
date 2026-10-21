#ifndef SUBTRIM_RENDER_H
#define SUBTRIM_RENDER_H
#include "include/ass.h"

typedef struct SubtrimRenderer SubtrimRenderer;
SubtrimRenderer *subtrim_open(const char *path, char *error, size_t capacity);
void subtrim_close(SubtrimRenderer *r);
ASS_Track *subtrim_load(SubtrimRenderer *r, char *data, size_t size, const char *fonts, int reuse);
int subtrim_configure(SubtrimRenderer *r, int width, int height, int storage_width,
                    int storage_height, const char *font);
ASS_Image *subtrim_frame(SubtrimRenderer *r, long long time, int *changed);
#endif
