package wxime

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	sdk "github.com/mkdir700/wx-ime-sdk/go"
	"github.com/uniclipboard/unic/plugin"
	"github.com/uniclipboard/unic/plugin/rpc"
)

const (
	textMIME  = "text/plain"
	pngMIME   = "image/png"
	maxText   = 1 << 20
	maxImage  = 16 << 20
	maxPixels = 16 << 20
)

type Backend struct {
	open     func(context.Context, bool) (provider, error)
	receiver plugin.Receiver
	resolver plugin.ChannelResolver
	diag     *diagLog
	gate     chan struct{}
	client   provider        // protected by gate
	pending  *sdk.ReceivedID // owned by the receive worker

	mu     sync.Mutex
	life   context.Context
	cancel context.CancelFunc
	done   chan struct{}
	jobs   chan sendJob
	slot   chan struct{}
}

type sendJob struct {
	view plugin.SyncView
	part plugin.SyncPart
	host plugin.SyncHost
}

func New() *Backend {
	return &Backend{
		open: openWeType,
		diag: newDiagLog(),
		gate: make(chan struct{}, 1),
		jobs: make(chan sendJob, 1),
		slot: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
}

func (*Backend) Spec() plugin.Spec {
	return plugin.Spec{
		ID: "unic.wxime", Version: "0.1.4", APIVersion: 1,
		Capabilities:     []string{plugin.CapSyncBackend},
		TransportKind:    plugin.TransportObjectStore,
		ExternalChannels: true,
	}
}

func (*Backend) Init(context.Context, plugin.Host, plugin.Config) error { return nil }

func (b *Backend) SetReceiver(r plugin.Receiver) {
	b.receiver = r
	b.resolver, _ = r.(plugin.ChannelResolver)
}

func (b *Backend) Start(context.Context) error {
	if b.resolver == nil {
		return rpc.NewError(rpc.CodeUnsupported)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.life, b.cancel = context.WithCancel(context.Background())
	go b.run(b.life)
	return nil
}

func (b *Backend) Close(ctx context.Context) error {
	b.mu.Lock()
	cancel := b.cancel
	b.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-b.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Backend) acquire(ctx context.Context) error {
	select {
	case b.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Backend) getClient(ctx context.Context, create bool) (provider, error) {
	if b.client == nil {
		c, err := b.open(ctx, create)
		if err != nil {
			return nil, err
		}
		b.client = c
	}
	return b.client, nil
}

func (b *Backend) PairChannel(ctx context.Context, code string) ([]byte, string, error) {
	b.mu.Lock()
	life := b.life
	b.mu.Unlock()
	if life == nil {
		return nil, "", rpc.NewError(rpc.CodeInvalidState)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(life, cancel)
	defer stop()
	if err := b.acquire(ctx); err != nil {
		return nil, "", err
	}
	defer func() { <-b.gate }()
	client, err := b.getClient(ctx, code != "")
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", rpc.NewError(rpc.CodeInvalidState)
	}
	if err != nil {
		return nil, "", err
	}
	group, err := client.Pair(ctx, code)
	if err != nil {
		return nil, "", err
	}
	return groupContext(group), "WeType", nil
}

func groupContext(group uint64) []byte {
	return binary.BigEndian.AppendUint64(nil, group)
}

func (b *Backend) Submit(ctx context.Context, v plugin.SyncView, h plugin.SyncHost) (bool, error) {
	part := slices.IndexFunc(v.Parts, func(p plugin.SyncPart) bool {
		return !p.File && p.MIME == pngMIME && p.Size > 0 && p.Size <= maxImage
	})
	if part < 0 {
		part = slices.IndexFunc(v.Parts, func(p plugin.SyncPart) bool {
			return p.MIME == textMIME && p.Size > 0 && p.Size <= maxText
		})
	}
	if part < 0 || len(v.Targets) == 0 {
		return false, nil
	}
	select {
	case b.slot <- struct{}{}:
	default:
		return false, rpc.NewError(rpc.CodeExhausted)
	}
	if err := h.Register(v.Targets); err != nil {
		<-b.slot
		return false, err
	}
	b.jobs <- sendJob{view: v, part: v.Parts[part], host: h}
	return true, nil
}

func (b *Backend) run(ctx context.Context) {
	defer close(b.done)
	defer func() {
		b.gate <- struct{}{}
		defer func() { <-b.gate }()
		if b.client != nil {
			_ = b.client.Close()
		}
	}()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case job := <-b.jobs:
			b.send(ctx, job)
			<-b.slot
		case <-ticker.C:
			b.poll(ctx)
		}
	}
}

func (b *Backend) send(life context.Context, job sendJob) {
	ctx, cancel := context.WithTimeout(life, 45*time.Second)
	defer cancel()
	sent, err := b.publish(ctx, job)
	if sent == "" && err != nil {
		b.diag.recordEvent("", "publish_failed", fmt.Sprintf("mime=%s size=%d class=%s", job.part.MIME, job.part.Size, classifyError(err)))
	}
	for _, target := range job.view.Targets {
		state := plugin.SyncFailed
		var cause error
		if target == sent {
			state = plugin.SyncSent
		} else {
			cause = err
		}
		job.host.Report(target, state, cause)
	}
}

func (b *Backend) publish(ctx context.Context, job sendJob) (string, error) {
	if err := b.acquire(ctx); err != nil {
		return "", err
	}
	defer func() { <-b.gate }()
	client, err := b.getClient(ctx, false)
	if err != nil {
		return "", err
	}
	group, err := client.Group(ctx)
	if err != nil {
		return "", err
	}
	if group == 0 {
		return "", sdk.ErrNotPaired
	}
	id, err := b.resolver.ResolveChannel(ctx, groupContext(group))
	if err != nil {
		return "", err
	}
	if id == "" || !slices.Contains(job.view.Targets, id) {
		return "", sdk.ErrGroupChanged
	}
	reader, err := job.part.Open(ctx)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, job.part.Size+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) != job.part.Size {
		return "", errors.New("wxime: outbound part size mismatch")
	}
	if job.part.MIME == pngMIME {
		_, err = client.SendImageToGroup(ctx, group, data)
	} else if utf8.Valid(data) {
		_, err = client.SendTextToGroup(ctx, group, string(data))
	} else {
		return "", errors.New("wxime: outbound text is not valid UTF-8")
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

func (b *Backend) poll(life context.Context) {
	ctx, cancel := context.WithTimeout(life, 12*time.Second)
	defer cancel()
	if b.acquire(ctx) != nil {
		return
	}
	defer func() { <-b.gate }()
	client, err := b.getClient(ctx, false)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			class := classifyError(err)
			b.diag.recordEvent("poll_open:"+class, "poll_open_failed", "class="+class)
		}
		return
	}
	if b.pending != nil {
		b.ack(ctx, client, *b.pending)
		return
	}
	group, err := client.Group(ctx)
	if err != nil {
		class := classifyError(err)
		b.diag.recordEvent("poll_group:"+class, "poll_group_failed", "class="+class)
		return
	}
	if group == 0 {
		return
	}
	id, err := b.resolver.ResolveChannel(ctx, groupContext(group))
	if err != nil {
		class := classifyError(err)
		b.diag.recordEvent("poll_resolve:"+class, "poll_resolve_failed", "class="+class)
		return
	}
	if id == "" {
		return
	}
	value, err := client.PollClipboardOnceFromGroup(ctx, group)
	if err != nil {
		class := classifyError(err)
		b.diag.recordEvent("poll_fetch:"+class, "poll_fetch_failed", "class="+class)
		return
	}
	b.diag.clearDedupe("poll_")
	if value == nil {
		return
	}
	deliverCtx, deliverCancel := context.WithTimeout(life, 30*time.Second)
	defer deliverCancel()
	if value.ID.Group != group {
		b.diag.recordEvent("", "receive_skipped", fmt.Sprintf("version=%d reason=group_mismatch", value.ID.Version))
		b.ack(deliverCtx, client, value.ID)
		return
	}
	part, hint, inBytes, err := b.receivedPart(value.Content)
	if err != nil {
		b.diag.recordEvent("", "receive_rejected", fmt.Sprintf("version=%d format=%s bytes=%d class=%s", value.ID.Version, hint, inBytes, classifyError(err)))
	} else if part.MIME != "" {
		transfer := sha256.Sum256(fmt.Appendf(nil, "%d/%d/%d/%s", value.ID.Group, value.ID.Version, value.ID.FromUin, value.ID.TraceID))
		view := sha256.Sum256(append([]byte(part.MIME+"\x00"), part.Data...))
		if rerr := b.receiver.Receive(deliverCtx, id, plugin.Received{
			Transfer: fmt.Sprintf("%x", transfer), View: fmt.Sprintf("%x", view),
			Parts: []plugin.ReceivedPart{part},
		}); rerr != nil {
			b.diag.recordEvent("", "receive_declined", fmt.Sprintf("version=%d mime=%s bytes=%d class=%s", value.ID.Version, part.MIME, len(part.Data), classifyError(rerr)))
		}
	}
	b.ack(deliverCtx, client, value.ID)
}

