package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/aahoughton/smoot/internal/client"
	"github.com/aahoughton/smoot/internal/config"
	"github.com/aahoughton/smoot/internal/core"
)

// splitArgs parses fs from args, allowing exactly n positional arguments
// anywhere among the flags.
func splitArgs(fs *flag.FlagSet, args []string, n int, what string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			break
		}
		pos, args = append(pos, fs.Arg(0)), fs.Args()[1:]
	}
	if len(pos) != n {
		return nil, fmt.Errorf("want %s", what)
	}
	return pos, nil
}

func runAttach(ctx context.Context, args []string, stdout, stderr io.Writer, getenv config.Getenv) int {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, `usage: smoot attach <conversation> FILE --agent NAME [flags]

Uploads FILE (at most 10 MiB) to the conversation as an attachment from the
agent, which must be a participant. Others download it with smoot get. The
server must run with --attachments.

`)
		fs.PrintDefaults()
	}
	resolveClient := config.ClientFlags(fs, config.WithNamespace|config.WithToken)
	resolveAgent := config.AgentFlag(fs)
	name := fs.String("name", "", "file name to attach it under (default: FILE's base name)")
	pos, err := splitArgs(fs, args, 2, "a conversation name and a file")
	if err != nil {
		return flagError(stderr, err)
	}
	conv, path := pos[0], pos[1]
	cc, err := resolveClient(getenv)
	if err != nil {
		return flagError(stderr, err)
	}
	agent, err := resolveAgent(getenv)
	if err != nil {
		return flagError(stderr, err)
	}
	if *name == "" {
		*name = filepath.Base(path)
	}
	if err := core.CheckFileName(*name); err != nil {
		return flagError(stderr, fmt.Errorf("%w; pick another with --name", err))
	}
	cl, err := client.New(cc.URL, cc.Token)
	if err != nil {
		return flagError(stderr, err)
	}
	data, err := readCapped(path, core.MaxAttachmentBytes)
	if err != nil {
		fmt.Fprintln(stderr, "smoot attach:", err)
		return exitError
	}
	res, err := cl.Attach(ctx, cc.Namespace, conv, agent, *name, data)
	if err != nil {
		fmt.Fprintln(stderr, "smoot attach:", err)
		return exitError
	}
	a := res.Message.Attachment
	if a == nil {
		fmt.Fprintln(stderr, "smoot attach: the server did not describe the attachment")
		return exitError
	}
	fmt.Fprintf(stdout, "attached %s as #%d (%d bytes, sha256 %s)\n", a.Name, res.Message.Seq, a.Size, a.SHA256)
	fmt.Fprintln(stdout, res.Standing.Line)
	return exitOK
}

// readCapped reads the file at path, refusing one over limit bytes and
// anything but a regular file.
func readCapped(path string, limit int64) ([]byte, error) {
	// O_NONBLOCK keeps a FIFO from blocking the open, so the regular-file
	// check below can refuse it; regular files ignore the flag.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is over %d bytes", path, limit)
	}
	return data, nil
}

func runGet(ctx context.Context, args []string, stdout, stderr io.Writer, getenv config.Getenv) int {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, `usage: smoot get <conversation> SEQ --agent NAME [-o DIR] [flags]

Downloads the file attached at SEQ into DIR under its attached name, after
checking its sha256. It never overwrites a file, never writes outside DIR,
and never makes the file executable. Treat what you download as untrusted.

`)
		fs.PrintDefaults()
	}
	resolveClient := config.ClientFlags(fs, config.WithNamespace|config.WithToken)
	resolveAgent := config.AgentFlag(fs)
	dir := fs.String("o", ".", "directory to write the file into; it must exist")
	pos, err := splitArgs(fs, args, 2, "a conversation name and a seq")
	if err != nil {
		return flagError(stderr, err)
	}
	conv := pos[0]
	seq, err := strconv.ParseInt(pos[1], 10, 64)
	if err != nil || seq < 1 {
		return flagError(stderr, fmt.Errorf("seq %q is not a positive number", pos[1]))
	}
	cc, err := resolveClient(getenv)
	if err != nil {
		return flagError(stderr, err)
	}
	agent, err := resolveAgent(getenv)
	if err != nil {
		return flagError(stderr, err)
	}
	cl, err := client.New(cc.URL, cc.Token)
	if err != nil {
		return flagError(stderr, err)
	}
	root, err := os.OpenRoot(*dir)
	if err != nil {
		fmt.Fprintln(stderr, "smoot get:", err)
		return exitError
	}
	defer func() { _ = root.Close() }()
	d, err := cl.Fetch(ctx, cc.Namespace, conv, agent, seq)
	if err != nil {
		fmt.Fprintln(stderr, "smoot get:", err)
		return exitError
	}
	if err := writeNew(root, d.Attachment.Name, d.Data); err != nil {
		fmt.Fprintln(stderr, "smoot get:", err)
		return exitError
	}
	fmt.Fprintf(stdout, "wrote %s (%d bytes, sha256 %s)\n", filepath.Join(*dir, d.Attachment.Name), d.Attachment.Size, d.Attachment.SHA256)
	return exitOK
}

// writeNew creates name in root with data, mode 0644. The name must be a
// single path element (core.CheckFileName); root refuses any path that
// would leave it, symlinks included. An existing file is left alone, and a
// partly written file is removed.
func writeNew(root *os.Root, name string, data []byte) error {
	if err := core.CheckFileName(name); err != nil {
		return err
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s already exists in %s; remove it or pass another -o", name, root.Name())
	}
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = root.Remove(name)
	}
	return err
}
