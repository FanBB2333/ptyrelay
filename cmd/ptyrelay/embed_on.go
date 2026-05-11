//go:build embedagents

package main

import (
	"embed"

	"github.com/FanBB2333/ptyrelay/pkg/bootstrap"
)

// agentsFS holds the cross-compiled agent matrix when the binary was
// built with `-tags embedagents`. The `all:` prefix is what lets a
// repo with only the README in `agents/` still compile — files added
// by `scripts/build-agents.sh` light up automatically.
//
//go:embed all:agents
var agentsFS embed.FS

// embeddedProvider returns a Provider over the embedded matrix.
// Production CLI builds set the build tag and this returns a working
// provider; default builds use embed_off.go which returns nil.
func embeddedProvider() bootstrap.Provider {
	return &bootstrap.EmbedProvider{FS: agentsFS, Root: "agents"}
}

// embedTag reports whether this binary was built with the embedagents
// tag — surfaced in error messages so users can tell at a glance which
// flavor they're running.
func embedTag() bool { return true }
