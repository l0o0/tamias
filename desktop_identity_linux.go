//go:build linux && cgo

package main

/*
#cgo pkg-config: gtk4
#include <gtk/gtk.h>

static void tamias_configure_desktop_identity(void) {
    g_set_application_name("小花鼠");
    gtk_window_set_default_icon_name("io.tamiops.tami");
}
*/
import "C"

// GTK 4.14 does not derive its window icon from the application id, and Wails'
// byte-based Icon option is a no-op on GTK4. Set the themed fallback before any
// window is created; AppImage supplies it through its private icon search path.
func configureDesktopIdentity() {
	C.tamias_configure_desktop_identity()
}
