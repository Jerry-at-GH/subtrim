#include "render.h"
#include <stdlib.h>
#include <string.h>
#ifdef _WIN32
#include <windows.h>
#else
#include <dlfcn.h>
#endif

/* only library loading and libass calls belong in C; Go owns pixel work. */
#define FUNCTIONS(X) \
 X(ASS_Library *, ass_library_init, (void)) \
 X(void, ass_library_done, (ASS_Library *)) \
 X(void, ass_set_message_cb, (ASS_Library *, void (*)(int,const char *,va_list,void *),void *)) \
 X(void, ass_set_extract_fonts, (ASS_Library *,int)) \
 X(void, ass_set_fonts_dir, (ASS_Library *,const char *)) \
 X(ASS_Track *, ass_read_memory, (ASS_Library *,char *,size_t,char *)) \
 X(void, ass_free_track, (ASS_Track *)) \
 X(ASS_Renderer *, ass_renderer_init, (ASS_Library *)) \
 X(void, ass_renderer_done, (ASS_Renderer *)) \
 X(void, ass_set_frame_size, (ASS_Renderer *,int,int)) \
 X(void, ass_set_storage_size, (ASS_Renderer *,int,int)) \
 X(void, ass_set_hinting, (ASS_Renderer *,ASS_Hinting)) \
 X(void, ass_set_fonts, (ASS_Renderer *,const char *,const char *,int,const char *,int)) \
 X(ASS_Image *, ass_render_frame, (ASS_Renderer *,ASS_Track *,long long,int *))

struct SubtrimRenderer {
 void *module;
 ASS_Library *library;
 ASS_Renderer *renderer;
 ASS_Track *track;
#define FIELD(result,name,args) result (*name) args;
 FUNCTIONS(FIELD)
#undef FIELD
};

static void message(int level,const char *format,va_list args,void *context) {}

SubtrimRenderer *subtrim_open(const char *path,char *error,size_t capacity) {
 SubtrimRenderer *r=calloc(1,sizeof(*r));
 if(!r){snprintf(error,capacity,"cannot allocate renderer");return NULL;}
#ifdef _WIN32
 int length=MultiByteToWideChar(CP_UTF8,MB_ERR_INVALID_CHARS,path,-1,NULL,0);
 wchar_t *wide=calloc(length?length:1,sizeof(*wide));
 if(wide && length) {
  MultiByteToWideChar(CP_UTF8,MB_ERR_INVALID_CHARS,path,-1,wide,length);
  r->module=LoadLibraryExW(wide,NULL,LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR|LOAD_LIBRARY_SEARCH_DEFAULT_DIRS);
 }
 free(wide);
 if(!r->module)snprintf(error,capacity,"cannot load libass (Windows error %lu)",GetLastError());
#define SYMBOL(name) GetProcAddress((HMODULE)r->module,#name)
#else
 r->module=dlopen(path,RTLD_NOW|RTLD_LOCAL);
 if(!r->module)snprintf(error,capacity,"cannot load libass: %s",dlerror());
#define SYMBOL(name) dlsym(r->module,#name)
#endif
 if(!r->module){free(r);return NULL;}
#define LOAD(result,name,args) \
 r->name=(result (*) args)SYMBOL(name); \
 if(!r->name){snprintf(error,capacity,"libass lacks " #name);subtrim_close(r);return NULL;}
 FUNCTIONS(LOAD)
#undef LOAD
#undef SYMBOL
 return r;
}

static void unload(SubtrimRenderer *r) {
 if(r->renderer)r->ass_renderer_done(r->renderer);
 if(r->track)r->ass_free_track(r->track);
 if(r->library)r->ass_library_done(r->library);
 r->renderer=NULL;r->track=NULL;r->library=NULL;
}

void subtrim_close(SubtrimRenderer *r) {
 if(!r)return;
 unload(r);
#ifdef _WIN32
 if(r->module)FreeLibrary((HMODULE)r->module);
#else
 if(r->module)dlclose(r->module);
#endif
 free(r);
}

ASS_Track *subtrim_load(SubtrimRenderer *r,char *data,size_t size,const char *fonts,int reuse) {
 /* Go permits reuse only within one file and without embedded fonts. */
 if(reuse && r->library) {
  if(r->track)r->ass_free_track(r->track);
  r->track=NULL;
 } else {
  unload(r);
  r->library=r->ass_library_init();
  if(!r->library)return 0;
  r->ass_set_message_cb(r->library,message,NULL);
  r->ass_set_extract_fonts(r->library,1);
  if(fonts && *fonts)r->ass_set_fonts_dir(r->library,fonts);
 }
 r->track=r->ass_read_memory(r->library,data,size,NULL);
 return r->track;
}

int subtrim_configure(SubtrimRenderer *r,int width,int height,int storage_width,
                    int storage_height,const char *font) {
 r->renderer=r->ass_renderer_init(r->library);
 if(!r->renderer)return 0;
 r->ass_set_frame_size(r->renderer,width,height);
 r->ass_set_storage_size(r->renderer,storage_width,storage_height);
 r->ass_set_hinting(r->renderer,ASS_HINTING_NONE);
 r->ass_set_fonts(r->renderer,font && *font?font:NULL,"sans-serif",font && *font?0:1,NULL,1);
 return 1;
}
ASS_Image *subtrim_frame(SubtrimRenderer *r,long long time,int *changed){return r->ass_render_frame(r->renderer,r->track,time,changed);}
