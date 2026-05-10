package bootstrap

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// FileProvider reads agent binaries from a local directory laid out as
// `<dir>/<os>-<arch>` (e.g. `agents/linux-amd64`).
//
// It is the form tests use; production builds prefer EmbedProvider.
type FileProvider struct {
	Dir string
}

// Get implements [Provider].
func (p *FileProvider) Get(osName, arch string) ([]byte, error) {
	path := filepath.Join(p.Dir, osName+"-"+arch)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s/%s", ErrUnsupportedTarget, osName, arch)
		}
		return nil, err
	}
	return data, nil
}

// EmbedProvider reads agent binaries out of an embed.FS rooted at
// `agents/<os>-<arch>`. Production CLIs declare:
//
//	//go:embed agents/*
//	var agents embed.FS
//	provider := &bootstrap.EmbedProvider{FS: agents, Root: "agents"}
type EmbedProvider struct {
	FS   embed.FS
	Root string // subdir within FS, e.g. "agents"
}

// Get implements [Provider].
func (p *EmbedProvider) Get(osName, arch string) ([]byte, error) {
	root := p.Root
	if root == "" {
		root = "agents"
	}
	path := root + "/" + osName + "-" + arch
	data, err := fs.ReadFile(p.FS, path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s (%v)", ErrUnsupportedTarget, path, err)
	}
	return data, nil
}
