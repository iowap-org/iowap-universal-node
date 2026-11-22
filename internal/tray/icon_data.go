package tray

import _ "embed"

// trayIconICO is the 32x32 tray icon (simple bridge glyph, generated —
// no design asset dependency).
//
//go:embed icon.ico
var trayIconICO []byte
