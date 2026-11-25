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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	if bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		return &sdk.ReceivedClipboard{
			ID: sdk.ReceivedID{Group: group, Version: uint64(cfg.Width) + 1, FromUin: 10, TraceID: "synthetic-image"},
			Content: sdk.ClipboardContent{Image: &sdk.DownloadedImage{
				Path: p.path("incoming"), Bytes: len(data),
			}},
		}, nil
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
	if mode := os.Getenv("WXIME_E2E_OFFLINE_IDENTITY"); mode != "" {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// The SDK saves the identity before making its first network request.
		if _, err := openWeType(ctx, mode == "create"); err != context.Canceled {
			fmt.Fprintln(os.Stderr, "open offline identity:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
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

func checkDeviceNames(t *testing.T, exe string) {
	t.Helper()
	hostname, _ := os.Hostname()
	hostname = strings.TrimSpace(strings.ToValidUTF8(hostname, ""))
	if hostname == "" {
		hostname = "Device"
	}
	for len(hostname) > 123 {
		_, size := utf8.DecodeLastRuneInString(hostname)
		hostname = hostname[:len(hostname)-size]
	}
	for _, name := range []string{"Unic-" + hostname, "UniClipboard"} {
		dir := t.TempDir()
		identity := filepath.Join(dir, stateDir, "identity.json")
		var previous []byte
		var err error
		if name == "UniClipboard" {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := sdk.Open(ctx, sdk.ClientOptions{StateDir: filepath.Dir(identity), DeviceName: name}); err != context.Canceled {
				t.Fatal("create legacy identity offline:", err)
			}
			previous, err = os.ReadFile(identity)
			if err != nil {
				t.Fatal(err)
			}
		}
		for attempt := 0; attempt < 2; attempt++ {
			mode := "reopen"
			if previous == nil {
				mode = "create"
			}
			cmd := exec.Command(exe, "-test.run=^$")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "WXIME_E2E_OFFLINE_IDENTITY="+mode)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("open offline identity: %v\n%s", err, output)
			}
			raw, err := os.ReadFile(identity)
			if err != nil {
				t.Fatal(err)
			}
			var saved struct {
				Name string `json:"device_name"`
			}
			if err := json.Unmarshal(raw, &saved); err != nil || saved.Name != name {
				t.Fatalf("saved device name = %q, want %q: %v", saved.Name, name, err)
			}
			if previous != nil && !bytes.Equal(raw, previous) {
				t.Fatal("reopening changed the persisted identity")
			}
			previous = raw
		}
	}
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
	checkDeviceNames(t, exe)
	binary, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	opts := engine.Options{DataDir: dir, Plugins: []engine.PluginInstance{{
		Config: plugin.Config{"images": false},
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
	commit := func(requestID, mime string, payload []byte) engine.SubmissionResult {
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
		return result
	}
	submit := func(requestID, mime string, payload []byte, state engine.TargetState) {
		t.Helper()
		result := commit(requestID, mime, payload)
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

	setImages := func(enabled bool) {
		t.Helper()
		state, err := e.GetState(ctx)
		if err != nil {
			t.Fatal(err)
		}
		policy := engine.DefaultBackendPolicy()
		policy.Images = enabled
		state.Settings.Value.BackendPolicies = map[engine.PluginID]engine.BackendSyncPolicy{"unic.wxime": policy}
		_, err = e.UpdateSettings(ctx, engine.UpdateSettingsRequest{
			ExpectedRevision: state.Settings.Revision,
			Value:            state.Settings.Value,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	offerImage := func(payload string) {
		t.Helper()
		if err := os.Remove(fixture.path("acknowledged")); err != nil {
			t.Fatal(err)
		}
		put("incoming", payload)
		await("image acknowledgment", func() bool {
			_, err := os.Stat(fixture.path("acknowledged"))
			return err == nil
		})
	}
	setImages(false)
	if err := os.Remove(fixture.path("sent-image")); err != nil {
		t.Fatal(err)
	}
	if result := commit("out-image-disabled", pngMIME, img.Bytes()); result.SyncDisposition != engine.SyncPolicyFilter {
		t.Fatalf("host did not disable outbound image sync: %s", result.SyncDisposition)
	}
	if _, err := os.Stat(fixture.path("sent-image")); !os.IsNotExist(err) {
		t.Fatal("disabled image reached the provider")
	}
	var blockedImage bytes.Buffer
	if err := png.Encode(&blockedImage, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	offerImage(blockedImage.String())
	externalImages := engine.ListHistoryRequest{Filter: engine.HistoryFilter{
		OriginKinds: []engine.HistoryOriginKind{engine.OriginKindExternal},
	}}
	page, err = e.ListHistory(ctx, externalImages)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("disabled image entered external history: %+v %v", page, err)
	}
	setImages(true)
	submit("out-image-reenabled", pngMIME, img.Bytes(), engine.TargetSent)
	offerImage(img.String())
	await("incoming image in history", func() bool {
		page, err = e.ListHistory(ctx, externalImages)
		return err == nil && len(page.Items) == 2
	})
	imageLease, err := e.RetainHistoryContent(ctx, page.Items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer e.ReleaseContent(ctx, imageLease.LeaseID)
	part := imageLease.Content.Parts[0]
	body, err = e.ReadContent(ctx, imageLease.LeaseID, part.ID)
	if err != nil {
		t.Fatal(err)
	}
	incomingImage, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil || part.MIME != pngMIME || !bytes.Equal(incomingImage, img.Bytes()) {
		t.Fatal("received image payload mismatch")
	}

	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	opts.Plugins[0].Config = plugin.Config{}
	e = start()
	_, err = e.GetChannel(ctx, ch.ID)
	if channelErr, ok := err.(*engine.Error); !ok || channelErr.Code != engine.CodeNotFound {
		t.Fatalf("expected the old local authorization to be gone: %v", err)
	}
	ch, err = e.PairChannel(ctx, engine.PairChannelRequest{BackendID: "unic.wxime"})
	if err != nil || !ch.Enabled {
		t.Fatalf("could not resume the existing group after removing legacy settings: %v", err)
	}
	submit("out-image-after-upgrade", pngMIME, img.Bytes(), engine.TargetSent)

	if path := os.Getenv("WXIME_E2E_RECEIPT"); path != "" {
		receipt, err := json.MarshalIndent(map[string]any{
			"scenario":                 "engine-plugin-subprocess-with-local-provider",
			"checks":                   []string{"pair-and-resume", "send-text", "send-image", "provider-rejection", "unapproved-group", "restart", "pause-and-resume", "receive-history-and-ack", "disable-and-reenable-bidirectional-image-sync-via-host-policy", "resume-existing-group-after-removing-legacy-settings"},
			"sent_text_sha256":         fmt.Sprintf("%x", sha256.Sum256(text)),
			"sent_image_sha256":        fmt.Sprintf("%x", sha256.Sum256(img.Bytes())),
			"received_text_sha256":     fmt.Sprintf("%x", sha256.Sum256(data)),
			"received_image_sha256":    fmt.Sprintf("%x", sha256.Sum256(incomingImage)),
			"verified_device_names":    []string{"Unic-<hostname> on first open", "saved name and identity on reopen"},
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
