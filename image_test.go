package wxime

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gen2brain/h265/heic"
	sdk "github.com/mkdir700/wx-ime-sdk/go"
)

func makeQuadrantImage(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var c color.NRGBA
			switch {
			case x < w/2 && y < h/2:
				c = color.NRGBA{R: 230, G: 30, B: 30, A: 255} // red top-left
			case x >= w/2 && y < h/2:
				c = color.NRGBA{R: 30, G: 210, B: 30, A: 255} // green top-right
			case x < w/2 && y >= h/2:
				c = color.NRGBA{R: 30, G: 30, B: 230, A: 255} // blue bottom-left
			default:
				c = color.NRGBA{R: 240, G: 240, B: 240, A: 255} // white bottom-right
			}
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

func makeNoisyImage(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewPCG(42, 99))
	for i := 0; i < len(img.Pix); i += 4 {
		v := rng.Uint32()
		img.Pix[i+0] = byte(v)
		img.Pix[i+1] = byte(v >> 8)
		img.Pix[i+2] = byte(v >> 16)
		img.Pix[i+3] = 255
	}
	return img
}

func injectJPEGExifOrientation(t *testing.T, jpg []byte, orientation uint16) []byte {
	t.Helper()
	if len(jpg) < 4 || jpg[0] != 0xff || jpg[1] != 0xd8 {
		t.Fatal("not a JPEG")
	}
	// Minimal TIFF (big-endian) with 1 IFD0 entry: tag 0x0112 (Orientation), type SHORT (3), count 1.
	var tiff bytes.Buffer
	tiff.WriteString("MM")
	_ = binary.Write(&tiff, binary.BigEndian, uint16(42))
	_ = binary.Write(&tiff, binary.BigEndian, uint32(8))
	_ = binary.Write(&tiff, binary.BigEndian, uint16(1))      // entry count
	_ = binary.Write(&tiff, binary.BigEndian, uint16(0x0112)) // Orientation
	_ = binary.Write(&tiff, binary.BigEndian, uint16(3))      // SHORT
	_ = binary.Write(&tiff, binary.BigEndian, uint32(1))      // count
	_ = binary.Write(&tiff, binary.BigEndian, orientation)    // value
	_ = binary.Write(&tiff, binary.BigEndian, uint16(0))      // padding
	_ = binary.Write(&tiff, binary.BigEndian, uint32(0))      // next IFD offset

	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	var out bytes.Buffer
	out.Write(jpg[:2])
	out.Write([]byte{0xff, 0xe1})
	_ = binary.Write(&out, binary.BigEndian, uint16(len(payload)+2))
	out.Write(payload)
	out.Write(jpg[2:])
	return out.Bytes()
}

func decodePNGOrFail(t *testing.T, data []byte) image.Image {
	t.Helper()
	if !bytes.HasPrefix(data, pngMagic) {
		t.Fatalf("output is not PNG (len=%d, prefix=%x)", len(data), data[:min(len(data), 8)])
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("png.Decode failed: %v", err)
	}
	return img
}

func assertRedDominant(t *testing.T, label string, c color.Color) {
	t.Helper()
	n := color.NRGBAModel.Convert(c).(color.NRGBA)
	if n.R < 150 || n.G > 100 || n.B > 100 {
		t.Fatalf("%s: expected red-dominant pixel, got %+v", label, n)
	}
}

func TestConvertImagePNGPassthrough(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, makeQuadrantImage(8, 8)); err != nil {
		t.Fatal(err)
	}
	got, err := convertImageToPNG(buf.Bytes())
	if err != nil {
		t.Fatalf("convertImageToPNG(png): %v", err)
	}
	if !bytes.Equal(got, buf.Bytes()) {
		t.Fatal("valid PNG within limits was unexpectedly re-encoded")
	}
}

