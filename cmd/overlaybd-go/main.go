//go:build (linux || windows) && (amd64 || arm64)

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/bigvan/overlaybd-go/iscsi"
	"github.com/bigvan/overlaybd-go/lsmt"
	"github.com/bigvan/overlaybd-go/zfile"
)

const usage = `overlaybd-go: local LSMT/ZFile storage over ublk or iSCSI

Commands:
  create   --data FILE --index FILE --size 1GiB [--hybrid]
  serve    [--lower LAYER ...] [--data FILE --index FILE] [--id N] [--queue-depth 32] [--io-engine 0|3|4]
  iscsi    [--lower LAYER ...] [--data FILE --index FILE] [--listen 127.0.0.1:3260] [--target IQN]
  delete   --id N
  commit   --data FILE --index FILE --output LAYER [--io-engine 0|3|4]
  read     [--lower LAYER ...] [--data FILE --index FILE] --offset 0 --length 4KiB [--output FILE] [--io-engine 0|3|4]
  write    --data FILE --index FILE --offset 0 --input FILE [--io-engine 0|3|4]
  zfile compress   --input FILE --output FILE [--algorithm lz4|zstd] [--block-size 4KiB]
  zfile decompress --input FILE --output FILE
  zfile verify     --input FILE

serve creates /dev/ublkbN on Linux. iscsi exports LUN 0 on loopback on Windows/Linux.
Both run in the foreground until interrupted. Windows supports only --io-engine 0.
Lower layers are ordered bottom to top; ZFile wrappers are detected automatically.
Writable data ioEngine: 0=psync buffered (default), 3=io_uring buffered, 4=io_uring direct.
New output files are never overwritten. Sizes accept B, KiB, MiB, GiB, TiB.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "overlaybd-go:", err)
		os.Exit(1)
	}
}
func flags(name string) *flag.FlagSet { return flag.NewFlagSet(name, flag.ContinueOnError) }
func parse(f *flag.FlagSet, args []string) error {
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", f.Args())
	}
	return nil
}
func required(values ...string) error {
	for _, v := range values {
		if v == "" {
			return errors.New("missing required file path; run with --help")
		}
	}
	return nil
}

func run(args []string) (err error) {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Print(usage)
		return nil
	}
	defer func() {
		if errors.Is(err, flag.ErrHelp) {
			err = nil
		}
	}()
	switch args[0] {
	case "create":
		f := flags("create")
		data := f.String("data", "", "new data file")
		index := f.String("index", "", "new index file")
		size := f.String("size", "", "virtual size, e.g. 1GiB")
		hybrid := f.Bool("hybrid", false, "overwrite mapped ranges in place")
		if err = parse(f, args[1:]); err != nil {
			return err
		}
		if err = required(*data, *index, *size); err != nil {
			return err
		}
		n, err := parseSize(*size)
		if err != nil {
			return err
		}
		create := lsmt.Create
		if *hybrid {
			create = lsmt.CreateHybrid
		}
		layer, err := create(*data, *index, n)
		if err != nil {
			return err
		}
		return layer.Close()
	case "commit":
		f := flags("commit")
		data := f.String("data", "", "data file")
		index := f.String("index", "", "index file")
		out := f.String("output", "", "new sealed layer")
		engine := f.Int("io-engine", 0, "writable data backend: 0=psync, 3=io_uring buffered, 4=io_uring direct")
		if err = parse(f, args[1:]); err != nil {
			return err
		}
		if err = required(*data, *index, *out); err != nil {
			return err
		}
		layer, err := lsmt.OpenRWWithOptions(*data, *index, lsmt.OpenOptions{IOEngine: lsmt.IOEngine(*engine)})
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, layer.Close()) }()
		return layer.Commit(*out)
	case "serve", "iscsi", "read", "write":
		return imageCommand(args)
	case "delete":
		f := flags("delete")
		id := f.Int("id", -1, "device ID to stop and remove")
		if err = parse(f, args[1:]); err != nil {
			return err
		}
		return deleteDevice(*id)
	case "zfile":
		return zfileCommand(args[1:])
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
}

type paths []string

func (p *paths) String() string     { return strings.Join(*p, ",") }
func (p *paths) Set(s string) error { *p = append(*p, s); return nil }

func openImage(data, index string, lowers []string, engine lsmt.IOEngine) (*lsmt.Stack, error) {
	if (data == "") != (index == "") {
		return nil, errors.New("--data and --index must be used together")
	}
	if engine != lsmt.PSync && engine != lsmt.IOUringBuffered && engine != lsmt.IOUringDirect {
		return nil, fmt.Errorf("unsupported ioEngine %d (use 0, 3 or 4)", engine)
	}
	if data == "" && engine != lsmt.PSync {
		return nil, errors.New("--io-engine selects the writable data backend; --data and --index are required")
	}
	var files []*lsmt.File
	var upper *lsmt.File
	success := false
	defer func() {
		if !success {
			for _, f := range files {
				f.Close()
			}
			if upper != nil {
				upper.Close()
			}
		}
	}()
	for _, path := range lowers {
		f, err := lsmt.OpenRO(path)
		if err != nil {
			return nil, fmt.Errorf("lower %s: %w", path, err)
		}
		files = append(files, f)
	}
	if data != "" {
		var err error
		upper, err = lsmt.OpenRWWithOptions(data, index, lsmt.OpenOptions{IOEngine: engine})
		if err != nil {
			return nil, err
		}
	}
	s, err := lsmt.NewStack(files, upper)
	if err != nil {
		return nil, err
	}
	success = true
	return s, nil
}

func imageCommand(args []string) (err error) {
	command := args[0]
	f := flags(command)
	data := f.String("data", "", "writable data file")
	index := f.String("index", "", "writable index file")
	engine := f.Int("io-engine", 0, "writable data backend: 0=psync, 3=io_uring buffered, 4=io_uring direct")
	var lowers paths
	f.Var(&lowers, "lower", "sealed lower layer, repeat in bottom-to-top order")
	var id, depth int
	var listen, target string
	var off, length, input, output string
	if command == "serve" {
		f.IntVar(&id, "id", -1, "device ID; -1 allocates automatically")
		f.IntVar(&depth, "queue-depth", 32, "queue depth 1..128")
	} else if command == "iscsi" {
		f.StringVar(&listen, "listen", "127.0.0.1:3260", "loopback TCP address")
		f.StringVar(&target, "target", "iqn.2026-11.io.overlaybd:disk", "iSCSI target IQN")
	} else {
		f.StringVar(&off, "offset", "0", "byte offset")
		if command == "read" {
			f.StringVar(&length, "length", "0", "bytes to read; 0 reads to end")
			f.StringVar(&output, "output", "-", "new output file or - for stdout")
		} else {
			f.StringVar(&input, "input", "", "input file (512-byte aligned length)")
		}
	}
	if err = parse(f, args[1:]); err != nil {
		return err
	}
	// Parse requests before opening writable files (which can recover a partial tail).
	var offset, count int64
	if command != "serve" && command != "iscsi" {
		offset, err = parseSize(off)
		if err != nil {
			return err
		}
	}
	if command == "read" {
		count, err = parseSize(length)
		if err != nil {
			return err
		}
	}
	if command == "write" {
		if err = required(input); err != nil {
			return err
		}
	}
	image, err := openImage(*data, *index, lowers, lsmt.IOEngine(*engine))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, image.Close()) }()
	if command == "serve" || command == "iscsi" {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		var outputErr error
		ready := func(info any) {
			outputErr = json.NewEncoder(os.Stdout).Encode(info)
			if outputErr != nil {
				cancel()
			}
		}
		if command == "iscsi" {
			err = iscsi.Serve(ctx, image, iscsi.Options{Listen: listen, TargetName: target}, func(info iscsi.Info) { ready(info) })
		} else {
			err = serveDevice(ctx, image, id, depth, *engine, ready)
		}
		return errors.Join(err, outputErr)
	}
	if offset > image.Size() {
		return errors.New("offset exceeds device size")
	}
	if command == "read" {
		if count == 0 {
			count = image.Size() - offset
		}
		if count > image.Size()-offset {
			return errors.New("read exceeds device size")
		}
		return outputFile(output, func(w io.Writer) error { _, err := io.Copy(w, io.NewSectionReader(image, offset, count)); return err })
	}
	src, err := os.Open(input)
	if err != nil {
		return err
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size()%512 != 0 || offset%512 != 0 || st.Size() > image.Size()-offset {
		return errors.New("write requires a regular input file, sector alignment and an in-bounds range")
	}
	if _, err = io.Copy(io.NewOffsetWriter(image, offset), src); err != nil {
		return err
	}
	return image.Sync()
}

func zfileCommand(args []string) (err error) {
	if len(args) == 0 {
		return errors.New("expected zfile compress, decompress or verify")
	}
	f := flags("zfile " + args[0])
	input := f.String("input", "", "input file")
	var output, algorithm, block string
	if args[0] != "verify" {
		f.StringVar(&output, "output", "", "new output file")
	}
	if args[0] == "compress" {
		f.StringVar(&algorithm, "algorithm", "lz4", "lz4 or zstd")
		f.StringVar(&block, "block-size", "4KiB", "power of two from 4KiB to 64KiB")
	}
	if args[0] != "compress" && args[0] != "decompress" && args[0] != "verify" {
		return errors.New("unknown zfile command")
	}
	if err = parse(f, args[1:]); err != nil {
		return err
	}
	if err = required(*input); err != nil {
		return err
	}
	if args[0] != "verify" {
		if err = required(output); err != nil {
			return err
		}
	}
	if args[0] == "compress" {
		size, err := parseSize(block)
		if err != nil {
			return err
		}
		if size > 65536 || size < 4096 {
			return errors.New("block size must be 4KiB..64KiB")
		}
		src, err := os.Open(*input)
		if err != nil {
			return err
		}
		defer src.Close()
		return outputFile(output, func(w io.Writer) error {
			return zfile.Compress(w, src, zfile.Options{Algorithm: algorithm, BlockSize: int(size)})
		})
	}
	r, err := zfile.Open(*input)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, r.Close()) }()
	copyTo := func(w io.Writer) error { _, e := io.Copy(w, io.NewSectionReader(r, 0, r.Size())); return e }
	if args[0] == "verify" {
		return copyTo(io.Discard)
	}
	return outputFile(output, copyTo)
}

func outputFile(path string, write func(io.Writer) error) (err error) {
	if path == "-" {
		return write(os.Stdout)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, f.Close())
		if err != nil {
			os.Remove(path)
		}
	}()
	if err = write(f); err != nil {
		return err
	}
	return f.Sync()
}

func parseSize(s string) (int64, error) {
	for _, unit := range []struct {
		suffix string
		mult   int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(s, unit.suffix) {
			n, err := strconv.ParseInt(strings.TrimSuffix(s, unit.suffix), 10, 64)
			if err != nil || n < 0 || n > math.MaxInt64/unit.mult {
				return 0, fmt.Errorf("invalid size %q", s)
			}
			return n * unit.mult, nil
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n, nil
}
