//go:build darwin

package wxime

/*
#cgo LDFLAGS: -framework ImageIO -framework CoreFoundation -framework CoreGraphics

#include <CoreFoundation/CoreFoundation.h>
#include <CoreGraphics/CoreGraphics.h>
#include <ImageIO/ImageIO.h>
#include <math.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

enum {
	WXIME_IMG_OK = 0,
	WXIME_IMG_UNSUPPORTED = 1,
	WXIME_IMG_TOO_LARGE = 2,
	WXIME_IMG_DECODE_FAILED = 3,
	WXIME_IMG_ENCODE_FAILED = 4,
};

static int wxime_cf_number_i64(CFDictionaryRef dict, CFStringRef key, int64_t *out) {
	if (!dict) {
		return 0;
	}
	CFTypeRef val = CFDictionaryGetValue(dict, key);
	if (!val || CFGetTypeID(val) != CFNumberGetTypeID()) {
		return 0;
	}
	return CFNumberGetValue((CFNumberRef)val, kCFNumberSInt64Type, out) ? 1 : 0;
}

static void wxime_fit_dims(int64_t w, int64_t h, int64_t budget, int64_t *outW, int64_t *outH) {
	if (w <= 0 || h <= 0 || budget <= 0) {
		*outW = 0;
		*outH = 0;
		return;
	}
	if (w <= budget / h) {
		*outW = w;
		*outH = h;
		return;
	}
	double scale = sqrt((double)budget / ((double)w * (double)h));
	int64_t nw = (int64_t)floor((double)w * scale);
	int64_t nh = (int64_t)floor((double)h * scale);
	if (nw < 1) nw = 1;
	if (nh < 1) nh = 1;
	while (nw > budget / nh) {
		if (nw >= nh && nw > 1) {
			nw--;
		} else if (nh > 1) {
			nh--;
		} else {
			break;
		}
	}
	*outW = nw;
	*outH = nh;
}

static int wxime_shrink_dims(int64_t w, int64_t h, size_t gotBytes, size_t budgetBytes, int64_t *outW, int64_t *outH) {
	if (w <= 1 && h <= 1) {
		return 0;
	}
	double ratio = sqrt(((double)budgetBytes * 0.85) / (double)gotBytes);
	if (ratio > 0.85) ratio = 0.85;
	if (ratio < 0.25) ratio = 0.25;
	int64_t nw = (int64_t)floor((double)w * ratio);
	int64_t nh = (int64_t)floor((double)h * ratio);
	if (nw < 1) nw = 1;
	if (nh < 1) nh = 1;
	if (nw == w && nh == h) {
		if (w >= h && nw > 1) {
			nw--;
		} else if (nh > 1) {
			nh--;
		}
	}
	*outW = nw;
	*outH = nh;
	return 1;
}

static CFMutableDataRef wxime_encode_cgimage_png(CGImageRef img, int64_t maxOutPixels) {
	size_t w = CGImageGetWidth(img);
	size_t h = CGImageGetHeight(img);
	if (w == 0 || h == 0) {
		return NULL;
	}
	int64_t targetW = (int64_t)w;
	int64_t targetH = (int64_t)h;
	if (targetW > maxOutPixels / targetH) {
		wxime_fit_dims(targetW, targetH, maxOutPixels, &targetW, &targetH);
	}

	CGColorSpaceRef cs = CGColorSpaceCreateWithName(kCGColorSpaceSRGB);
	if (!cs) {
		return NULL;
	}
	CGImageAlphaInfo ai = CGImageGetAlphaInfo(img);
	int hasAlpha = (ai != kCGImageAlphaNone &&
	                ai != kCGImageAlphaNoneSkipFirst &&
	                ai != kCGImageAlphaNoneSkipLast);
	CGBitmapInfo bitmapInfo = (hasAlpha ? kCGImageAlphaPremultipliedLast : kCGImageAlphaNoneSkipLast) | kCGBitmapByteOrder32Big;

	CGContextRef ctx = CGBitmapContextCreate(NULL, (size_t)targetW, (size_t)targetH, 8, 0, cs, bitmapInfo);
	CGColorSpaceRelease(cs);
	if (!ctx) {
		return NULL;
	}
	CGContextSetInterpolationQuality(ctx, kCGInterpolationHigh);
	CGContextDrawImage(ctx, CGRectMake(0, 0, (CGFloat)targetW, (CGFloat)targetH), img);
	CGImageRef normalized = CGBitmapContextCreateImage(ctx);
	CGContextRelease(ctx);
	if (!normalized) {
		return NULL;
	}

	CFMutableDataRef pngData = CFDataCreateMutable(kCFAllocatorDefault, 0);
	if (!pngData) {
		CGImageRelease(normalized);
		return NULL;
	}
	CGImageDestinationRef dst = CGImageDestinationCreateWithData(pngData, CFSTR("public.png"), 1, NULL);
	if (!dst) {
		CFRelease(pngData);
		CGImageRelease(normalized);
		return NULL;
	}
	CGImageDestinationAddImage(dst, normalized, NULL);
	Boolean ok = CGImageDestinationFinalize(dst);
	CFRelease(dst);
	CGImageRelease(normalized);
	if (!ok) {
		CFRelease(pngData);
		return NULL;
	}
	return pngData;
}

static int wxime_imageio_to_png(
	const uint8_t *data,
	size_t len,
	int64_t maxDecodePixels,
	int64_t maxOutPixels,
	size_t maxOutBytes,
	int maxAttempts,
	uint8_t **outBuf,
	size_t *outLen
) {
	*outBuf = NULL;
	*outLen = 0;

	CFDataRef cfData = CFDataCreateWithBytesNoCopy(kCFAllocatorDefault, data, (CFIndex)len, kCFAllocatorNull);
	if (!cfData) {
		return WXIME_IMG_DECODE_FAILED;
	}
	CGImageSourceRef src = CGImageSourceCreateWithData(cfData, NULL);
	if (!src) {
		CFRelease(cfData);
		return WXIME_IMG_UNSUPPORTED;
	}
	if (CGImageSourceGetCount(src) == 0) {
		CFRelease(src);
		CFRelease(cfData);
		return WXIME_IMG_UNSUPPORTED;
	}

	CFDictionaryRef props = CGImageSourceCopyPropertiesAtIndex(src, 0, NULL);
	int64_t rawW = 0, rawH = 0, orientation = 1;
	int haveW = wxime_cf_number_i64(props, kCGImagePropertyPixelWidth, &rawW);
	int haveH = wxime_cf_number_i64(props, kCGImagePropertyPixelHeight, &rawH);
	wxime_cf_number_i64(props, kCGImagePropertyOrientation, &orientation);
	if (props) {
		CFRelease(props);
	}
	if (!haveW || !haveH || rawW <= 0 || rawH <= 0) {
		CFRelease(src);
		CFRelease(cfData);
		return WXIME_IMG_UNSUPPORTED;
	}
	if (rawW > maxDecodePixels / rawH) {
		CFRelease(src);
		CFRelease(cfData);
		return WXIME_IMG_TOO_LARGE;
	}

	int64_t orientW = rawW;
	int64_t orientH = rawH;
	if (orientation >= 5 && orientation <= 8) {
		orientW = rawH;
		orientH = rawW;
	}

	int64_t targetW = 0, targetH = 0;
	wxime_fit_dims(orientW, orientH, maxOutPixels, &targetW, &targetH);
	if (targetW <= 0 || targetH <= 0) {
		CFRelease(src);
		CFRelease(cfData);
		return WXIME_IMG_TOO_LARGE;
	}

	int status = WXIME_IMG_TOO_LARGE;
	for (int attempt = 0; attempt < maxAttempts; attempt++) {
		int64_t maxDim = targetW > targetH ? targetW : targetH;
		CFNumberRef maxDimNum = CFNumberCreate(kCFAllocatorDefault, kCFNumberSInt64Type, &maxDim);
		if (!maxDimNum) {
			status = WXIME_IMG_DECODE_FAILED;
			break;
		}
		const void *keys[4] = {
			kCGImageSourceCreateThumbnailFromImageAlways,
			kCGImageSourceCreateThumbnailWithTransform,
			kCGImageSourceThumbnailMaxPixelSize,
			kCGImageSourceShouldCacheImmediately,
		};
		const void *vals[4] = {
			kCFBooleanTrue,
			kCFBooleanTrue,
			maxDimNum,
			kCFBooleanTrue,
		};
		CFDictionaryRef opts = CFDictionaryCreate(
			kCFAllocatorDefault,
			keys,
			vals,
			4,
			&kCFTypeDictionaryKeyCallBacks,
			&kCFTypeDictionaryValueCallBacks
		);
		CFRelease(maxDimNum);
		if (!opts) {
			status = WXIME_IMG_DECODE_FAILED;
			break;
		}

		CGImageRef img = CGImageSourceCreateThumbnailAtIndex(src, 0, opts);
		CFRelease(opts);
		if (!img) {
			status = WXIME_IMG_DECODE_FAILED;
			break;
		}

		CFMutableDataRef pngData = wxime_encode_cgimage_png(img, maxOutPixels);
		int64_t actualW = (int64_t)CGImageGetWidth(img);
		int64_t actualH = (int64_t)CGImageGetHeight(img);
		CGImageRelease(img);
		if (!pngData) {
			status = WXIME_IMG_ENCODE_FAILED;
			break;
		}

		CFIndex pngLen = CFDataGetLength(pngData);
		if (pngLen <= 0) {
			CFRelease(pngData);
			status = WXIME_IMG_ENCODE_FAILED;
			break;
		}
		if ((size_t)pngLen <= maxOutBytes) {
			uint8_t *copy = (uint8_t *)malloc((size_t)pngLen);
			if (!copy) {
				CFRelease(pngData);
				status = WXIME_IMG_ENCODE_FAILED;
				break;
			}
			memcpy(copy, CFDataGetBytePtr(pngData), (size_t)pngLen);
			CFRelease(pngData);
			*outBuf = copy;
			*outLen = (size_t)pngLen;
			status = WXIME_IMG_OK;
			break;
		}

		CFRelease(pngData);
		if (!wxime_shrink_dims(actualW, actualH, (size_t)pngLen, maxOutBytes, &targetW, &targetH)) {
			status = WXIME_IMG_TOO_LARGE;
			break;
		}
	}

	CFRelease(src);
	CFRelease(cfData);
	return status;
}
*/
import "C"