func TestConvertImageHEIC(t *testing.T) {
	src := makeQuadrantImage(64, 48)
	var heicBuf bytes.Buffer
	if err := heic.Encode(&heicBuf, src, heic.EncodeOptions{Quality: 90}); err != nil {
		t.Fatalf("heic.Encode: %v", err)
	}
	if hint := detectImageFormatHint(heicBuf.Bytes()); hint != "heic" {
		t.Fatalf("format hint = %q, want heic", hint)
	}

	for _, tc := range []struct {
		name string
		fn   func([]byte) ([]byte, error)
	}{
		{"platform", convertImageToPNG},
		{"purego", func(b []byte) ([]byte, error) { return decodeImageToPNGPureGo(b, maxPixels, maxImage) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.fn(heicBuf.Bytes())
			if err != nil {
				t.Fatalf("%s HEIC decode failed: %v", tc.name, err)
			}
			img := decodePNGOrFail(t, out)
			if img.Bounds().Dx() != 64 || img.Bounds().Dy() != 48 {
				t.Fatalf("got %dx%d, want 64x48", img.Bounds().Dx(), img.Bounds().Dy())
			}
			assertRedDominant(t, tc.name+" top-left", img.At(8, 8))
		})
	}
}

func TestConvertImageHEICAlpha(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			a := uint8(255)
			if x < 16 {
				a = 0
			}
			src.SetNRGBA(x, y, color.NRGBA{R: 20, G: 180, B: 220, A: a})
		}
	}
	var heicBuf bytes.Buffer
	if err := heic.Encode(&heicBuf, src, heic.EncodeOptions{Quality: 90}); err != nil {
		t.Fatalf("heic.Encode alpha: %v", err)
	}
	for _, tc := range []struct {
		name string
		fn   func([]byte) ([]byte, error)
	}{
		{"platform", convertImageToPNG},
		{"purego", func(b []byte) ([]byte, error) { return decodeImageToPNGPureGo(b, maxPixels, maxImage) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.fn(heicBuf.Bytes())
			if err != nil {
				t.Fatalf("%s HEIC alpha decode failed: %v", tc.name, err)
			}
			img := decodePNGOrFail(t, out)
			left := color.NRGBAModel.Convert(img.At(4, 16)).(color.NRGBA)
			right := color.NRGBAModel.Convert(img.At(24, 16)).(color.NRGBA)
			if left.A > 20 || right.A < 235 {
				t.Fatalf("alpha not preserved: left=%+v right=%+v", left, right)
			}
		})
	}
}

func TestConvertImageAppleSipsHEIC(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Apple sips HEIC generation test runs on macOS")
	}
	sipsPath, err := exec.LookPath("sips")
	if err != nil {
		t.Skip("sips not available")
	}
	dir := t.TempDir()
	pngPath := filepath.Join(dir, "src.png")
	heicPath := filepath.Join(dir, "src.heic")

	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, makeQuadrantImage(80, 60)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pngPath, pngBuf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(sipsPath, "-s", "format", "heic", pngPath, "--out", heicPath).CombinedOutput(); err != nil {
		t.Fatalf("sips encode heic failed: %v\n%s", err, out)
	}
	heicData, err := os.ReadFile(heicPath)
	if err != nil {
		t.Fatal(err)
	}
	if hint := detectImageFormatHint(heicData); hint != "heic" {
		t.Fatalf("Apple HEIC hint = %q, want heic", hint)
	}

	for _, tc := range []struct {
		name string
		fn   func([]byte) ([]byte, error)
	}{
		{"darwin-imageio", convertImageToPNG},
		{"purego", func(b []byte) ([]byte, error) { return decodeImageToPNGPureGo(b, maxPixels, maxImage) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.fn(heicData)
			if err != nil {
				t.Fatalf("%s Apple HEIC decode failed: %v", tc.name, err)
			}
			img := decodePNGOrFail(t, out)
			if img.Bounds().Dx() != 80 || img.Bounds().Dy() != 60 {
				t.Fatalf("got %dx%d, want 80x60", img.Bounds().Dx(), img.Bounds().Dy())
			}
			assertRedDominant(t, tc.name+" top-left", img.At(10, 10))
		})
	}
}

