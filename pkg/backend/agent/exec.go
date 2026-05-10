package agent

import (
	"context"

	"github.com/FanBB2333/ptyrelay/pkg/backend"
	"github.com/FanBB2333/ptyrelay/pkg/proto"
)

// Run implements [backend.RemoteExec].
//
// Unlike ShellBackend, AgentBackend gets stdout and stderr separately
// because the agent runs the user's command via exec.Command and pipes
// each stream to its own buffer. This is one of the main reasons to
// prefer the agent path when it's available.
func (b *Backend) Run(ctx context.Context, cmd string, stdin []byte) (*backend.Result, error) {
	var data proto.RunData
	if err := b.callOp(ctx, proto.OpRun, proto.RunArgs{
		Cmd:   cmd,
		Stdin: stdin,
	}, &data); err != nil {
		return nil, err
	}
	return &backend.Result{
		Stdout:   data.Stdout,
		Stderr:   data.Stderr,
		ExitCode: data.ExitCode,
		Duration: data.Duration,
	}, nil
}
