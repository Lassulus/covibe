package dashboard

import qrcode "github.com/skip2/go-qrcode"

// qrPNG encodes data as a QR-code PNG of the given pixel size.
func qrPNG(data string, size int) ([]byte, error) {
	if size <= 0 {
		size = 256
	}
	return qrcode.Encode(data, qrcode.Medium, size)
}
