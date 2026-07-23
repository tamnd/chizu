// chizu serve: mount .hot shards and answer QUERY frames on a TCP
// port, the single-node serve loop of doc 07. This is the dev-shape
// entry point; generation download and chain following arrive with
// their own slices.

package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/tamnd/chizu/serve"
)

func serveCmd(args []string) error {
	fs := flag.NewFlagSet("chizu serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:4980", "listen address")
	mlock := fs.String("mlock", "try", "mlock mode: try, required, off")
	inflight := fs.Int("inflight", 64, "concurrent queries before BUSY")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: chizu serve [flags] shard.hot...")
	}
	var mode serve.MlockMode
	switch *mlock {
	case "try":
		mode = serve.MlockTry
	case "required":
		mode = serve.MlockRequired
	case "off":
		mode = serve.MlockOff
	default:
		return fmt.Errorf("unknown mlock mode %q", *mlock)
	}

	reg := serve.NewRegistry()
	for _, path := range fs.Args() {
		m, err := serve.MountShard(path, serve.Options{Mlock: mode})
		if err != nil {
			return fmt.Errorf("mount %s: %w", path, err)
		}
		if err := reg.Publish(m); err != nil {
			return err
		}
		res := m.Residency()
		fmt.Printf("mounted shard %d gen %d: %d docs, resident %d bytes, locked %d\n",
			m.Shard.Header.Shard, m.Shard.Header.Generation,
			m.Shard.Header.DocCount, res.ResidentPrefix, res.Locked)
	}

	l, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	srv := serve.NewServer(reg, serve.ServerOptions{NodeID: 1, MaxInflight: *inflight})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		_ = srv.Close()
	}()
	fmt.Printf("serving on %s (inflight cap %d)\n", l.Addr(), *inflight)
	return srv.Serve(l)
}
