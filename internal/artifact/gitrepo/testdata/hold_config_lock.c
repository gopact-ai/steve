#define _GNU_SOURCE
#include <dlfcn.h>
#include <fcntl.h>
#include <stdarg.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

/* Hold the real config lock until the parent cancels this git process. */
static void hold_lock(const char *path, int fd, int (*open_real)(const char *, int, ...)) {
    const char *root = getenv("GITREPO_LOCK_ROOT");
    const char *ready = getenv("GITREPO_LOCK_READY");
    const char *gate = getenv("GITREPO_LOCK_GATE");
    const char *suffix = "/config.lock";
    if (fd < 0 || !root || !ready || !gate) return;
    size_t size = strlen(path), end = strlen(suffix);
    if (strncmp(path, root, strlen(root)) || size < end || strcmp(path + size - end, suffix)) return;
    int claim = open_real(ready, O_WRONLY | O_CREAT | O_EXCL, 0600);
    if (claim < 0) return;
    write(claim, path, size);
    close(claim);
    int blocked = open_real(gate, O_RDONLY);
    if (blocked >= 0) close(blocked);
}

int open(const char *path, int flags, ...) {
    int (*open_real)(const char *, int, ...) = dlsym(RTLD_NEXT, "open");
    mode_t mode = 0;
    if (flags & O_CREAT) { va_list ap; va_start(ap, flags); mode = va_arg(ap, int); va_end(ap); }
    int fd = open_real(path, flags, mode);
    hold_lock(path, fd, open_real);
    return fd;
}

int open64(const char *path, int flags, ...) {
    int (*open_real)(const char *, int, ...) = dlsym(RTLD_NEXT, "open64");
    mode_t mode = 0;
    if (flags & O_CREAT) { va_list ap; va_start(ap, flags); mode = va_arg(ap, int); va_end(ap); }
    int fd = open_real(path, flags, mode);
    hold_lock(path, fd, open_real);
    return fd;
}
