package wxime

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/mkdir700/wx-ime-sdk/go"
)

type errRoundTripper struct {
	err    error
	called bool
}

func (rt *errRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	rt.called = true
	return nil, rt.err
}

// TestOpenSSLSecp128r1Identity verifies that the linked OpenSSL libcrypto
// supports secp128r1 key generation and public-key reload on this platform.
func TestOpenSSLSecp128r1Identity(t *testing.T) {
	dir := t.TempDir()
	for attempt := range 2 {
		rt := &errRoundTripper{err: errors.New("offline smoke test")}
		opts := sdk.ClientOptions{
			StateDir:   dir,
			DeviceName: "UniClipboard",
			HTTPClient: &http.Client{Transport: rt},
		}
		c, err := sdk.Open(context.Background(), opts)
		if c != nil {
			_ = c.Close()
		}
		if !rt.called || err == nil {
			t.Fatalf("attempt %d: expected OpenSSL identity setup to reach offline transport, called=%v err=%v", attempt+1, rt.called, err)
		}
		raw, readErr := os.ReadFile(filepath.Join(dir, "identity.json"))
		if readErr != nil || !bytes.Contains(raw, []byte("BEGIN PRIVATE KEY")) {
			t.Fatalf("attempt %d: expected persisted PEM identity, readErr=%v", attempt+1, readErr)
		}
	}
}