func TestConvertImageJPEGExifOrientation(t *testing.T) {
	// Asymmetric 32x16 image with red top-left quadrant.
	// With EXIF orientation=6 (rotate 90 CW), result must be 16x32 with red at top-right!
	var jpgBuf bytes.Buffer
	if err := jpeg.Encode(&jpgBuf, makeQuadrantImage(32, 16), &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	rotated := injectJPEGExifOrientation(t, jpgBuf.Bytes(), 6)

	for _, tc := range []struct {
		name string
		fn   func([]byte) ([]byte, error)
	}{
		{"platform", convertImageToPNG},
		{"purego", func(b []byte) ([]byte, error) { return decodeImageToPNGPureGo(b, maxPixels, maxImage) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.fn(rotated)
			if err != nil {
				t.Fatalf("%s JPEG decode failed: %v", tc.name, err)
			}
			img := decodePNGOrFail(t, out)
			if img.Bounds().Dx() != 16 || img.Bounds().Dy() != 32 {
				t.Fatalf("got %dx%d, want 16x32 after EXIF orientation 6", img.Bounds().Dx(), img.Bounds().Dy())
			}
			assertRedDominant(t, tc.name+" top-right after 90CW", img.At(12, 4))
		})
	}
}

func TestConvertImageGIFAndWebP(t *testing.T) {
	var gifBuf bytes.Buffer
	if err := gif.Encode(&gifBuf, makeQuadrantImage(24, 16), nil); err != nil {
		t.Fatal(err)
	}
	// Valid 2x2 lossless VP8L WebP (top-left red, top-right green, bottom-left blue, bottom-right white).
	webpData, err := base64.StdEncoding.DecodeString("UklGRjQAAABXRUJQVlA4TCcAAAAvAUAAEAfQ//73v/+BiOh/AAA=")
	if err != nil {
		t.Fatal(err)
	}

	for _, sample := range []struct {
		name string
		data []byte
		hint string
	}{
		{"gif", gifBuf.Bytes(), "gif"},
		{"webp", webpData, "webp"},
	} {
		if got := detectImageFormatHint(sample.data); got != sample.hint {
			t.Fatalf("%s hint = %q, want %q", sample.name, got, sample.hint)
		}
		for _, dec := range []struct {
			name string
			fn   func([]byte) ([]byte, error)
		}{
			{"platform", convertImageToPNG},
			{"purego", func(b []byte) ([]byte, error) { return decodeImageToPNGPureGo(b, maxPixels, maxImage) }},
		} {
			t.Run(sample.name+"/"+dec.name, func(t *testing.T) {
				out, err := dec.fn(sample.data)
				if err != nil {
					t.Fatalf("%s %s decode failed: %v", sample.name, dec.name, err)
				}
				img := decodePNGOrFail(t, out)
				if img.Bounds().Dx() <= 0 || img.Bounds().Dy() <= 0 {
					t.Fatalf("invalid decoded bounds: %v", img.Bounds())
				}
			})
		}
	}
}

func TestDownsamplePixelAndByteBudgets(t *testing.T) {
	// 1. Pixel budget downsampling: 300x150 (45,000 px) -> budget 10,000 px.
	var jpgBuf bytes.Buffer
	if err := jpeg.Encode(&jpgBuf, makeQuadrantImage(300, 150), &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		fn   func([]byte, int64, int) ([]byte, error)
	}{
		{"platform", decodePlatformImageToPNG},
		{"purego", decodeImageToPNGPureGo},
	} {
		t.Run("pixels/"+tc.name, func(t *testing.T) {
			out, err := tc.fn(jpgBuf.Bytes(), 10_000, maxImage)
			if err != nil {
				t.Fatalf("pixel downsample failed: %v", err)
			}
			img := decodePNGOrFail(t, out)
			w, h := img.Bounds().Dx(), img.Bounds().Dy()
			if int64(w)*int64(h) > 10_000 || w <= h {
				t.Fatalf("got %dx%d (%d px), want <= 10000 px with landscape aspect", w, h, w*h)
			}
		})
	}

	// 2. Byte budget downsampling: noisy 512x512 image -> budget 96 KiB PNG.
	var noisyPNG bytes.Buffer
	if err := png.Encode(&noisyPNG, makeNoisyImage(512, 512)); err != nil {
		t.Fatal(err)
	}
	if noisyPNG.Len() <= 96<<10 {
		t.Fatalf("expected noisy PNG > 96 KiB, got %d", noisyPNG.Len())
	}
	const budget = 96 << 10
	for _, tc := range []struct {
		name string
		fn   func([]byte, int64, int) ([]byte, error)
	}{
		{"platform", decodePlatformImageToPNG},
		{"purego", decodeImageToPNGPureGo},
	} {
		t.Run("bytes/"+tc.name, func(t *testing.T) {
			out, err := tc.fn(noisyPNG.Bytes(), maxPixels, budget)
			if err != nil {
				t.Fatalf("byte budget downsample failed: %v", err)
			}
			if len(out) > budget {
				t.Fatalf("output PNG len=%d exceeds budget=%d", len(out), budget)
			}
			img := decodePNGOrFail(t, out)
			if img.Bounds().Dx() >= 512 || img.Bounds().Dx() < 64 {
				t.Fatalf("unexpected downsampled width=%d", img.Bounds().Dx())
			}
		})
	}
}

func TestConvertImageInvalidInputs(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"garbage", []byte("not an image at all")},
		{"truncated-heic", []byte{0x00, 0x00, 0x00, 0x28, 'f', 't', 'y', 'p', 'h', 'e', 'i', 'c', 0, 0, 0, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := convertImageToPNG(tc.data); err == nil {
				t.Fatal("expected error for invalid input")
			}
			if _, err := decodeImageToPNGPureGo(tc.data, maxPixels, maxImage); err == nil {
				t.Fatal("expected purego error for invalid input")
			}
		})
	}
}

