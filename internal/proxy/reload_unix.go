//go:build !windows

package proxy

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// watchSIGHUP reloads the config from path on every SIGHUP until ctx is done, so
// `kill -HUP $(pidof chicco)` (or systemd's reload) applies chicco.yaml edits
// without a restart. Non-Windows only; the Windows build has a no-op stub.
func watchSIGHUP(ctx context.Context, rot *Rotator, path string) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-ch:
				reloadFromFile(rot, path)
			case <-ctx.Done():
				signal.Stop(ch)
				return
			}
		}
	}()
}
