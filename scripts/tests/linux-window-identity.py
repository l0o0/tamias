#!/usr/bin/env python3
"""Check a running Linux X11 window's application identity and icon."""

import argparse
from array import array
import ctypes
import ctypes.util
import os
import struct
import sys
import zlib
from pathlib import Path

TITLE = "小花鼠"
APP_ID = "io.tamiops.tami"
WM_CLASS = "Tamias"
MAX_WINDOWS = 100_000
ICON_UNITS = 2 * 1024 * 1024
MAX_ICON_PIXELS = 1024 * 1024


class X11:
    def __init__(self):
        libname = ctypes.util.find_library("X11")
        if not libname:
            raise RuntimeError("libX11 was not found")
        self.lib = ctypes.CDLL(libname)
        self._declare()
        self.display = self.lib.XOpenDisplay(None)
        if not self.display:
            raise RuntimeError(f"cannot open X11 display {os.environ.get('DISPLAY', '(unset)')}")
        self.atoms = {}

    def _declare(self):
        lib = self.lib
        display, window, atom = ctypes.c_void_p, ctypes.c_ulong, ctypes.c_ulong
        lib.XOpenDisplay.argtypes = [ctypes.c_char_p]
        lib.XOpenDisplay.restype = display
        lib.XCloseDisplay.argtypes = [display]
        lib.XCloseDisplay.restype = ctypes.c_int
        lib.XDefaultScreen.argtypes = [display]
        lib.XDefaultScreen.restype = ctypes.c_int
        lib.XRootWindow.argtypes = [display, ctypes.c_int]
        lib.XRootWindow.restype = window
        lib.XInternAtom.argtypes = [display, ctypes.c_char_p, ctypes.c_int]
        lib.XInternAtom.restype = atom
        lib.XQueryTree.argtypes = [display, window, ctypes.POINTER(window), ctypes.POINTER(window),
                                   ctypes.POINTER(ctypes.POINTER(window)), ctypes.POINTER(ctypes.c_uint)]
        lib.XQueryTree.restype = ctypes.c_int
        lib.XGetWindowProperty.argtypes = [display, window, atom, ctypes.c_long, ctypes.c_long,
                                           ctypes.c_int, atom, ctypes.POINTER(atom),
                                           ctypes.POINTER(ctypes.c_int), ctypes.POINTER(ctypes.c_ulong),
                                           ctypes.POINTER(ctypes.c_ulong),
                                           ctypes.POINTER(ctypes.POINTER(ctypes.c_ubyte))]
        lib.XGetWindowProperty.restype = ctypes.c_int
        lib.XFree.argtypes = [ctypes.c_void_p]
        lib.XFree.restype = ctypes.c_int

    def close(self):
        if self.display:
            self.lib.XCloseDisplay(self.display)
            self.display = None

    def atom(self, name):
        if name not in self.atoms:
            self.atoms[name] = int(self.lib.XInternAtom(self.display, name.encode(), 0))
        return self.atoms[name]

    def prop(self, wid, name, units):
        actual_type, fmt = ctypes.c_ulong(), ctypes.c_int()
        count, after = ctypes.c_ulong(), ctypes.c_ulong()
        data = ctypes.POINTER(ctypes.c_ubyte)()
        status = self.lib.XGetWindowProperty(self.display, wid, self.atom(name), 0, units, 0, 0,
                                             ctypes.byref(actual_type), ctypes.byref(fmt),
                                             ctypes.byref(count), ctypes.byref(after), ctypes.byref(data))
        try:
            if status != 0:
                raise RuntimeError(f"could not read {name}")
            if not actual_type.value:
                return None
            if after.value:
                raise RuntimeError(f"{name} exceeds its {units}-unit limit")
            if fmt.value == 8:
                values = ctypes.string_at(data, count.value)
            elif fmt.value == 32:
                values = array("L")
                if values.itemsize != ctypes.sizeof(ctypes.c_ulong):
                    raise RuntimeError("native unsigned-long size does not match Xlib")
                values.frombytes(ctypes.string_at(data, count.value * values.itemsize))
            else:
                raise RuntimeError(f"{name} has unsupported format {fmt.value}")
            return int(actual_type.value), fmt.value, values
        finally:
            if data:
                self.lib.XFree(ctypes.cast(data, ctypes.c_void_p))

    def text(self, wid, name, type_name=None):
        prop = self.prop(wid, name, 1024)
        if not prop or prop[1] != 8:
            return None
        if type_name and prop[0] != self.atom(type_name):
            return None
        return prop[2].rstrip(b"\0").decode("utf-8", "replace")

    def class_names(self, wid):
        prop = self.prop(wid, "WM_CLASS", 1024)
        if not prop or prop[1] != 8 or prop[0] != self.atom("STRING"):
            return "", ""
        parts = prop[2].split(b"\0")
        names = [part.decode("utf-8", "replace") for part in parts if part]
        return (names + ["", ""])[:2]

    def children(self, wid):
        root, parent = ctypes.c_ulong(), ctypes.c_ulong()
        children = ctypes.POINTER(ctypes.c_ulong)()
        count = ctypes.c_uint()
        ok = self.lib.XQueryTree(self.display, wid, ctypes.byref(root), ctypes.byref(parent),
                                 ctypes.byref(children), ctypes.byref(count))
        try:
            if not ok or count.value > MAX_WINDOWS:
                return []
            return [int(children[i]) for i in range(count.value)] if children else []
        finally:
            if children:
                self.lib.XFree(ctypes.cast(children, ctypes.c_void_p))

    def find_window(self, pid):
        root = int(self.lib.XRootWindow(self.display, self.lib.XDefaultScreen(self.display)))
        queue, visited = [root], set()
        cardinal = self.atom("CARDINAL")
        while queue and len(visited) < MAX_WINDOWS:
            wid = queue.pop()
            if wid in visited:
                continue
            visited.add(wid)
            try:
                prop = self.prop(wid, "_NET_WM_PID", 1)
                if prop and prop[0] == cardinal and prop[1] == 32 and prop[2] and prop[2][0] == pid:
                    title = self.text(wid, "_NET_WM_NAME", "UTF8_STRING")
                    if title is None:
                        title = self.text(wid, "WM_NAME")
                    if title == TITLE:
                        return wid
            except RuntimeError:
                pass
            room = MAX_WINDOWS - len(visited) - len(queue)
            if room > 0:
                queue.extend(self.children(wid)[:room])
        return None

