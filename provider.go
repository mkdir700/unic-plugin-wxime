package wxime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	sdk "github.com/mkdir700/wx-ime-sdk/go"
	"github.com/uniclipboard/unic/plugin/rpc"
)

const stateDir = "wetype"

type provider interface {
	Pair(context.Context, string) (uint64, error)
	Group(context.Context) (uint64, error)
	SendTextToGroup(context.Context, uint64, string) (sdk.SendReceipt, error)
	SendImageToGroup(context.Context, uint64, []byte) (sdk.SendReceipt, error)
	PollClipboardOnceFromGroup(context.Context, uint64) (*sdk.ReceivedClipboard, error)
	Acknowledge(context.Context, sdk.ReceivedID) error
	Close() error
}

type weType struct{ *sdk.Client }

func openWeType(ctx context.Context, create bool) (provider, error) {
	if !create {
		if _, err := os.Stat(stateDir); err != nil {
			return nil, err
		}
	}
	name, err := weTypeDeviceName()
	if err != nil {
		return nil, err
	}
	client, err := sdk.Open(ctx, sdk.ClientOptions{StateDir: stateDir, DeviceName: name})
	if err != nil {
		return nil, err
	}
	return weType{client}, nil
}

func weTypeDeviceName() (string, error) {
	// The SDK requires the saved name when reopening a device identity.
	f, err := os.Open(filepath.Join(stateDir, "identity.json"))
	if err == nil {
		defer f.Close()
		var identity struct {
			Name string `json:"device_name"`
		}
		if err := json.NewDecoder(io.LimitReader(f, 8<<20)).Decode(&identity); err != nil {
			return "", err
		}
		return identity.Name, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	const prefix = "Unic-"
	const maxHostnameBytes = 128 - len(prefix) // SDK name limit in UTF-8 bytes.
	hostname, _ := os.Hostname()
	hostname = strings.TrimSpace(strings.ToValidUTF8(hostname, ""))
	if hostname == "" {
		hostname = "Device"
	}
	if len(hostname) > maxHostnameBytes {
		end := maxHostnameBytes
		for !utf8.RuneStart(hostname[end]) {
			end--
		}
		hostname = hostname[:end]
	}
	return prefix + hostname, nil
}

func (c weType) Group(ctx context.Context) (uint64, error) {
	g, err := c.Client.Group(ctx)
	if err != nil || g == nil {
		return 0, err
	}
	self, err := c.LocalDevice()
	if err != nil {
		return 0, err
	}
	for _, device := range g.Devices {
		if device.Uin == self.Uin && len(g.Devices) > 1 {
			return g.ID, nil
		}
	}
	return 0, nil
}

func (c weType) Pair(ctx context.Context, value string) (uint64, error) {
	current, err := c.Group(ctx)
	switch {
	case err != nil:
		return 0, err
	case value == "" && current == 0:
		return 0, rpc.NewError(rpc.CodeInvalidState)
	case value == "":
		return current, nil
	case current != 0:
		return 0, rpc.NewError(rpc.CodeConflict)
	}
	code, err := sdk.ParsePairingCode(value)
	if err != nil {
		return 0, rpc.NewError(rpc.CodeInvalidArgument)
	}
	session, err := c.Join(ctx, code)
	if err == nil {
		defer session.Close()
		var group *sdk.Group
		group, _, err = session.Wait(ctx)
		if err == nil {
			return group.ID, nil
		}
	}
	var rejected *sdk.BusinessError
	switch {
	case errors.As(err, &rejected):
		return 0, rpc.NewError(rpc.CodeInvalidArgument)
	case errors.Is(err, sdk.ErrPairingInUse):
		return 0, rpc.NewError(rpc.CodeConflict)
	default:
		return 0, err
	}
}
