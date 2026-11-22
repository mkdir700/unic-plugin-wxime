package wxime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/mkdir700/wx-ime-sdk/go"
	"github.com/uniclipboard/unic/engine"
	"github.com/uniclipboard/unic/plugin"
	"github.com/uniclipboard/unic/plugin/rpc"
)

const processGroup = 42

type fixtureProvider struct {
	dir string
}

func (p fixtureProvider) path(name string) string { return filepath.Join(p.dir, name) }
func (p fixtureProvider) Pair(ctx context.Context, code string) (uint64, error) {
	if code == "" {
		return p.Group(ctx)
	}
	if code != "012345" {
		return 0, rpc.NewError(rpc.CodeInvalidArgument)
	}
	return processGroup, os.WriteFile(p.path("group"), []byte("42"), 0600)
}
func (p fixtureProvider) Group(context.Context) (uint64, error) {
	data, err := os.ReadFile(p.path("group"))
	if err != nil {
		return 0, err
	}
	if string(data) != "42" {
		return processGroup + 1, nil
	}
	return processGroup, nil
}
func (p fixtureProvider) SendTextToGroup(_ context.Context, group uint64, text string) (sdk.SendReceipt, error) {
	if group != processGroup || strings.HasPrefix(text, "reject") {
		return sdk.SendReceipt{}, sdk.ErrOutcomeUnknown
	}
	return sdk.SendReceipt{}, os.WriteFile(p.path("sent-text"), []byte(text), 0600)
}
func (p fixtureProvider) SendImageToGroup(_ context.Context, group uint64, data []byte) (sdk.SendReceipt, error) {
	if group != processGroup {
		return sdk.SendReceipt{}, sdk.ErrGroupChanged
	}
	return sdk.SendReceipt{}, os.WriteFile(p.path("sent-image"), data, 0600)
}
func (p fixtureProvider) PollClipboardOnceFromGroup(_ context.Context, group uint64) (*sdk.ReceivedClipboard, error) {
	if group != processGroup {
		return nil, sdk.ErrGroupChanged
	}
	data, err := os.ReadFile(p.path("incoming"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	text := string(data)
	return &sdk.ReceivedClipboard{
		ID:      sdk.ReceivedID{Group: group, Version: 1, FromUin: 10, TraceID: "synthetic"},
		Content: sdk.ClipboardContent{Text: &text},
	}, nil
}
func (p fixtureProvider) Acknowledge(context.Context, sdk.ReceivedID) error {
	return os.Rename(p.path("incoming"), p.path("acknowledged"))
}
func (fixtureProvider) Close() error { return nil }

func TestMain(m *testing.M) {
	if dir := os.Getenv("WXIME_E2E_FIXTURE"); dir != "" {
		backend := New()
		backend.open = func(context.Context, bool) (provider, error) {
			return fixtureProvider{dir}, nil
		}
		if plugin.Serve(backend) != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestPluginProcessE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fixture := fixtureProvider{dir: dir}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	opts := engine.Options{DataDir: dir, Plugins: []engine.PluginInstance{{
		Config: plugin.Config{"images": true},
		Process: &engine.PluginProcess{
			Path: exe, SHA256: fmt.Sprintf("%x", sha256.Sum256(binary)), Spec: New().Spec(),
			Args: []string{"-test.run=^$"}, Env: []string{"WXIME_E2E_FIXTURE=" + dir},
		},
	}}}
	start := func() *engine.Engine {
		t.Helper()
		e, err := engine.New(ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Close(context.Background()) })
		if err := e.Start(ctx); err != nil {
			t.Fatal(err)
		}
		return e
	}
	e := start()
	if _, err := e.PairChannel(ctx, engine.PairChannelRequest{BackendID: "unic.wxime", Code: "999999"}); err == nil || strings.Contains(err.Error(), "999999") {
		t.Fatal("invalid pairing succeeded or leaked its code")
	}
	ch, err := e.PairChannel(ctx, engine.PairChannelRequest{BackendID: "unic.wxime", Code: "012345"})
	if err != nil || !ch.Enabled || ch.RemoteRevocation {
		t.Fatalf("pair channel: %+v %v", ch, err)
	}
	replay, err := e.PairChannel(ctx, engine.PairChannelRequest{BackendID: "unic.wxime"})
	if err != nil || replay.ID != ch.ID {
		t.Fatal("resume created a different channel", err)
	}

	await := func(label string, f func() bool) {
		t.Helper()
		deadline := time.After(7 * time.Second)
		tick := time.NewTicker(25 * time.Millisecond)
		defer tick.Stop()
		for !f() {
			select {
			case <-tick.C:
			case <-deadline:
				t.Fatalf("timed out waiting for %s", label)
			}
		}
	}
	submit := func(requestID, mime string, payload []byte, state engine.TargetState) {
		t.Helper()
		lease, err := e.ImportContent(ctx, engine.ContentInput{Representations: []engine.RepresentationInput{{
			MIME: mime, Size: int64(len(payload)), Reader: bytes.NewReader(payload),
		}}})
		if err != nil {
			t.Fatal(err)
		}
		defer e.ReleaseContent(ctx, lease.LeaseID)
		result, err := e.CommitContent(ctx, engine.CommitRequest{RequestID: engine.RequestID(requestID), LeaseID: lease.LeaseID})
		if err != nil {
			t.Fatal(err)
		}
		await(requestID, func() bool {
			op, err := e.GetOperation(ctx, result.OperationID)
			return err == nil && len(op.Targets) == 1 && op.Targets[0].TargetID == ch.ID && op.Targets[0].State == state
		})
	}
	text := []byte("synthetic outgoing text")
	submit("out-text", textMIME, text, engine.TargetSent)
	var img bytes.Buffer
	if err := png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	submit("out-image", pngMIME, img.Bytes(), engine.TargetSent)
	for name, expected := range map[string][]byte{"sent-text": text, "sent-image": img.Bytes()} {
		got, err := os.ReadFile(fixture.path(name))
		if err != nil || !bytes.Equal(got, expected) {
			t.Fatal("provider payload mismatch", name, err)
		}
	}
	submit("provider-failure", textMIME, []byte("reject synthetic text"), engine.TargetFailed)

	put := func(name, value string) {
		t.Helper()
		tmp := fixture.path(name + ".tmp")
		if err := os.WriteFile(tmp, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, fixture.path(name)); err != nil {
			t.Fatal(err)
		}
	}
	put("group", "43")
	submit("unapproved-group", textMIME, []byte("must not be published"), engine.TargetFailed)
	if got, err := os.ReadFile(fixture.path("sent-text")); err != nil || !bytes.Equal(got, text) {
		t.Fatal("an unapproved group caused publication")
	}
	put("group", "42")
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	e = start()
	paused, err := e.SetChannelEnabled(ctx, engine.SetChannelEnabledRequest{ID: ch.ID, ExpectedRevision: ch.Revision, Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	put("incoming", "synthetic incoming text")
	// Cover at least one worker tick while the channel is paused.
	time.Sleep(2300 * time.Millisecond)
	if _, err := os.Stat(fixture.path("acknowledged")); !os.IsNotExist(err) {
		t.Fatal("disabled channel polled clipboard content")
	}
	if _, err = e.SetChannelEnabled(ctx, engine.SetChannelEnabledRequest{ID: ch.ID, ExpectedRevision: paused.Revision, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	await("inbound acknowledgment", func() bool {
		_, err := os.Stat(fixture.path("acknowledged"))
		return err == nil
	})
	received, err := e.ListReceived(ctx)
	if err != nil || len(received) != 1 || received[0].ChannelID != ch.ID || received[0].PeerID != "" {
		t.Fatal("received content lost its channel attribution", err)
	}
	page, err := e.ListHistory(ctx, engine.ListHistoryRequest{Filter: engine.HistoryFilter{OriginKinds: []engine.HistoryOriginKind{engine.OriginKindExternal}}})
	if err != nil || len(page.Items) != 1 || page.Items[0].Origin.PluginID != "unic.wxime" {
		t.Fatalf("external history missing or misattributed: %v", err)
	}
	lease, err := e.RetainHistoryContent(ctx, page.Items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer e.ReleaseContent(ctx, lease.LeaseID)
	body, err := e.ReadContent(ctx, lease.LeaseID, lease.Content.Parts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil || string(data) != "synthetic incoming text" {
		t.Fatal("received payload mismatch")
	}

	if path := os.Getenv("WXIME_E2E_RECEIPT"); path != "" {
		receipt, err := json.MarshalIndent(map[string]any{
			"scenario":                 "engine-plugin-subprocess-with-local-provider",
			"checks":                   []string{"pair-and-resume", "send-text", "send-image", "provider-rejection", "unapproved-group", "restart", "pause-and-resume", "receive-history-and-ack"},
			"sent_text_sha256":         fmt.Sprintf("%x", sha256.Sum256(text)),
			"sent_image_sha256":        fmt.Sprintf("%x", sha256.Sum256(img.Bytes())),
			"received_text_sha256":     fmt.Sprintf("%x", sha256.Sum256(data)),
			"official_device_verified": false,
		}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, append(receipt, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
