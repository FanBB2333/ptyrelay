package session

import (
	"testing"
)

// FuzzSentinelParser feeds arbitrary byte streams to the parser and
// asserts (a) it never panics and (b) if it claims done, the exit code
// parses cleanly.
func FuzzSentinelParser(f *testing.F) {
	seeds := [][]byte{
		[]byte(""),
		[]byte("__PR_BEG_xx__\nfoo\n__PR_END_xx__:0\n"),
		[]byte("garbage\n__PR_BEG_xx__\n__PR_END_xx__:42\n"),
		[]byte("__PR_BEG_xx__"),
		[]byte("__PR_END_xx__:0\n"),
		// A nonce-mismatched marker should never satisfy the parser.
		[]byte("__PR_BEG_yy__\nfoo\n__PR_END_yy__:0\n"),
		// Repeated markers.
		[]byte("__PR_BEG_xx__\n__PR_BEG_xx__\n__PR_END_xx__:0\n"),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// Two parsers: one fed all-at-once, one byte-by-byte. Both
		// must agree on terminal state.
		bulk := newSentinelParser("xx", 1<<20)
		bulkDone, _ := bulk.feed(data)

		drip := newSentinelParser("xx", 1<<20)
		var dripDone bool
		for i := 0; i < len(data); i++ {
			d, err := drip.feed(data[i : i+1])
			if err != nil {
				dripDone = false
				break
			}
			if d {
				dripDone = true
				break
			}
		}

		// "Done at all" should agree: if bulk found a complete
		// frame, byte-by-byte must too (the parser is stateful but
		// deterministic).
		if bulkDone != dripDone {
			t.Fatalf("bulk done = %v, drip done = %v\nbulk.output=%q drip.output=%q",
				bulkDone, dripDone, bulk.output, drip.output)
		}
		if bulkDone && bulk.exitCode != drip.exitCode {
			t.Fatalf("bulk exit = %d, drip exit = %d", bulk.exitCode, drip.exitCode)
		}
	})
}
