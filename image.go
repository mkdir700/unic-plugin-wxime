package wxime

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"math"

	"github.com/gen2brain/h265/heic"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	// maxDecodePixels bounds the decoded input area so high-resolution mobile
	// photos (e.g. 12MP/24MP/48MP iPhone HEIC) can be decoded and downsampled
	// safely while blocking extreme decompression bombs.
	maxDecodePixels = 100_000_000

	// maxEncodeAttempts bounds how many times an oversized PNG is scaled down
	// to fit within maxImage.
	maxEncodeAttempts = 4
)

var (
	pngMagic = []byte("\x89PNG\r\n\x1a\n")

	errImageUnsupportedFormat = errors.New("wxime: unsupported image format")
	errImageTooLarge          = errors.New("wxime: image exceeds size limits")
	errImageDecodeFailed      = errors.New("wxime: image decode failed")
	errImageEncodeFailed      = errors.New("wxime: image encode failed")
)

// convertImageToPNG validates and converts an inbound image payload into a PNG
// that fits within maxPixels and maxImage.
func convertImageToPNG(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errImageUnsupportedFormat
	}
	if bytes.HasPrefix(data, pngMagic) {
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err == nil && cfg.Width > 0 && cfg.Height > 0 &&
			int64(cfg.Width) <= int64(maxPixels)/int64(cfg.Height) &&
			len(data) <= maxImage {
			return data, nil
		}
	}
	return decodePlatformImageToPNG(data, maxPixels, maxImage)
}

func detectImageFormatHint(data []byte) string {
	switch {
	case bytes.HasPrefix(data, pngMagic):
		return "png"
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")):
		return "gif"
	case len(data) >= 12 && bytes.Equal(data[0:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "webp"
	case isISOBMFF(data):
		brand := string(data[8:12])
		switch brand {
		case "heic", "heix", "hevc", "hevx", "mif1", "msf1", "heif", "heim", "heis":
			return "heic"
		case "avif", "avis":
			return "avif"
		default:
			return "isobmff"
		}
	case len(data) >= 4 && ((data[0] == 'I' && data[1] == 'I' && data[2] == 42 && data[3] == 0) ||
		(data[0] == 'M' && data[1] == 'M' && data[2] == 0 && data[3] == 42)):
		return "tiff"
	case len(data) >= 2 && data[0] == 'B' && data[1] == 'M':
		return "bmp"
	default:
		return "unknown"
	}
}

func isISOBMFF(data []byte) bool {
	return len(data) >= 12 && bytes.Equal(data[4:8], []byte("ftyp"))
}

func fitDimensions(w, h int, pixelBudget int64) (int, int) {
	if w <= 0 || h <= 0 || pixelBudget <= 0 {
		return 0, 0
	}
	if int64(w) <= pixelBudget/int64(h) {
		return w, h
	}
	scale := math.Sqrt(float64(pixelBudget) / (float64(w) * float64(h)))
	nw := max(1, int(math.Floor(float64(w)*scale)))
	nh := max(1, int(math.Floor(float64(h)*scale)))
	for int64(nw) > pixelBudget/int64(nh) {
		if nw >= nh && nw > 1 {
			nw--
		} else if nh > 1 {
			nh--
		} else {
			break
		}
	}
	return nw, nh
}

func shrinkDimensionsForBytes(w, h, gotBytes, budgetBytes int) (int, int) {
	if w <= 1 && h <= 1 {
		return 0, 0
	}
	ratio := math.Sqrt(float64(budgetBytes) * 0.85 / float64(gotBytes))
	if ratio > 0.85 {
		ratio = 0.85
	}
	if ratio < 0.25 {
		ratio = 0.25
	}
	nw := max(1, int(math.Floor(float64(w)*ratio)))
	nh := max(1, int(math.Floor(float64(h)*ratio)))
	if nw == w && nh == h {
		if w >= h && nw > 1 {
			nw--
		} else if nh > 1 {
			nh--
		}
	}
	return nw, nh
}

func decodeImageToPNGPureGo(data []byte, pixelBudget int64, byteBudget int) ([]byte, error) {
	img, err := decodeImagePureGo(data)
	if err != nil {
		return nil, err
	}
	img = normalize8Bit(img)
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= 0 || h <= 0 {
		return nil, errImageDecodeFailed
	}
	targetW, targetH := fitDimensions(w, h, pixelBudget)
	if targetW != w || targetH != h {
		img = resizeImage(img, targetW, targetH)
		w, h = targetW, targetH
	}
	return encodePNGWithinBudget(img, byteBudget)
}

func decodeImagePureGo(data []byte) (image.Image, error) {
	if isISOBMFF(data) {
		cfg, err := heic.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			if errors.Is(err, heic.ErrUnsupported) {
				return nil, fmt.Errorf("%w: %v", errImageUnsupportedFormat, err)
			}
			return nil, fmt.Errorf("%w: %v", errImageDecodeFailed, err)
		}
		if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width) > maxDecodePixels/int64(cfg.Height) {
			return nil, errImageTooLarge
		}
		img, err := heic.Decode(bytes.NewReader(data), heic.Options{
			AutoRotate:     true,
			FrameSizeLimit: maxDecodePixels,
		})
		if err != nil {
			if errors.Is(err, heic.ErrUnsupported) {
				return nil, fmt.Errorf("%w: %v", errImageUnsupportedFormat, err)
			}
			return nil, fmt.Errorf("%w: %v", errImageDecodeFailed, err)
		}
		return img, nil
	}

	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		if errors.Is(err, image.ErrFormat) {
			return nil, errImageUnsupportedFormat
		}
		return nil, fmt.Errorf("%w: %v", errImageDecodeFailed, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width) > maxDecodePixels/int64(cfg.Height) {
		return nil, errImageTooLarge
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errImageDecodeFailed, err)
	}
	if format == "jpeg" {
		if orient := jpegExifOrientation(data); orient >= 2 && orient <= 8 {
			img = applyExifOrientation(img, orient)
		}
	}
	return img, nil
}

