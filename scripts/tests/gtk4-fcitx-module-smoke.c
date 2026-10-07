#include <gtk/gtk.h>

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static gboolean mapped_from_appdir(const char *appdir, const char *soname) {
    FILE *maps = fopen("/proc/self/maps", "r");
    if (maps == NULL) {
        perror("open /proc/self/maps");
        return FALSE;
    }

    char *line = NULL;
    size_t capacity = 0;
    gboolean found = FALSE;
    while (getline(&line, &capacity, maps) >= 0) {
        if (strstr(line, appdir) != NULL && strstr(line, soname) != NULL) {
            found = TRUE;
            break;
        }
    }
    free(line);
    fclose(maps);
    return found;
}

int main(int argc, char **argv) {
    if (argc != 2) {
        fprintf(stderr, "usage: %s APPDIR\n", argv[0]);
        return 2;
    }

    const char *appdir = argv[1];
    char *gtk_path = g_build_filename(appdir, "usr", "lib", "gtk-4.0", NULL);
    char *gtk_exe_prefix = g_build_filename(appdir, "usr", NULL);
    g_setenv("GTK_PATH", gtk_path, TRUE);
    g_setenv("GTK_EXE_PREFIX", gtk_exe_prefix, TRUE);
    g_setenv("GDK_BACKEND", "x11", TRUE);
    g_free(gtk_path);
    g_free(gtk_exe_prefix);

    gtk_init();
    GtkWidget *entry = gtk_entry_new();
    g_object_ref_sink(entry);
    GtkIMContext *context = gtk_im_multicontext_new();
    gtk_im_context_set_client_widget(context, entry);
    gtk_im_multicontext_set_context_id(GTK_IM_MULTICONTEXT(context), "fcitx");
    // GtkIMMulticontext replaces its delegate lazily when the context id is
    // set. Focus-in asks it to create and activate the selected delegate.
    gtk_im_context_focus_in(context);

    if (!mapped_from_appdir(appdir, "libgtk-4.so.1")) {
        fprintf(stderr, "GTK was not loaded from the AppImage\n");
        return 1;
    }

    if (!mapped_from_appdir(appdir, "libim-fcitx5.so")) {
        fprintf(stderr, "GTK did not load the Fcitx5 IM module from the AppImage\n");
        return 1;
    }
    if (!mapped_from_appdir(appdir, "libFcitx5GClient.so")) {
        fprintf(stderr, "Fcitx5 GTK4 module did not load its bundled client library\n");
        return 1;
    }

    g_object_unref(context);
    g_object_unref(entry);
    puts("AppImage GTK4 loaded its bundled Fcitx5 input module and client library.");
    return 0;
}