def choose_icon(values):
    candidates, pos = [], 0
    while pos < len(values):
        if pos + 2 > len(values):
            raise RuntimeError("_NET_WM_ICON has an incomplete image header")
        width, height = int(values[pos]), int(values[pos + 1])
        pos += 2
        count = width * height
        if not width or not height or width > 4096 or height > 4096 or count > MAX_ICON_PIXELS or pos + count > len(values):
            raise RuntimeError(f"_NET_WM_ICON has invalid dimensions or data: {width}x{height}")
        brown = transparent = 0
        for i in range(pos, pos + count):
            p = int(values[i]) & 0xffffffff
            a, r, g, b = p >> 24, (p >> 16) & 255, (p >> 8) & 255, p & 255
            transparent += a <= 16
            brown += a >= 192 and r >= 48 and r - g >= 8 and g - b >= 4 and r > g > b
        if transparent and brown >= max(12, count // 10):
            pixels = [int(p) & 0xffffffff for p in values[pos:pos + count]]
            candidates.append((count, width, height, pixels, brown, transparent))
        pos += count
    if not candidates:
        raise RuntimeError("_NET_WM_ICON lacks a transparent image with enough warm-brown pixels")
    _, width, height, pixels, brown, transparent = max(candidates, key=lambda item: item[0])
    return width, height, pixels, brown, transparent

def png_chunk(kind, data):
    body = kind + data
    return struct.pack(">I", len(data)) + body + struct.pack(">I", zlib.crc32(body) & 0xffffffff)

def icon_png(width, height, pixels):
    rows = bytearray()
    for y in range(height):
        rows.append(0)
        for p in pixels[y * width:(y + 1) * width]:
            rows.extend(((p >> 16) & 255, (p >> 8) & 255, p & 255, p >> 24))
    header = struct.pack(">IIBBBBB", width, height, 8, 6, 0, 0, 0)
    return (b"\x89PNG\r\n\x1a\n" + png_chunk(b"IHDR", header)
            + png_chunk(b"IDAT", zlib.compress(rows, 9)) + png_chunk(b"IEND", b""))

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pid", type=int, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if args.pid <= 0:
        parser.error("--pid must be positive")
    x11 = None
    try:
        x11 = X11()
        wid = x11.find_window(args.pid)
        if wid is None:
            raise RuntimeError(f"no window for PID {args.pid} has title {TITLE!r}")
        instance, klass = x11.class_names(wid)
        if "wails" in (instance + " " + klass).lower() or klass != WM_CLASS:
            raise RuntimeError(f"WM_CLASS=({instance!r}, {klass!r}), expected class {WM_CLASS!r} without wails")
        app_id = x11.text(wid, "_GTK_APPLICATION_ID", "UTF8_STRING")
        if app_id != APP_ID:
            raise RuntimeError(f"_GTK_APPLICATION_ID={app_id!r}, expected {APP_ID!r}")
        prop = x11.prop(wid, "_NET_WM_ICON", ICON_UNITS)
        if not prop or prop[0] != x11.atom("CARDINAL") or prop[1] != 32:
            raise RuntimeError("_NET_WM_ICON is missing or not CARDINAL/32")
        width, height, pixels, brown, transparent = choose_icon(prop[2])
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_bytes(icon_png(width, height, pixels))
        wm_icon_name = x11.text(wid, "WM_ICON_NAME")
        net_icon_name = x11.text(wid, "_NET_WM_ICON_NAME", "UTF8_STRING")
        print(f"window=0x{wid:x} pid={args.pid} title={TITLE!r} WM_CLASS=({instance!r}, {klass!r}) "
              f"_GTK_APPLICATION_ID={app_id!r} WM_ICON_NAME={wm_icon_name!r} "
              f"_NET_WM_ICON_NAME={net_icon_name!r} icon={width}x{height} "
              f"brown={brown} transparent={transparent} output={args.output}")
        return 0
    except (OSError, RuntimeError) as exc:
        print(f"linux-window-identity: {exc}", file=sys.stderr)
        return 1
    finally:
        if x11 is not None:
            x11.close()

if __name__ == "__main__":
    sys.exit(main())