func encodePNGWithinBudget(img image.Image, byteBudget int) ([]byte, error) {
	cur := img
	for attempt := 0; attempt < maxEncodeAttempts; attempt++ {
		var out bytes.Buffer
		enc := png.Encoder{CompressionLevel: png.DefaultCompression}
		if err := enc.Encode(&out, cur); err != nil {
			return nil, fmt.Errorf("%w: %v", errImageEncodeFailed, err)
		}
		if out.Len() <= byteBudget {
			return out.Bytes(), nil
		}
		b := cur.Bounds()
		nw, nh := shrinkDimensionsForBytes(b.Dx(), b.Dy(), out.Len(), byteBudget)
		if nw <= 0 || nh <= 0 {
			break
		}
		cur = resizeImage(img, nw, nh)
	}
	return nil, errImageTooLarge
}

// normalize8Bit down-converts 16-bit images (such as 10-bit HEIC decoded as
// NRGBA64) to 8-bit NRGBA so PNG output stays compact and universally compatible.
func normalize8Bit(img image.Image) image.Image {
	switch src := img.(type) {
	case *image.NRGBA64:
		b := src.Bounds()
		dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
		for y := 0; y < b.Dy(); y++ {
			so := (y + b.Min.Y - src.Rect.Min.Y) * src.Stride
			do := y * dst.Stride
			for x := 0; x < b.Dx(); x++ {
				si := so + (x+b.Min.X-src.Rect.Min.X)*8
				di := do + x*4
				dst.Pix[di+0] = src.Pix[si+0]
				dst.Pix[di+1] = src.Pix[si+2]
				dst.Pix[di+2] = src.Pix[si+4]
				dst.Pix[di+3] = src.Pix[si+6]
			}
		}
		return dst
	case *image.RGBA64, *image.Gray16:
		b := img.Bounds()
		dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
		draw.Copy(dst, image.Point{}, img, b, draw.Src, nil)
		return dst
	default:
		return img
	}
}

func resizeImage(src image.Image, dstW, dstH int) image.Image {
	dst := image.NewNRGBA(image.Rect(0, 0, dstW, dstH))
	draw.BiLinear.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	return dst
}

// jpegExifOrientation extracts the TIFF/EXIF Orientation tag (1..8) from a
// JPEG APP1 segment, returning 1 when absent or unreadable.
func jpegExifOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return 1
	}
	i := 2
	for i+4 <= len(data) {
		if data[i] != 0xff {
			return 1
		}
		for i < len(data) && data[i] == 0xff {
			i++
		}
		if i >= len(data) {
			return 1
		}
		marker := data[i]
		i++
		if marker == 0xd9 || marker == 0xda { // EOI or SOS
			return 1
		}
		if marker == 0x01 || (marker >= 0xd0 && marker <= 0xd7) {
			continue
		}
		if i+2 > len(data) {
			return 1
		}
		segLen := int(binary.BigEndian.Uint16(data[i : i+2]))
		if segLen < 2 || i+segLen > len(data) {
			return 1
		}
		seg := data[i+2 : i+segLen]
		if marker == 0xe1 && len(seg) > 14 && bytes.HasPrefix(seg, []byte("Exif\x00\x00")) {
			if orient := parseTiffOrientation(seg[6:]); orient >= 1 && orient <= 8 {
				return orient
			}
		}
		i += segLen
	}
	return 1
}

func parseTiffOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch {
	case tiff[0] == 'I' && tiff[1] == 'I':
		order = binary.LittleEndian
	case tiff[0] == 'M' && tiff[1] == 'M':
		order = binary.BigEndian
	default:
		return 1
	}
	if order.Uint16(tiff[2:4]) != 42 {
		return 1
	}
	ifdOff := int(order.Uint32(tiff[4:8]))
	if ifdOff < 8 || ifdOff+2 > len(tiff) {
		return 1
	}
	entries := int(order.Uint16(tiff[ifdOff : ifdOff+2]))
	pos := ifdOff + 2
	for n := 0; n < entries && pos+12 <= len(tiff); n++ {
		tag := order.Uint16(tiff[pos : pos+2])
		if tag == 0x0112 { // Orientation
			typ := order.Uint16(tiff[pos+2 : pos+4])
			cnt := order.Uint32(tiff[pos+4 : pos+8])
			if typ == 3 && cnt >= 1 {
				return int(order.Uint16(tiff[pos+8 : pos+10]))
			}
			return 1
		}
		pos += 12
	}
	return 1
}

func applyExifOrientation(src image.Image, orient int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dstW, dstH := w, h
	if orient >= 5 && orient <= 8 {
		dstW, dstH = h, w
	}
	dst := image.NewNRGBA(image.Rect(0, 0, dstW, dstH))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.NRGBAModel.Convert(src.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
			var dx, dy int
			switch orient {
			case 2: // flip horizontal
				dx, dy = w-1-x, y
			case 3: // rotate 180
				dx, dy = w-1-x, h-1-y
			case 4: // flip vertical
				dx, dy = x, h-1-y
			case 5: // transpose
				dx, dy = y, x
			case 6: // rotate 90 CW
				dx, dy = h-1-y, x
			case 7: // transverse
				dx, dy = h-1-y, w-1-x
			case 8: // rotate 270 CW (90 CCW)
				dx, dy = y, w-1-x
			default:
				dx, dy = x, y
			}
			dst.SetNRGBA(dx, dy, c)
		}
	}
	return dst
}