import "unsafe"

func decodePlatformImageToPNG(data []byte, pixelBudget int64, byteBudget int) ([]byte, error) {
	out, err := decodeDarwinImageToPNG(data, pixelBudget, byteBudget)
	if err == nil {
		return out, nil
	}
	if err == errImageUnsupportedFormat {
		return decodeImageToPNGPureGo(data, pixelBudget, byteBudget)
	}
	return nil, err
}

func decodeDarwinImageToPNG(data []byte, pixelBudget int64, byteBudget int) ([]byte, error) {
	if len(data) == 0 {
		return nil, errImageUnsupportedFormat
	}
	var outBuf *C.uint8_t
	var outLen C.size_t
	code := C.wxime_imageio_to_png(
		(*C.uint8_t)(unsafe.Pointer(&data[0])),
		C.size_t(len(data)),
		C.int64_t(maxDecodePixels),
		C.int64_t(pixelBudget),
		C.size_t(byteBudget),
		C.int(maxEncodeAttempts),
		&outBuf,
		&outLen,
	)
	if outBuf != nil {
		defer C.free(unsafe.Pointer(outBuf))
	}
	switch code {
	case C.WXIME_IMG_OK:
		return C.GoBytes(unsafe.Pointer(outBuf), C.int(outLen)), nil
	case C.WXIME_IMG_UNSUPPORTED:
		return nil, errImageUnsupportedFormat
	case C.WXIME_IMG_TOO_LARGE:
		return nil, errImageTooLarge
	case C.WXIME_IMG_ENCODE_FAILED:
		return nil, errImageEncodeFailed
	default:
		return nil, errImageDecodeFailed
	}
}
