package shell

import (
	"context"

	"github.com/FanBB2333/ptyrelay/pkg/backend"
)

// Run implements [backend.RemoteExec]. The user's command runs as-is
// inside the framing wrapper; stdin (if any) is delivered via the
// session's here-doc mechanism.
//
// Stdout and stderr arrive merged in Result.Stdout — the underlying PTY
// channel does not separate them. Result.Stderr is reserved for a future
// AgentBackend that does.
func (b *Backend) Run(ctx context.Context, cmd string, stdin []byte) (*backend.Result, error) {
	if _, err := b.ensureProbed(ctx); err != nil {
		return nil, err
	}
	res, err := b.runShell(ctx, cmd, stdin)
	if err != nil {
		return nil, err
	}
	return &backend.Result{
		Stdout:   res.Output,
		ExitCode: res.ExitCode,
		Duration: res.Duration,
	}, nil
}
