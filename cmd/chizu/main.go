// Command chizu is the single binary for every chizu plane: crawl, build,
// serve, root, and the dev harness. Spec 2107 defines the surface; planes
// land milestone by milestone.
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/tamnd/chizu/chain"
	"github.com/tamnd/chizu/s3c"
)

const version = "0.0.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println("chizu " + version)
	case "admin":
		if err := admin(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "chizu:", err)
			os.Exit(1)
		}
	case "dev":
		if err := dev(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "chizu:", err)
			os.Exit(1)
		}
	case "fixture":
		if err := fixtureCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "chizu:", err)
			os.Exit(1)
		}
	case "serve":
		if err := serveCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "chizu:", err)
			os.Exit(1)
		}
	case "replay":
		if err := replayCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "chizu:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "chizu: unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: chizu <command>")
	fmt.Fprintln(os.Stderr, "commands: version, admin create, dev, fixture, serve, replay")
	fmt.Fprintln(os.Stderr, "more arrive milestone by milestone")
}

func admin(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: chizu admin create [flags]")
	}
	switch args[0] {
	case "create":
		return adminCreate(args[1:])
	default:
		return fmt.Errorf("unknown admin subcommand %q", args[0])
	}
}

// adminCreate makes a database: it probes the bucket's conditional-write
// support, then CAS-creates the root carrying the frozen knobs (doc 02
// section 2). The bucket connection comes from the CHIZU_S3_* environment.
func adminCreate(args []string) error {
	fs := flag.NewFlagSet("chizu admin create", flag.ContinueOnError)
	prefix := fs.String("prefix", "", "database key prefix inside the bucket")
	p := fs.Uint("p", 4096, "crawl partition count, frozen at create")
	shardSize := fs.Uint("shard-size", 6_000_000, "docs per shard policy, frozen at create")
	law := fs.Uint("law", 1, "canonicalization law version, frozen at create")
	tok := fs.Uint("tokenizer", 1, "tokenizer version, frozen at create")
	quant := fs.Uint("quant", 1, "quantization scale policy version, frozen at create")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *p == 0 || *p > 0xFFFF {
		return fmt.Errorf("p %d is outside the u16 partition space", *p)
	}
	if *shardSize == 0 || *shardSize > 0xFFFFFFFF {
		return fmt.Errorf("shard-size %d is outside the u32 space", *shardSize)
	}

	cfg := s3c.FromEnv()
	if cfg.Endpoint == "" {
		return errors.New("CHIZU_S3_ENDPOINT is unset; admin create needs the CHIZU_S3_* environment")
	}
	client, err := s3c.New(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := client.CreateBucket(ctx); err != nil {
		return err
	}

	ifMatch, err := client.ProbeConditionalWrites(ctx, *prefix+"probe/cas")
	if err != nil {
		return err
	}
	mode := "cas"
	if !ifMatch {
		mode = "rootv sequence (bucket ignores If-Match)"
	}

	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	root := &chain.Root{
		DBID:      binary.LittleEndian.Uint64(raw[:]),
		CreatedMS: uint64(time.Now().UnixMilli()),
		P:         uint16(*p),
		ShardSize: uint32(*shardSize),
		Frozen:    fmt.Appendf(nil, "law=%d tok=%d quant=%d", *law, *tok, *quant),
	}
	rs := chain.NewRootStore(client, *prefix, 0, !ifMatch)
	if err := rs.Create(ctx, root); err != nil {
		if errors.Is(err, s3c.ErrPrecondition) {
			return fmt.Errorf("a database already exists at prefix %q", *prefix)
		}
		return err
	}
	fmt.Printf("created database %016x at prefix %q\n", root.DBID, *prefix)
	fmt.Printf("root mode: %s\n", mode)
	fmt.Printf("frozen: P=%d shard_size=%d %s\n", root.P, root.ShardSize, root.Frozen)
	return nil
}
