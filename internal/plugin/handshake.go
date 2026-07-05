package plugin

import (
	"context"
	"fmt"
	"time"

	"github.com/harishhary/blink/internal/helpers"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Handshake runs the boilerplate every plugin type's DoHandshake shares: assert the dispensed gRPC
// client to the type's client interface C, run Init under a 10s timeout, and build the typed plugin
// via newPlugin (the only per-type step; T and C are inferred from its signature). It returns the
// same client as a PluginRPC for the Manager to ping/stop.
//
// It does not return id/name: those live on the plugin's Metadata() (snapshot-derived), so the
// caller reads them there and there is one source of truth.
func Handshake[T any, C PluginRPC](
	ctx context.Context,
	raw any, binPath, hash string,
	newPlugin func(fileName string, rpc C, hash string) T,
) (T, PluginRPC, error) {
	var zero T
	rpc, ok := raw.(C)
	if !ok {
		return zero, nil, fmt.Errorf("dispense: unexpected type %T", raw)
	}
	fileName := helpers.BinaryBaseName(binPath)

	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err := rpc.Init(initCtx, &emptypb.Empty{})
	cancel()
	if err != nil {
		return zero, nil, fmt.Errorf("init: %w", err)
	}
	return newPlugin(fileName, rpc, hash), rpc, nil
}
