//go:build !darwin

package wxime

func decodePlatformImageToPNG(data []byte, pixelBudget int64, byteBudget int) ([]byte, error) {
	return decodeImageToPNGPureGo(data, pixelBudget, byteBudget)
}
