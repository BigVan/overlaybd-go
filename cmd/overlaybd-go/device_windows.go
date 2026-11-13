package main

import (
	"context"
	"errors"

	"github.com/bigvan/overlaybd-go/lsmt"
)

func serveDevice(ctx context.Context, image *lsmt.Stack, id, depth, engine int, ready func(any)) error {
	return errors.New("ublk is Linux-only; use the iscsi command on Windows")
}

func deleteDevice(id int) error {
	return errors.New("ublk is Linux-only; disconnect the Windows iSCSI session with Disconnect-IscsiTarget")
}