func TestFitDimensions48MP(t *testing.T) {
	w, h := fitDimensions(8064, 6048, maxPixels)
	if int64(w)*int64(h) > maxPixels || w < 4000 || h < 3000 {
		t.Fatalf("48MP fitDimensions = %dx%d (%d px)", w, h, int64(w)*int64(h))
	}
}

func TestDiagnosticsClassificationAndDedupe(t *testing.T) {
	if got := classifyError(&sdk.BusinessError{Command: 1, Status: 10020}); got != "business_error_cmd1_status10020" {
		t.Fatalf("classifyError(BusinessError) = %q", got)
	}
	if got := classifyError(&sdk.HTTPError{Status: 502}); got != "http_502" {
		t.Fatalf("classifyError(HTTPError) = %q", got)
	}
	if got := classifyError(errImageUnsupportedFormat); got != "unsupported_image_format" {
		t.Fatalf("classifyError(unsupported) = %q", got)
	}

	logPath := filepath.Join(t.TempDir(), "diagnostics.log")
	d := &diagLog{path: logPath}
	d.recordEvent("poll_group:business_error_cmd1_status10020", "poll_group_failed", "class=business_error_cmd1_status10020")
	d.recordEvent("poll_group:business_error_cmd1_status10020", "poll_group_failed", "class=business_error_cmd1_status10020")
	d.clearDedupe("poll_")
	d.recordEvent("", "receive_rejected", "version=1907 format=heic bytes=84932 class=image_decode_failed")

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 diagnostic lines (deduped + recovered + rejected), got %d:\n%s", len(lines), raw)
	}
	if !strings.Contains(lines[0], "business_error_cmd1_status10020") ||
		!strings.Contains(lines[1], "poll_recovered") ||
		!strings.Contains(lines[2], "version=1907 format=heic") {
		t.Fatalf("unexpected diagnostic lines:\n%s", raw)
	}

	// Verify receivedPart surfaces errors instead of silently returning nil error.
	dir := t.TempDir()
	badFile := filepath.Join(dir, "bad.heic")
	badBytes := []byte{0x00, 0x00, 0x00, 0x28, 'f', 't', 'y', 'p', 'h', 'e', 'i', 'c', 0, 0, 0, 0}
	if err := os.WriteFile(badFile, badBytes, 0600); err != nil {
		t.Fatal(err)
	}
	b := New()
	_, hint, n, err := b.receivedPart(sdk.ClipboardContent{
		Image: &sdk.DownloadedImage{Path: badFile, Bytes: len(badBytes)},
	})
	if err == nil || hint != "heic" || n != len(badBytes) || (!errors.Is(err, errImageDecodeFailed) && !errors.Is(err, errImageUnsupportedFormat)) {
		t.Fatalf("receivedPart(bad heic) = hint=%q n=%d err=%v", hint, n, err)
	}
}
