package proxy

// clock.go is the package's one read of the wall clock.
//
// Cooldowns, quota windows, the daily counters and the dashboard all need the
// current time, and a test that cannot move it has to sleep instead. Every
// caller goes through now(), so a test replaces this one variable and the
// whole package follows.

import "time"

// now reads the wall clock. Replace it in a test, and restore it.
var now = time.Now //nolint:forbidigo // FC-GEN-055: this variable IS the injectable clock
