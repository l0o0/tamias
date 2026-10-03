/* SPDX-License-Identifier: AGPL-3.0-only
 * Resolve only WebKitGTK's three compiled helper paths into this AppImage.
 */
#define _GNU_SOURCE

#include <dlfcn.h>
#include <errno.h>
#include <limits.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

typedef struct _GSubprocess GSubprocess;
typedef struct _GSubprocessLauncher GSubprocessLauncher;
typedef struct _GError GError;
typedef GSubprocess *(*spawnv_fn)(GSubprocessLauncher *, const char *const *, GError **);

static int is_webkit_helper(const char *path, const char *source_dir, const char *name)
{
    char expected[PATH_MAX];
    int length = snprintf(expected, sizeof(expected), "%s/%s", source_dir, name);
    return length > 0 && (size_t)length < sizeof(expected) && !strcmp(path, expected);
}

GSubprocess *g_subprocess_launcher_spawnv(
    GSubprocessLauncher *launcher,
    const char *const *argv,
    GError **error)
{
    union {
        void *object;
        spawnv_fn function;
    } next = { .object = dlsym(RTLD_NEXT, "g_subprocess_launcher_spawnv") };
    if (!next.function) {
        errno = ENOSYS;
        return NULL;
    }
    if (!argv || !argv[0])
        return next.function(launcher, argv, error);

    const char *appdir = getenv("APPDIR");
    const char *source_dir = getenv("TAMIOPS_WEBKIT_SOURCE_EXEC_DIR");
    const char *appdir_exec_dir = getenv("WEBKIT_EXEC_PATH");
    if (!appdir || !source_dir || !appdir_exec_dir)
        return next.function(launcher, argv, error);
    size_t appdir_length = strlen(appdir);
    if (strncmp(appdir_exec_dir, appdir, appdir_length) || appdir_exec_dir[appdir_length] != '/')
        return next.function(launcher, argv, error);

    const char *names[] = {
        "WebKitWebProcess",
        "WebKitNetworkProcess",
        "WebKitGPUProcess",
        NULL,
    };
    size_t helper_arg = SIZE_MAX;
    const char *helper_name = NULL;
    size_t count = 0;
    while (argv[count])
        count++;

    // The trusted NetworkProcess is spawned directly; web and GPU processes
    // are the executable immediately following bubblewrap's `--` marker.
    for (size_t k = 0; names[k]; k++) {
        if (is_webkit_helper(argv[0], source_dir, names[k])) {
            helper_arg = 0;
            helper_name = names[k];
            break;
        }
    }
    for (size_t i = 0; i + 1 < count; i++) {
        if (helper_name)
            break;
        if (strcmp(argv[i], "--"))
            continue;
        size_t j = i + 1;
        if (j < count) {
            for (size_t k = 0; names[k]; k++) {
                if (is_webkit_helper(argv[j], source_dir, names[k])) {
                    helper_arg = j;
                    helper_name = names[k];
                    break;
                }
            }
        }
        break;
    }
    if (helper_arg == SIZE_MAX)
        return next.function(launcher, argv, error);

    char replacement[PATH_MAX];
    // WEBKIT_EXEC_PATH already names the bundled helper directory. Use it as
    // the destination while the source path check above limits rewrites to the
    // three known WebKit subprocesses only.
    int length = snprintf(replacement, sizeof(replacement), "%s/%s", appdir_exec_dir, helper_name);
    if (length < 0 || (size_t)length >= sizeof(replacement))
        return next.function(launcher, argv, error);

    const char **rewritten = malloc((count + 1) * sizeof(*rewritten));
    if (!rewritten)
        return next.function(launcher, argv, error);
    memcpy(rewritten, argv, count * sizeof(*rewritten));
    rewritten[count] = NULL;
    rewritten[helper_arg] = replacement;

    GSubprocess *result = next.function(launcher, rewritten, error);
    free(rewritten);
    return result;
}
