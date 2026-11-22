package wxime

import (
	"context"
	"errors"
	"os"

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
	client, err := sdk.Open(ctx, sdk.ClientOptions{StateDir: stateDir, DeviceName: "UniClipboard"})
	if err != nil {
		return nil, err
	}
	return weType{client}, nil
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
