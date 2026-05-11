package subprocess

import "time"

// closeGrace is how long we wait for the child to exit voluntarily
// after we close its stdin. Most well-behaved REPLs (bash, sh, python,
// node) exit on EOF within a few ms; we wait a couple of multiples of
// that.
const closeGrace = 250 * time.Millisecond

// killGrace is the wait between SIGTERM and the final SIGKILL. Very
// short — by the time we're SIGTERM'ing, the child has already ignored
// stdin EOF, so politeness budget is exhausted.
const killGrace = 100 * time.Millisecond

// timerC is a tiny wrapper that returns a one-shot channel firing
// after d. Inlined so test files can stub for determinism if needed.
func timerC(d time.Duration) <-chan time.Time { return time.After(d) }
