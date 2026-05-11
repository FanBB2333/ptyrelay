// Package bootstrap installs the ptyrelay-agent binary on the remote.
//
// The flow is:
//  1. Detect the remote OS + architecture via `uname -sm` over a
//     ShellBackend.
//  2. Look up the matching agent binary from a [Provider].
//  3. Choose an install path (XDG-aware, $HOME-rooted by default).
//  4. ShellBackend.Write the binary atomically (tempfile + sha256 +
//     rename) with mode 0755.
//  5. Probe the freshly-installed agent to confirm it answers.
//
// On success, [Bootstrap] returns the install path; AgentBackend.New
// can then be called against that path.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/FanBB2333/ptyrelay/pkg/backend/agent"
	"github.com/FanBB2333/ptyrelay/pkg/backend/shell"
)

// Provider supplies an agent binary for a target (os, arch) tuple.
//
// Production builds use [EmbedProvider] over a `//go:embed agents/*`
// directory; tests inject a [FileProvider] that reads from disk.
type Provider interface {
	// Get returns the agent bytes for the given GOOS/GOARCH names
	// (e.g. "linux", "amd64"). Returns ErrUnsupportedTarget when the
	// provider has no binary for that combination.
	Get(osName, arch string) ([]byte, error)
}

// ErrUnsupportedTarget is returned by Provider.Get when no binary is
// available for the requested platform.
var ErrUnsupportedTarget = errors.New("bootstrap: unsupported target")

// Options configures [Bootstrap].
type Options struct {
	// Provider supplies the agent binary as bytes the local side then
	// uploads to the remote via ShellBackend.Write. Required when
	// FromURL is nil.
	Provider Provider

	// FromURL, when non-nil, takes precedence over Provider: the
	// remote fetches the binary directly via curl/wget, skipping the
	// (slow) multi-MB local→remote upload. The callback receives the
	// detected (osName, arch) and returns a download URL plus an
	// optional sha256 hex (empty string disables verification).
	//
	// URLs ending in ".gz" are gunzipped on the remote. Requires the
	// remote to have either curl or wget on PATH.
	FromURL func(osName, arch string) (url, sha256 string)

	// InstallPath is the absolute path where the binary should land on
	// the remote. Empty means "$HOME/.local/bin/ptyrelay-agent" — the
	// $HOME is resolved at runtime by the remote shell.
	InstallPath string
}

// Bootstrap installs the agent and returns the absolute path it was
// written to.
//
// Path selection:
//   - opts.FromURL != nil → remote curl/wget directly to the install
//     path. Best when the remote has outbound network.
//   - opts.Provider != nil → local bytes uploaded via ShellBackend.Write.
//     Best when the remote is air-gapped.
func Bootstrap(ctx context.Context, sb *shell.Backend, opts Options) (string, error) {
	if opts.Provider == nil && opts.FromURL == nil {
		return "", errors.New("bootstrap: Provider or FromURL is required")
	}

	osName, arch, err := detectPlatform(ctx, sb)
	if err != nil {
		return "", fmt.Errorf("bootstrap: detect platform: %w", err)
	}

	installPath, err := resolveInstallPath(ctx, sb, opts.InstallPath)
	if err != nil {
		return "", fmt.Errorf("bootstrap: resolve install path: %w", err)
	}

	parent := installPath
	if i := strings.LastIndexByte(parent, '/'); i >= 0 {
		parent = parent[:i]
	}
	if parent != "" && parent != installPath {
		if err := sb.MkdirAll(ctx, parent, 0o755); err != nil {
			return "", fmt.Errorf("bootstrap: mkdir %s: %w", parent, err)
		}
	}

	if opts.FromURL != nil {
		url, sha := opts.FromURL(osName, arch)
		if url == "" {
			return "", fmt.Errorf("bootstrap: FromURL returned empty URL for %s/%s",
				osName, arch)
		}
		if err := fetchOnRemote(ctx, sb, url, sha, installPath); err != nil {
			return "", fmt.Errorf("bootstrap: fetch %s: %w", url, err)
		}
		return installPath, nil
	}

	bin, err := opts.Provider.Get(osName, arch)
	if err != nil {
		return "", fmt.Errorf("bootstrap: get %s/%s binary: %w", osName, arch, err)
	}
	if err := sb.Write(ctx, installPath, bin, 0o755); err != nil {
		return "", fmt.Errorf("bootstrap: write agent: %w", err)
	}
	return installPath, nil
}

// VerifyInstall calls ping on a freshly-installed agent to confirm the
// binary actually works on the remote (right architecture, executable,
// not silently truncated).
func VerifyInstall(ctx context.Context, sb *shell.Backend, agentPath string) error {
	ab := agent.New(sb.Session(), agentPath)
	return ab.Probe(ctx)
}

// detectPlatform runs `uname -sm` and normalizes the output into
// (GOOS-style, GOARCH-style) names.
func detectPlatform(ctx context.Context, sb *shell.Backend) (osName, arch string, err error) {
	res, err := sb.Run(ctx, "uname -sm", nil)
	if err != nil {
		return "", "", err
	}
	if res.ExitCode != 0 {
		return "", "", fmt.Errorf("uname exited %d: %s", res.ExitCode, res.Stdout)
	}
	parts := strings.Fields(strings.TrimSpace(string(res.Stdout)))
	if len(parts) < 2 {
		return "", "", fmt.Errorf("malformed uname output: %q", res.Stdout)
	}
	return normalizeOS(parts[0]), normalizeArch(parts[1]), nil
}

func normalizeOS(s string) string {
	switch strings.ToLower(s) {
	case "linux":
		return "linux"
	case "darwin":
		return "darwin"
	case "freebsd":
		return "freebsd"
	default:
		return strings.ToLower(s)
	}
}

func normalizeArch(s string) string {
	switch s {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "i386", "i686":
		return "386"
	case "armv7l", "armv6l":
		return "arm"
	default:
		return strings.ToLower(s)
	}
}

// resolveInstallPath expands the default install template against the
// remote's $HOME, or returns the caller-supplied path verbatim.
func resolveInstallPath(ctx context.Context, sb *shell.Backend, requested string) (string, error) {
	if requested != "" {
		return requested, nil
	}
	res, err := sb.Run(ctx, "printf '%s' \"$HOME\"", nil)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("$HOME lookup failed: exit %d", res.ExitCode)
	}
	home := strings.TrimSpace(string(res.Stdout))
	if home == "" {
		return "", errors.New("remote $HOME is empty")
	}
	return home + "/.local/bin/ptyrelay-agent", nil
}
