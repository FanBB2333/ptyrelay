package backend_test

import (
	"testing"

	"github.com/FanBB2333/ptyrelay/pkg/backend"
)

func TestOpClass(t *testing.T) {
	t.Parallel()

	cases := map[backend.Op]backend.OpClass{
		backend.OpPing:      backend.ClassReadOnly,
		backend.OpRead:      backend.ClassReadOnly,
		backend.OpStat:      backend.ClassReadOnly,
		backend.OpLstat:     backend.ClassReadOnly,
		backend.OpList:      backend.ClassReadOnly,
		backend.OpOpenRead:  backend.ClassReadOnly,
		backend.OpMkdirAll:  backend.ClassIdempotent,
		backend.OpRename:    backend.ClassIdempotent,
		backend.OpWrite:     backend.ClassIdempotent,
		backend.OpOpenWrite: backend.ClassIdempotent,
		backend.OpRemove:    backend.ClassNonIdempotent,
		backend.OpRun:       backend.ClassNonIdempotent,
	}

	for op, want := range cases {
		if got := op.Class(); got != want {
			t.Errorf("Op(%q).Class() = %d, want %d", op, got, want)
		}
	}
}

func TestOpClass_UnknownDefaultsToNonIdempotent(t *testing.T) {
	t.Parallel()

	if got := backend.Op("future-op").Class(); got != backend.ClassNonIdempotent {
		t.Errorf("unknown op should default to NonIdempotent for safety, got %d", got)
	}
}
