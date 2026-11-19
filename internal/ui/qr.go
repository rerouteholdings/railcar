package ui

import (
	"strings"

	"rsc.io/qr"
)

// qrQuiet is the quiet zone around a QR code, in modules.
const qrQuiet = 2

// renderQR draws text as a QR code with half-block characters, two
// modules to a character cell. Scanners need dark modules on a light
// background, so on a dark terminal the light modules and the quiet zone
// are drawn, and on a light terminal the dark ones are. It returns "" when
// the text doesn't fit in a QR code.
func renderQR(text string, darkTerminal bool) string {
	code, err := qr.Encode(text, qr.L)
	if err != nil {
		return ""
	}
	n := code.Size
	// drawn reports whether the module at x, y gets ink in this terminal.
	drawn := func(x, y int) bool {
		x, y = x-qrQuiet, y-qrQuiet
		black := x >= 0 && y >= 0 && x < n && y < n && code.Black(x, y)
		return black != darkTerminal
	}
	size := n + 2*qrQuiet
	var b strings.Builder
	for y := 0; y < size; y += 2 {
		for x := range size {
			top, bottom := drawn(x, y), y+1 < size && drawn(x, y+1)
			switch {
			case top && bottom:
				b.WriteRune('█')
			case top:
				b.WriteRune('▀')
			case bottom:
				b.WriteRune('▄')
			default:
				b.WriteByte(' ')
			}
		}
		if y+2 < size {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// qrSize returns how many columns and rows renderQR uses for text.
func qrSize(text string) (cols, rows int) {
	code, err := qr.Encode(text, qr.L)
	if err != nil {
		return 0, 0
	}
	size := code.Size + 2*qrQuiet
	return size, (size + 1) / 2
}
