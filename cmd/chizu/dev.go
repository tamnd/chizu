package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/tamnd/chizu/build"
	"github.com/tamnd/chizu/chain"
	"github.com/tamnd/chizu/coldfmt"
	"github.com/tamnd/chizu/hotfmt"
	"github.com/tamnd/chizu/s3c"
	"github.com/tamnd/chizu/wire"
)

// dev is the harness of doc 02 section 2: every plane as a stub in one
// process against one bucket, so each later slice lands into a running
// vertical. A fixture corpus goes through a cold page segment, comes
// back out as a hot shard, and answers a query over real wire frames.
// The CG2 gate (doc 09) starts here: the s3c request counter must not
// move while the query round runs.
func dev(args []string) error {
	fs := flag.NewFlagSet("chizu dev", flag.ContinueOnError)
	prefix := fs.String("prefix", "dev/", "database key prefix inside the bucket")
	fixtureN := fs.Uint64("fixture", 0, "build and serve an N-doc fixture shard instead of the 8-page corpus")
	scratch := fs.String("scratch", "", "scratch directory for build spools and the .hot (default: a temp dir)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg := s3c.FromEnv()
	if cfg.Endpoint == "" {
		return errors.New("CHIZU_S3_ENDPOINT is unset; dev needs a bucket (MinIO works) from the CHIZU_S3_* environment")
	}
	client, err := s3c.New(cfg)
	if err != nil {
		return err
	}
	// The 8-page vertical is over in seconds; a 100M-doc fixture build
	// owns the box for hours.
	timeout := 2 * time.Minute
	if *fixtureN > 0 {
		timeout = 12 * time.Hour
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := client.CreateBucket(ctx); err != nil {
		return err
	}
	if *fixtureN > 0 {
		return devFixture(ctx, client, *prefix, *scratch, *fixtureN)
	}
	root, err := devRoot(ctx, client, *prefix)
	if err != nil {
		return err
	}
	fmt.Printf("dev database %016x at prefix %q\n", root.DBID, *prefix)

	// Crawl stub: the fixture corpus sealed into one cold page segment.
	rows := devCorpus()
	w := &coldfmt.PageSegmentWriter{Partition: 0, Epoch: 1, Seq: 1, Writer: root.DBID}
	for _, r := range rows {
		w.Add(r)
	}
	seg, err := w.Seal()
	if err != nil {
		return err
	}
	pageKey := *prefix + "cold/page/p0000/0000000000000001.cold"
	if _, err := client.Put(ctx, pageKey, seg); err != nil {
		return err
	}
	fmt.Printf("crawl stub: %d pages -> %s (%d bytes)\n", len(rows), pageKey, len(seg))

	// Build stub: read the segment back from the bucket, build the shard.
	segBack, _, err := client.Get(ctx, pageKey)
	if err != nil {
		return err
	}
	ps, err := coldfmt.OpenPageSegment(segBack)
	if err != nil {
		return err
	}
	crawled, err := ps.Rows()
	ps.Close()
	if err != nil {
		return err
	}
	shardBytes, err := devBuildShard(crawled)
	if err != nil {
		return err
	}
	shardKey := *prefix + "hot/s0000/0000000000000001.hot"
	hotPath := filepath.Join(os.TempDir(), fmt.Sprintf("chizu-dev-%016x.hot", root.DBID))
	if err := os.WriteFile(hotPath, shardBytes, 0o644); err != nil {
		return err
	}
	defer func() { _ = os.Remove(hotPath) }()
	if err := build.UploadHot(ctx, client, shardKey, hotPath, build.DefaultPartSize); err != nil {
		return err
	}
	fmt.Printf("build stub: %d docs -> %s (%d bytes)\n", len(crawled), shardKey, len(shardBytes))

	// Serve stub: pull the shard once and hold every band in memory.
	// After this point the bucket is off limits; that is CG2.
	hot, _, err := client.Get(ctx, shardKey)
	if err != nil {
		return err
	}
	sh, err := openDevShard(hot)
	if err != nil {
		return err
	}
	defer sh.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- devServe(ln, sh) }()

	before := client.Requests()
	res, snips, err := devRootRound(ln.Addr().String(), devQueryTerms, 3)
	if err != nil {
		return err
	}
	if d := client.Requests() - before; d != 0 {
		return fmt.Errorf("CG2 violated: %d bucket requests on the query path", d)
	}
	_ = ln.Close()
	if err := <-serveErr; err != nil {
		return err
	}

	if err := devCheckAnswers(res, snips); err != nil {
		return err
	}
	for i, e := range res.Entries {
		rec := snips.Records[i]
		fmt.Printf("  #%d doc %d score %d %s | %s\n", i+1, e.Docid, e.Score, rec.URL, rec.Title)
	}
	fmt.Println("cg2: 0 bucket requests on the query path")
	return nil
}

// devRoot loads the database root at prefix, creating it on first run
// so `chizu dev` works repeatedly against the same bucket.
func devRoot(ctx context.Context, client *s3c.Client, prefix string) (*chain.Root, error) {
	ifMatch, err := client.ProbeConditionalWrites(ctx, prefix+"probe/cas")
	if err != nil {
		return nil, err
	}
	rs := chain.NewRootStore(client, prefix, 1, !ifMatch)
	root, err := rs.Load(ctx)
	if err == nil {
		return root, nil
	}
	if !errors.Is(err, s3c.ErrNotFound) {
		return nil, err
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	root = &chain.Root{
		DBID:      binary.LittleEndian.Uint64(raw[:]),
		CreatedMS: uint64(time.Now().UnixMilli()),
		P:         16,
		ShardSize: 100_000,
		Frozen:    fmt.Appendf(nil, "law=1 tok=1 quant=1"),
	}
	if err := rs.Create(ctx, root); err != nil {
		return nil, err
	}
	return root, nil
}

// devQueryTerms is the fixture query; devCheckAnswers pins what it must
// return, so the harness is a gate and not just a demo.
var devQueryTerms = []string{"mountain", "river"}

// devCorpus is eight tiny pages. The words are chosen so the fixture
// query exercises both dictionary paths: "mountain" has df 3 and stays
// inlined, "river" and "the" go through FOR postings and skips.
func devCorpus() []coldfmt.PageRow {
	pages := []struct{ url, title, text string }{
		{"https://dev.chizu.test/fuji", "Mount Fuji", "fuji is the tallest mountain in japan and a sacred mountain"},
		{"https://dev.chizu.test/alps", "The Alps", "the alps mountain range crosses europe"},
		{"https://dev.chizu.test/andes", "Andes Mountain Chain", "the andes mountain chain feeds every river on the continent with mountain meltwater"},
		{"https://dev.chizu.test/amazon", "Amazon River", "the amazon river carries more water than any other river"},
		{"https://dev.chizu.test/nile", "The Nile", "the nile river flows north across the desert"},
		{"https://dev.chizu.test/sumida", "Sumida", "tokyo sits where the sumida river meets the bay"},
		{"https://dev.chizu.test/kamo", "Kamo", "the kamo river runs through kyoto"},
		{"https://dev.chizu.test/osaka", "Osaka", "osaka is a port city known for street food"},
	}
	rows := make([]coldfmt.PageRow, len(pages))
	for i, p := range pages {
		fp := sha256.Sum256([]byte(p.url))
		rows[i] = coldfmt.PageRow{
			URL:     p.url,
			Title:   p.title,
			Text:    p.text,
			FetchMS: 1_750_000_000_000 + uint64(i),
			Status:  200,
			SHA256:  sha256.Sum256([]byte(p.text)),
			Lang:    1,
			LawVer:  1,
		}
		copy(rows[i].URLFP[:], fp[:16])
		rows[i].CanonFP = rows[i].URLFP
		if i+1 < len(pages) {
			next := sha256.Sum256([]byte(pages[i+1].url))
			var dst [16]byte
			copy(dst[:], next[:16])
			rows[i].Outlinks = []coldfmt.Outlink{{Dst: dst, Anchor: "next"}}
		}
	}
	return rows
}

// devBuildShard runs the real build pipeline: the shard pass spills
// sorted runs, the emit pass merges them into a sealed .hot on scratch
// disk, and the bytes come back for the multipart upload. BuildMS is
// the corpus's max fetch watermark, so the shard stays a pure function
// of its input (B2).
func devBuildShard(rows []coldfmt.PageRow) ([]byte, error) {
	dir, err := os.MkdirTemp("", "chizu-dev-build-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	p := build.NewShardPass(dir, build.DefaultRunBudget)
	var watermark uint64
	for i := range rows {
		watermark = max(watermark, rows[i].FetchMS)
		if err := p.AddRow(&rows[i]); err != nil {
			return nil, err
		}
	}
	out, err := p.Finish()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	cfg := &build.EmitConfig{
		SpoolDir:   dir,
		Shard:      0,
		Generation: 1,
		Writer:     1,
		Builder:    1,
		BuildMS:    watermark,
		Watermarks: []uint64{watermark},
	}
	if err := build.Emit(&buf, out, cfg); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// devShard is the serve stub's residency: every band held in memory so
// the query path never needs the bucket.
type devShard struct {
	dict      *hotfmt.Dict
	postings  []byte
	skips     []byte
	positions []byte
	dv        *hotfmt.DocValues
	docs      *hotfmt.DocBand
	docCount  uint32
}

func (s *devShard) Close() { s.docs.Close() }

func openDevShard(data []byte) (*devShard, error) {
	s, err := hotfmt.Open(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	if err := s.VerifyBands(); err != nil {
		return nil, err
	}
	band := func(id byte) []byte {
		if err != nil {
			return nil
		}
		var b []byte
		b, err = s.ReadBand(id)
		return b
	}
	dictBand := band(hotfmt.BandDict)
	postings := band(hotfmt.BandPostings)
	skips := band(hotfmt.BandSkips)
	positions := band(hotfmt.BandPositions)
	dvBand := band(hotfmt.BandDocvalues)
	docBand := band(hotfmt.BandDocband)
	if err != nil {
		return nil, err
	}
	dict, err := hotfmt.OpenDict(dictBand)
	if err != nil {
		return nil, err
	}
	dv, err := hotfmt.OpenDocValues(dvBand, s.Header.DocCount)
	if err != nil {
		return nil, err
	}
	docs, err := hotfmt.OpenDocBand(docBand)
	if err != nil {
		return nil, err
	}
	return &devShard{
		dict: dict, postings: postings, skips: skips, positions: positions,
		dv: dv, docs: docs, docCount: s.Header.DocCount,
	}, nil
}

// devServe accepts connections until the listener closes and answers
// frames from the in-memory shard.
func devServe(ln net.Listener, sh *devShard) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return nil // listener closed: clean shutdown
		}
		if err := devServeConn(conn, sh); err != nil {
			return err
		}
	}
}

func devServeConn(conn net.Conn, sh *devShard) error {
	defer func() { _ = conn.Close() }()
	fr := wire.NewFrameReader(conn)
	for {
		f, err := fr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		body, err := wire.DecodeBody(f)
		if err != nil {
			return err
		}
		var kind byte
		var out []byte
		switch b := body.(type) {
		case *wire.Hello:
			kind = wire.KindHello
			out, err = wire.AppendHello(nil, &wire.Hello{Version: 1, NodeID: 1, MaxInflight: 16, MaxFrame: wire.MaxFrame})
		case *wire.Query:
			kind = wire.KindQResult
			var r *wire.QResult
			if r, err = devQuery(sh, b); err == nil {
				out, err = wire.AppendQResult(nil, r)
			}
		case *wire.Snip:
			kind = wire.KindSnipResult
			var r *wire.SnipResult
			if r, err = devSnip(sh, b); err == nil {
				out, err = wire.AppendSnipResult(nil, r)
			}
		default:
			return fmt.Errorf("dev serve: unexpected frame kind %d", f.Kind)
		}
		if err != nil {
			return err
		}
		frame, err := wire.AppendFrame(nil, kind, f.Reqid, out)
		if err != nil {
			return err
		}
		if _, err := conn.Write(frame); err != nil {
			return err
		}
	}
}

// devQuery is the serve stub's scorer: sum of tf per doc, no BM25F yet
// (doc 08 lands in C3). Inlined terms read the dictionary alone;
// everything else walks FOR blocks from their skip offsets, the
// traversal contract the integration test pins.
func devQuery(sh *devShard, q *wire.Query) (*wire.QResult, error) {
	scores := make(map[uint32]uint32)
	for _, t := range q.Terms {
		e, ok, err := sh.dict.Lookup(t.Term)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if len(e.Inline) > 0 {
			for _, p := range e.Inline {
				scores[p.Docid] += uint32(p.TF)
			}
			continue
		}
		termBytes := sh.postings[e.PostingsOff : e.PostingsOff+uint64(e.PostingsLen)]
		region := sh.skips[e.SkipOff : e.SkipOff+uint64(hotfmt.SkipRegionSize(e.DF))]
		l1, _, _, err := hotfmt.ParseSkips(region, e.DF)
		if err != nil {
			return nil, err
		}
		var docids [hotfmt.PostingsBlockLen]uint32
		var tfs, masks [hotfmt.PostingsBlockLen]uint8
		for bi := range l1 {
			prev := int64(-1)
			if bi > 0 {
				prev = int64(l1[bi-1].LastDocid)
			}
			b, _, err := hotfmt.DecodePostingsBlock(termBytes[l1[bi].Off:], prev, docids[:], tfs[:], masks[:])
			if err != nil {
				return nil, err
			}
			for i := range b.NEntries {
				scores[docids[i]] += uint32(tfs[i])
			}
		}
	}

	type hit struct {
		docid uint32
		score uint32
	}
	hits := make([]hit, 0, len(scores))
	for d, s := range scores {
		hits = append(hits, hit{d, s})
	}
	slices.SortFunc(hits, func(a, b hit) int {
		if a.score != b.score {
			return int(b.score) - int(a.score)
		}
		return int(a.docid) - int(b.docid)
	})
	hits = hits[:min(len(hits), int(q.K))]
	r := &wire.QResult{}
	for _, h := range hits {
		// The docvalues read stands in for the doc 08 priors; for now it
		// only drops spam.
		if sh.dv.At(h.docid).Spam > 128 {
			continue
		}
		r.Entries = append(r.Entries, wire.ResultEntry{
			Docid: uint64(h.docid),
			Score: uint16(min(h.score, 0xFFFF)),
		})
	}
	return r, nil
}

func devSnip(sh *devShard, s *wire.Snip) (*wire.SnipResult, error) {
	r := &wire.SnipResult{}
	for _, docid := range s.Docids {
		rec, err := sh.docs.Doc(uint32(docid))
		if err != nil {
			return nil, err
		}
		r.Records = append(r.Records, wire.SnippetRecord{
			Docid:   docid,
			URL:     rec.URL,
			Title:   rec.Title,
			Snippet: rec.Snippet,
		})
	}
	return r, nil
}

// devRootRound is the root stub: hello, one query, one snippet fetch,
// all through real frames on a real socket, deadlines carried as
// remaining budgets per the wire discipline.
func devRootRound(addr string, terms []string, k uint16) (*wire.QResult, *wire.SnipResult, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = conn.Close() }()
	fr := wire.NewFrameReader(conn)
	deadline := time.Now().Add(2 * time.Second)

	roundTrip := func(kind byte, reqid uint64, body []byte) (wire.Frame, error) {
		frame, err := wire.AppendFrame(nil, kind, reqid, body)
		if err != nil {
			return wire.Frame{}, err
		}
		if _, err := conn.Write(frame); err != nil {
			return wire.Frame{}, err
		}
		f, err := fr.Next()
		if err != nil {
			return wire.Frame{}, err
		}
		if f.Reqid != reqid {
			return wire.Frame{}, fmt.Errorf("dev root: reqid %d answered as %d", reqid, f.Reqid)
		}
		return f, nil
	}

	hb, err := wire.AppendHello(nil, &wire.Hello{Version: 1, NodeID: 2, MaxInflight: 4, MaxFrame: wire.MaxFrame})
	if err != nil {
		return nil, nil, err
	}
	f, err := roundTrip(wire.KindHello, 1, hb)
	if err != nil {
		return nil, nil, err
	}
	if f.Kind != wire.KindHello {
		return nil, nil, fmt.Errorf("dev root: hello answered with kind %d", f.Kind)
	}

	q := &wire.Query{
		BudgetUS:   wire.RemainingUS(deadline, time.Now()),
		K:          k,
		Candidates: 4 * k,
		Shards:     []uint16{0},
	}
	for _, t := range terms {
		q.Terms = append(q.Terms, wire.QueryTerm{Term: []byte(t)})
	}
	qb, err := wire.AppendQuery(nil, q)
	if err != nil {
		return nil, nil, err
	}
	if f, err = roundTrip(wire.KindQuery, 2, qb); err != nil {
		return nil, nil, err
	}
	if f.Kind != wire.KindQResult {
		return nil, nil, fmt.Errorf("dev root: query answered with kind %d", f.Kind)
	}
	res, err := wire.ParseQResult(f.Body)
	if err != nil {
		return nil, nil, err
	}
	if len(res.Entries) == 0 {
		return nil, nil, errors.New("dev root: empty result")
	}

	snip := &wire.Snip{BudgetUS: wire.RemainingUS(deadline, time.Now())}
	for _, t := range terms {
		snip.Terms = append(snip.Terms, []byte(t))
	}
	for _, e := range res.Entries {
		snip.Docids = append(snip.Docids, e.Docid)
	}
	sb, err := wire.AppendSnip(nil, snip)
	if err != nil {
		return nil, nil, err
	}
	if f, err = roundTrip(wire.KindSnip, 3, sb); err != nil {
		return nil, nil, err
	}
	if f.Kind != wire.KindSnipResult {
		return nil, nil, fmt.Errorf("dev root: snip answered with kind %d", f.Kind)
	}
	snips, err := wire.ParseSnipResult(f.Body)
	if err != nil {
		return nil, nil, err
	}
	return res, snips, nil
}

// devCheckAnswers pins the fixture expectations: the andes page holds
// three "mountain" hits (two body, one title) plus one "river", so it
// must win, and its snippet record must carry its URL.
func devCheckAnswers(res *wire.QResult, snips *wire.SnipResult) error {
	if len(res.Entries) != 3 {
		return fmt.Errorf("dev: got %d results, want 3", len(res.Entries))
	}
	top := res.Entries[0]
	if top.Docid != 2 || top.Score != 4 {
		return fmt.Errorf("dev: top hit doc %d score %d, want doc 2 score 4", top.Docid, top.Score)
	}
	if len(snips.Records) != len(res.Entries) {
		return fmt.Errorf("dev: %d snippets for %d results", len(snips.Records), len(res.Entries))
	}
	if got := string(snips.Records[0].URL); got != "https://dev.chizu.test/andes" {
		return fmt.Errorf("dev: top snippet url %q", got)
	}
	return nil
}
