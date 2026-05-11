package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

func cmdGet(args []string) int {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	common := &commonFlags{}
	common.register(fs)
	outPath := fs.String("o", "", "write to this local file (default stdout)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: ptyrelay get [flags] <remote-path>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fs.Usage()
		return 2
	}

	conn, err := dial(common)
	if err != nil {
		return fail("%v", err)
	}
	defer conn.Close()

	rc, err := conn.Backend.OpenRead(conn.Ctx, rest[0])
	if err != nil {
		return fail("open %s: %v", rest[0], err)
	}
	defer rc.Close()

	var w io.Writer = os.Stdout
	if *outPath != "" {
		f, err := os.Create(*outPath)
		if err != nil {
			return fail("create %s: %v", *outPath, err)
		}
		defer f.Close()
		w = f
	}
	if _, err := io.Copy(w, rc); err != nil {
		return fail("copy: %v", err)
	}
	return 0
}

func cmdPut(args []string) int {
	fs := flag.NewFlagSet("put", flag.ContinueOnError)
	common := &commonFlags{}
	common.register(fs)
	modeFlag := fs.Int("mode", 0o644, "remote file mode (octal int, e.g. 0o755)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: ptyrelay put [flags] <local-path> <remote-path>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 2 {
		fs.Usage()
		return 2
	}
	local, remote := rest[0], rest[1]

	data, err := os.ReadFile(local)
	if err != nil {
		return fail("read %s: %v", local, err)
	}

	conn, err := dial(common)
	if err != nil {
		return fail("%v", err)
	}
	defer conn.Close()

	if err := conn.Backend.Write(conn.Ctx, remote, data, os.FileMode(*modeFlag)); err != nil {
		return fail("write %s: %v", remote, err)
	}
	return 0
}

func cmdStat(args []string) int {
	fs := flag.NewFlagSet("stat", flag.ContinueOnError)
	common := &commonFlags{}
	common.register(fs)
	follow := fs.Bool("L", true, "follow symlinks (false = lstat)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: ptyrelay stat [flags] <remote-path>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fs.Usage()
		return 2
	}

	conn, err := dial(common)
	if err != nil {
		return fail("%v", err)
	}
	defer conn.Close()

	var fi any
	if *follow {
		fi, err = conn.Backend.Stat(conn.Ctx, rest[0])
	} else {
		fi, err = conn.Backend.Lstat(conn.Ctx, rest[0])
	}
	if err != nil {
		return fail("stat %s: %v", rest[0], err)
	}
	fmt.Printf("%+v\n", fi)
	return 0
}

func cmdList(args []string) int {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	common := &commonFlags{}
	common.register(fs)
	long := fs.Bool("l", false, "long listing (size, mode, mtime)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: ptyrelay ls [flags] <remote-path>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fs.Usage()
		return 2
	}

	conn, err := dial(common)
	if err != nil {
		return fail("%v", err)
	}
	defer conn.Close()

	entries, err := conn.Backend.List(conn.Ctx, rest[0])
	if err != nil {
		return fail("list %s: %v", rest[0], err)
	}
	for _, e := range entries {
		if *long {
			fmt.Printf("%s\t%d\t%s\t%s\n", e.Mode, e.Size, e.ModTime.Format("2006-01-02 15:04:05"), e.Name)
		} else {
			fmt.Println(e.Name)
		}
	}
	return 0
}
