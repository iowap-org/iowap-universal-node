package tray

import _ "embed"

// trayIconICO is the IOWAP tray icon (v0.3.1, Ronny feedback #1): the
// final multi-size ICO (16–256 px, vector rebuild of the approved mflux
// concept — host tower in cornflower blue, cluster tower in lavender,
// segmented arch on a dark plate).
//
// The canonical asset lives at assets/iowap.ico (repo root — it is also
// compiled into rsrc_windows_amd64.syso for the Explorer/Taskbar exe
// icon). go:embed cannot reference parent directories, so the tray embeds
// a byte-identical copy here; internal/tray/icon_test.go guards the two
// files against drift.
//
//go:embed icon.ico
var trayIconICO []byte
