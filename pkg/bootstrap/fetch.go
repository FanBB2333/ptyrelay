package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/FanBB2333/ptyrelay/internal/shellquote"
	"github.com/FanBB2333/ptyrelay/pkg/backend/shell"
)

// fetchOnRemote tells the remote to download url directly into installPath,
// optionally verifying against an expected sha256 hex digest. The remote
// must have either `curl` or `wget` on PATH; if neither exists the call
// fails with a clear error rather than silently falling back.
//
// URLs ending in ".gz" are gunzipped before install — this matches the
// release-tarball pattern (`ptyrelay-agent-linux-amd64.gz`) and shaves
// ~3× off the download size for a typical Go binary.
//
// The sequence is:
//
//	tmp=$(mktemp)
//	(curl|wget) "$url" > "$tmp"
//	# optional sha256 check
//	# optional gunzip
//	chmod 755 "$tmp"
//	mv "$tmp" "$install"
//
// All intermediate state lives under $TMPDIR; on failure the tempfile
// is removed so a retry starts clean.
//
// The script is staged to a remote tempfile via the proven chunked-base64
// path (`shell.Backend.Write`) and then invoked as `sh <tmpfile>`. The
// staging hop avoids a PTY-buffer interaction we hit on macOS bash 3.2:
// inline `sh -c '<~1KB script>'` writes lose synchronization in a narrow
// 900–1500-byte band, hanging the framed session forever. Staging keeps
// the framed command line constant (~120 bytes) regardless of script size.
func fetchOnRemote(ctx context.Context, sb *shell.Backend, url, sha256hex, installPath string) error {
	qURL := shellquote.Quote(url)
	qSHA := shellquote.Quote(sha256hex)
	qInst := shellquote.Quote(installPath)
	isGz := strings.HasSuffix(strings.ToLower(url), ".gz")

	gunzipBlock := ""
	if isGz {
		gunzipBlock = `
  if ! command -v gunzip >/dev/null 2>&1; then
    echo "ptyrelay-fetch: gunzip required for .gz URL" 1>&2
    exit 1
  fi
  GZ="$TMP".gz
  mv "$TMP" "$GZ"
  gunzip -c "$GZ" > "$TMP"
  rm -f "$GZ"
`
	}

	verifyBlock := ""
	if sha256hex != "" {
		verifyBlock = fmt.Sprintf(`
  HASHER=""
  if command -v sha256sum >/dev/null 2>&1; then HASHER="sha256sum"
  elif command -v shasum >/dev/null 2>&1; then HASHER="shasum -a 256"
  else echo "ptyrelay-fetch: sha256sum/shasum missing for verification" 1>&2; exit 1
  fi
  ACTUAL=$($HASHER "$TMP" | awk '{print $1}')
  if [ "$ACTUAL" != %s ]; then
    echo "ptyrelay-fetch: sha256 mismatch: $ACTUAL vs expected" 1>&2
    exit 1
  fi
`, qSHA)
	}

	inner := fmt.Sprintf(`set -e
TMP=$(mktemp 2>/dev/null || mktemp -t ptyrelay-agent)
trap 'rm -f "$TMP" "$TMP".gz' EXIT
if command -v curl >/dev/null 2>&1; then
  curl -fsSL %s -o "$TMP"
elif command -v wget >/dev/null 2>&1; then
  wget -q -O "$TMP" %s
else
  echo "ptyrelay-fetch: neither curl nor wget on PATH" 1>&2
  exit 1
fi
%s%s
chmod 755 "$TMP"
mv "$TMP" %s
trap - EXIT
`, qURL, qURL, gunzipBlock, verifyBlock, qInst)

	var nb [8]byte
	if _, err := rand.Read(nb[:]); err != nil {
		return fmt.Errorf("nonce: %w", err)
	}
	scriptPath := "/tmp/ptyrelay-fetch." + hex.EncodeToString(nb[:]) + ".sh"
	if err := sb.Write(ctx, scriptPath, []byte(inner), 0o700); err != nil {
		return fmt.Errorf("stage fetch script: %w", err)
	}

	// Subshell isolates `exit $rc` so it can't kill the framed bash. The
	// trailing `rm` cleans up the staged script whether sh exited 0 or not.
	qScript := shellquote.Quote(scriptPath)
	runCmd := "( sh " + qScript + "; __rc=$?; rm -f " + qScript + "; exit $__rc )"
	res, err := sb.Run(ctx, runCmd, nil)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("remote fetch failed (exit %d): %s",
			res.ExitCode, strings.TrimSpace(string(res.Stdout)+string(res.Stderr)))
	}
	return nil
}
