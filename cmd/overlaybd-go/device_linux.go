//go:build linux && (amd64 || arm64)

package main

import (
	"context"

	"github.com/bigvan/overlaybd-go/lsmt"
	"github.com/bigvan/overlaybd-go/ublk"
)

func serveDevice(ctx context.Context, image *lsmt.Stack, id, depth, engine int, ready func(any)) error {
	return ublk.Serve(ctx, image, ublk.Options{ID: id, QueueDepth: depth, Concurrent: engine == 3 || engine == 4, InlineReads: engine == 3, InlineSingle: engine != 0}, func(info ublk.Info) { ready(info) })
}

func deleteDevice(id int) error { return ublk.Delete(id) }