func (b *Backend) ack(ctx context.Context, client provider, id sdk.ReceivedID) {
	if err := client.Acknowledge(ctx, id); err != nil {
		b.pending = &id
		class := classifyError(err)
		b.diag.recordEvent("ack:"+class, "receive_ack_failed", fmt.Sprintf("version=%d class=%s", id.Version, class))
		return
	}
	b.pending = nil
}

var errInvalidText = errors.New("wxime: text payload empty or exceeds size limit")

func (b *Backend) receivedPart(value sdk.ClipboardContent) (plugin.ReceivedPart, string, int, error) {
	if value.Text != nil {
		n := len(*value.Text)
		if n == 0 || n > maxText {
			return plugin.ReceivedPart{}, "text", n, errInvalidText
		}
		return plugin.ReceivedPart{MIME: textMIME, Data: []byte(*value.Text)}, "text", n, nil
	}
	if value.Image == nil {
		return plugin.ReceivedPart{}, "empty", 0, errImageUnsupportedFormat
	}
	if value.Image.Bytes <= 0 || value.Image.Bytes > maxImage {
		return plugin.ReceivedPart{}, "image", value.Image.Bytes, errImageTooLarge
	}
	file, err := os.Open(value.Image.Path)
	if err != nil {
		return plugin.ReceivedPart{}, "image", value.Image.Bytes, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(value.Image.Bytes)+1))
	if err != nil {
		return plugin.ReceivedPart{}, "image", value.Image.Bytes, err
	}
	hint := detectImageFormatHint(data)
	if len(data) != value.Image.Bytes {
		return plugin.ReceivedPart{}, hint, len(data), errImageDecodeFailed
	}
	pngBytes, err := convertImageToPNG(data)
	if err != nil {
		return plugin.ReceivedPart{}, hint, len(data), err
	}
	return plugin.ReceivedPart{MIME: pngMIME, Data: pngBytes}, hint, len(data), nil
}
