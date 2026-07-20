package dashboard

import (
	"encoding/base64"

	qrcode "github.com/skip2/go-qrcode"
)

// qrPNG encodes data as a QR-code PNG of the given pixel size.
func qrPNG(data string, size int) ([]byte, error) {
	if size <= 0 {
		size = 256
	}
	return qrcode.Encode(data, qrcode.Medium, size)
}

// qrDataURI returns a base64 data: URI for embedding a QR PNG inline in HTML.
func qrDataURI(data string, size int) string {
	png, err := qrPNG(data, size)
	if err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}
