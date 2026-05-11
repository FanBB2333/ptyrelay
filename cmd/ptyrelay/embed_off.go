//go:build !embedagents

package main

import "github.com/FanBB2333/ptyrelay/pkg/bootstrap"

// embeddedProvider returns nil in default builds — the agent matrix
// is not embedded, so callers must supply --provider-dir or --from-url.
// Built with `-tags embedagents` (see embed_on.go) this hook returns a
// working Provider backed by `//go:embed agents`.
func embeddedProvider() bootstrap.Provider { return nil }

// embedTag reports whether this binary was built with the embedagents
// tag.
func embedTag() bool { return false }
