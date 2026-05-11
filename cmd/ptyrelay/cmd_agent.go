package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/FanBB2333/ptyrelay/pkg/bootstrap"
)

func cmdBootstrap(args []string) int {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	common := &commonFlags{}
	common.register(fs)
	verify := fs.Bool("verify", true, "ping the agent after install to confirm it answers")
	fromURL := fs.String("from-url", "", "URL template; remote fetches via curl/wget. {os}/{arch} are substituted (e.g. https://host/agent-{os}-{arch}.gz)")
	fromSHA := fs.String("from-url-sha256", "", "expected sha256 hex for the fetched binary; empty disables verification")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: ptyrelay bootstrap [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	embedded := embeddedProvider()
	if *fromURL == "" && common.providerDir == "" && embedded == nil {
		return fail("bootstrap: --from-url or --provider-dir is required (or rebuild with -tags embedagents)")
	}

	// Force --no-agent for the dial — we are about to install the agent,
	// so probing it pre-install is just noise.
	common.noAgent = true
	conn, err := dial(common)
	if err != nil {
		return fail("%v", err)
	}
	defer conn.Close()

	bopts := bootstrap.Options{InstallPath: common.agentPath}
	if *fromURL != "" {
		tmpl := *fromURL
		sha := *fromSHA
		bopts.FromURL = func(osName, arch string) (string, string) {
			u := strings.ReplaceAll(tmpl, "{os}", osName)
			u = strings.ReplaceAll(u, "{arch}", arch)
			return u, sha
		}
	} else if common.providerDir != "" {
		bopts.Provider = &bootstrap.FileProvider{Dir: common.providerDir}
	} else {
		// Embedded fallback; only reached when build tag is set,
		// guarded above so this branch is never nil.
		bopts.Provider = embedded
	}

	path, err := bootstrap.Bootstrap(conn.Ctx, conn.Shell, bopts)
	if err != nil {
		return fail("bootstrap: %v", err)
	}
	fmt.Printf("installed: %s\n", path)

	if *verify {
		if err := bootstrap.VerifyInstall(conn.Ctx, conn.Shell, path); err != nil {
			return fail("verify: %v", err)
		}
		fmt.Println("verified: ping ok")
	}
	return 0
}

func cmdAgentInfo(args []string) int {
	fs := flag.NewFlagSet("agent-info", flag.ContinueOnError)
	common := &commonFlags{}
	common.register(fs)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: ptyrelay agent-info [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	conn, err := dial(common)
	if err != nil {
		return fail("%v", err)
	}
	defer conn.Close()

	if conn.Agent == nil {
		return fail("agent-info requires the agent path (do not pass --no-agent)")
	}

	// stat for size, ping for version.
	fi, err := conn.Shell.Stat(conn.Ctx, conn.Agent.AgentPath())
	if err != nil {
		return fail("stat agent: %v", err)
	}
	fmt.Printf("path:    %s\n", conn.Agent.AgentPath())
	fmt.Printf("size:    %d bytes\n", fi.Size)
	fmt.Printf("mode:    %s\n", fi.Mode)
	fmt.Printf("mtime:   %s\n", fi.ModTime.Format("2006-01-02 15:04:05"))

	if err := conn.Agent.Probe(conn.Ctx); err != nil {
		fmt.Printf("status:  unhealthy (%v)\n", err)
		return 1
	}
	fmt.Println("status:  healthy")
	return 0
}
