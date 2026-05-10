package shell

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/FanBB2333/ptyrelay/pkg/session"
)

// probeResult records the remote's identity and the availability of the
// command-line tools ShellBackend depends on.
//
// Detection runs once per Backend (cached after first success). The flags
// drive command-construction: e.g. base64Wrap controls whether we add
// `-w0`, statStyle picks the GNU vs BSD format string.
type probeResult struct {
	// OS is the lowercased value of `uname -s` ("linux", "darwin", ...).
	OS string

	// HasBase64 is required — if false, ShellBackend cannot move bytes
	// through the channel and Probe returns an error.
	HasBase64 bool

	// Base64Wrap is true on GNU coreutils where `base64` adds line
	// wrapping at column 76 by default; we then pass `-w0` to disable
	// it. BSD/macOS base64 doesn't wrap and doesn't accept `-w0`.
	Base64Wrap bool

	// HasGzip enables compression on Read/Write payloads, roughly
	// halving over-the-wire bytes for typical text content.
	HasGzip bool

	// Sha256Cmd is the prefix used to compute a sha256 digest of a
	// path: either "sha256sum" (GNU) or "shasum -a 256" (BSD/macOS).
	// Empty when neither is available — verification is skipped.
	Sha256Cmd string

	// StatStyle is "gnu" (`stat -c`) or "bsd" (`stat -f`); selects the
	// format strings used by Stat/Lstat/List.
	StatStyle string

	// HasFindPrintf is true when GNU find supports `-printf`, which
	// gives us a single-pass List with explicit field separators. BSD
	// find doesn't, and we fall back to parsing `ls -la`.
	HasFindPrintf bool
}

// detect runs the platform/tool probe over sess and returns the result.
// Errors here are usually fatal for the Backend — without base64 we
// cannot encode binary bytes.
func detect(ctx context.Context, sess *session.FramedSession) (*probeResult, error) {
	// Single shell invocation that emits one labeled line per probe.
	// `command -v` is the POSIX-portable feature test; `2>/dev/null`
	// silences "not found" errors.
	const script = `
uname_out=$(uname -s 2>/dev/null || echo unknown)
echo "OS=${uname_out}"
if command -v base64 >/dev/null 2>&1; then echo BASE64=y; else echo BASE64=n; fi
if base64 -w0 </dev/null >/dev/null 2>&1; then echo BASE64WRAP=y; else echo BASE64WRAP=n; fi
if command -v gzip >/dev/null 2>&1; then echo GZIP=y; else echo GZIP=n; fi
if command -v sha256sum >/dev/null 2>&1; then echo SHA256=sha256sum
elif command -v shasum >/dev/null 2>&1; then echo "SHA256=shasum -a 256"
else echo SHA256=
fi
if stat -c '%n' /dev/null >/dev/null 2>&1; then echo STATSTYLE=gnu
else echo STATSTYLE=bsd
fi
if find /dev/null -maxdepth 0 -printf '%P' >/dev/null 2>&1; then echo FINDPRINTF=y; else echo FINDPRINTF=n; fi
`

	res, err := sess.RunFramed(ctx, script, nil)
	if err != nil {
		return nil, fmt.Errorf("probe: run: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("probe: shell exited %d: %s", res.ExitCode, res.Output)
	}

	p := &probeResult{}
	for _, line := range strings.Split(strings.TrimRight(string(res.Output), "\n"), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "OS":
			p.OS = strings.ToLower(strings.TrimSpace(v))
		case "BASE64":
			p.HasBase64 = v == "y"
		case "BASE64WRAP":
			p.Base64Wrap = v == "y"
		case "GZIP":
			p.HasGzip = v == "y"
		case "SHA256":
			p.Sha256Cmd = strings.TrimSpace(v)
		case "STATSTYLE":
			p.StatStyle = strings.TrimSpace(v)
		case "FINDPRINTF":
			p.HasFindPrintf = v == "y"
		}
	}

	if !p.HasBase64 {
		return nil, errors.New("probe: remote lacks `base64` — ShellBackend cannot encode binary payloads")
	}
	return p, nil
}

// b64EncodeCmd returns the shell snippet that base64-encodes stdin onto
// stdout without line wrapping.
func (p *probeResult) b64EncodeCmd() string {
	if p.Base64Wrap {
		return "base64 -w0"
	}
	return "base64"
}

// b64DecodeCmd is the snippet that decodes base64 from stdin. Both GNU
// and BSD `base64 -d` work.
func (p *probeResult) b64DecodeCmd() string {
	return "base64 -d"
}
