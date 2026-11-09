package servernode

import (
	"strings"

	"rsc.io/qr"
)

// qrText renders a QR code as text: two module rows per terminal row with half blocks, so the
// modules look square. It returns nil when the code does not fit the width (the caller shows the
// link alone then). No colour and no emoji: the blocks are plain Unicode and keep their width.
func qrText(s string, width int) []string {
	if s == "" || width < 8 {
		return nil
	}
	code, err := qr.Encode(s, qr.L)
	if err != nil {
		return nil
	}
	size := code.Size + 4 // four modules of quiet zone
	if size > width {
		return nil
	}
	dark := func(x, y int) bool {
		x, y = x-2, y-2
		if x < 0 || y < 0 || x >= code.Size || y >= code.Size {
			return false
		}
		return code.Black(x, y)
	}
	var out []string
	pad := strings.Repeat(" ", 4)
	for y := 0; y < size; y += 2 {
		var b strings.Builder
		b.WriteString(pad)
		for x := 0; x < size; x++ {
			top, bottom := dark(x, y), false
			if y+1 < size {
				bottom = dark(x, y+1)
			}
			switch {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		out = append(out, b.String())
	}
	return out
}
