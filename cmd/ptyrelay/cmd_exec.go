package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

func cmdExec(args []string) int {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	common := &commonFlags{}
	common.register(fs)
	stdinFlag := fs.Bool("stdin", false, "read stdin and pipe it to the remote command")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: ptyrelay exec [flags] -- <cmd> [args...]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return 2
	}
	cmd := strings.Join(rest, " ")

	conn, err := dial(common)
	if err != nil {
		return fail("%v", err)
	}
	defer conn.Close()

	var stdin []byte
	if *stdinFlag {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fail("read stdin: %v", err)
		}
		stdin = b
	}

	res, err := conn.Backend.Run(conn.Ctx, cmd, stdin)
	if err != nil {
		return fail("run: %v", err)
	}
	_, _ = os.Stdout.Write(res.Stdout)
	_, _ = os.Stderr.Write(res.Stderr)
	if res.ExitCode != 0 {
		// Match the conventional shell-pipeline exit: surface the
		// remote's exit code so callers can branch on it.
		return res.ExitCode
	}
	return 0
}
