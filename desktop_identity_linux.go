//go:build linux && cgo

package main

/*
#cgo pkg-config: gtk4
#include <gtk/gtk.h>

static void tamias_configure_desktop_identity(void) {
    g_set_application_name("小花鼠");
}

static void tamias_configure_window_icons(void) {
    gtk_window_set_default_icon_name("io.tamiops.tami");
    // Startup and pending window creation can run in either order. Apply the
    // icon to existing windows too; the default covers windows created later.
    GList *windows = gtk_window_list_toplevels();
    for (GList *item = windows; item != NULL; item = item->next) {
        gtk_window_set_icon_name(GTK_WINDOW(item->data), "io.tamiops.tami");
    }
    g_list_free(windows);
}
*/
import "C"

func configureDesktopIdentity() {
	C.tamias_configure_desktop_identity()
}

// Called on the GTK thread after startup. Wails' byte-based Icon option is a
// no-op on GTK4; AppImage supplies this named icon in its private hicolor theme.
func configureWindowIcons() {
	C.tamias_configure_window_icons()
}
