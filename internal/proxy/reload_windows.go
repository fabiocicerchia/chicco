//go:build windows

package proxy

import "context"

// watchSIGHUP is a no-op on Windows, which has no SIGHUP; config reload there needs
// a restart.
func watchSIGHUP(_ context.Context, _ *Rotator, _ string) {}
