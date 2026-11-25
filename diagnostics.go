package wxime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	sdk "github.com/mkdir700/wx-ime-sdk/go"
	"github.com/uniclipboard/unic/plugin/rpc"
)

const (
	diagnosticsFile     = "diagnostics.log"
	diagnosticsMaxBytes = 64 << 10
)

// diagLog writes strictly metadata-only, bounded diagnostics inside the
// plugin's private working directory. It never records clipboard content,
// device names, pairing codes, URLs, or secrets.
type diagLog struct {
	mu      sync.Mutex
	path    string
	lastKey string
}

func newDiagLog() *diagLog {
	return &diagLog{path: diagnosticsFile}
}

// recordEvent appends a single metadata-only diagnostic line, suppressing
// consecutive duplicates with the same dedupeKey (empty dedupeKey always logs).
func (d *diagLog) recordEvent(dedupeKey, event, detail string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if dedupeKey != "" && d.lastKey == dedupeKey {
		return
	}
	d.lastKey = dedupeKey
	d.writeLineLocked(fmt.Sprintf("%s %s %s\n", time.Now().UTC().Format(time.RFC3339), event, detail))
}

// clearDedupe clears consecutive-error suppression and logs a recovery entry
// when a previously failing state succeeds.
func (d *diagLog) clearDedupe(prefix string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lastKey == "" {
		return
	}
	if len(d.lastKey) >= len(prefix) && d.lastKey[:len(prefix)] == prefix {
		d.lastKey = ""
		d.writeLineLocked(fmt.Sprintf("%s %srecovered\n", time.Now().UTC().Format(time.RFC3339), prefix))
	}
}

func (d *diagLog) writeLineLocked(line string) {
	path := d.path
	if path == "" {
		return
	}
	if info, err := os.Stat(path); err == nil && info.Size() >= diagnosticsMaxBytes {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line)
	_ = f.Close()
}

// classifyError maps any error to a stable, privacy-safe error class without
// leaking raw upstream text, URLs, tokens, or clipboard payloads.
func classifyError(err error) string {
	if err == nil {
		return "none"
	}
	var biz *sdk.BusinessError
	if errors.As(err, &biz) {
		return fmt.Sprintf("business_error_cmd%d_status%d", biz.Command, biz.Status)
	}
	var httpErr *sdk.HTTPError
	if errors.As(err, &httpErr) {
		return fmt.Sprintf("http_%d", httpErr.Status)
	}
	var rpcErr *rpc.Error
	if errors.As(err, &rpcErr) {
		return "rpc_" + string(rpcErr.Code)
	}
	switch {
	case errors.Is(err, errImageUnsupportedFormat):
		return "unsupported_image_format"
	case errors.Is(err, errImageTooLarge):
		return "image_too_large"
	case errors.Is(err, errImageDecodeFailed):
		return "image_decode_failed"
	case errors.Is(err, errImageEncodeFailed):
		return "image_encode_failed"
	case errors.Is(err, errInvalidText):
		return "invalid_text"
	case errors.Is(err, sdk.ErrNotPaired):
		return "not_paired"
	case errors.Is(err, sdk.ErrGroupChanged):
		return "group_changed"
	case errors.Is(err, sdk.ErrOutcomeUnknown):
		return "outcome_unknown"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, os.ErrNotExist):
		return "not_found"
	default:
		return "internal_error"
	}
}
