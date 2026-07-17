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
	"maps"
	"net"
	"slices"
	"time"

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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := client.CreateBucket(ctx); err != nil {
		return err
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
	if _, err := client.Put(ctx, shardKey, shardBytes); err != nil {
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

// devTokenize is the stub tokenizer: lowercase ASCII words. The real
// tokenizer (doc 06) replaces it in C2; its version is frozen in the
// root, so the swap is a rebuild, not a migration.
func devTokenize(s string) []string {
	var toks []string
	start := -1
	flush := func(end int) {
		if start >= 0 {
			toks = append(toks, s[start:end])
			start = -1
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if c >= 'A' && c <= 'Z' {
			// Lowercasing rewrites the byte, so cut the word here and
			// build it from a lowered copy instead.
			alnum = true
		}
		if !alnum {
			flush(i)
			continue
		}
		if start < 0 {
			start = i
		}
	}
	flush(len(s))
	for i, t := range toks {
		toks[i] = lowerASCII(t)
	}
	return toks
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// devOcc is one term's occurrences in one doc: total tf and per-field
// positions (0 body, 1 title).
type devOcc struct {
	tf  int
	pos [2][]uint16
}

// devBuildShard is the build stub: crawled rows in, one sealed base
// .hot shard out, every band populated the way doc 05 lays them down.
func devBuildShard(rows []coldfmt.PageRow) ([]byte, error) {
	post := make(map[string]map[uint32]*devOcc)
	var sumLen [2]uint64
	addField := func(docid uint32, field int, text string) uint64 {
		toks := devTokenize(text)
		for i, tok := range toks {
			if i > 0xFFFF {
				break
			}
			m := post[tok]
			if m == nil {
				m = make(map[uint32]*devOcc)
				post[tok] = m
			}
			o := m[docid]
			if o == nil {
				o = &devOcc{}
				m[docid] = o
			}
			o.tf++
			o.pos[field] = append(o.pos[field], uint16(i))
		}
		return uint64(len(toks))
	}

	docCount := uint32(len(rows))
	dvs := make([]hotfmt.DocValue, len(rows))
	var docsW hotfmt.DocBandWriter
	for i, r := range rows {
		docid := uint32(i)
		bl := addField(docid, 0, r.Text)
		tl := addField(docid, 1, r.Title)
		sumLen[0] += bl
		sumLen[1] += tl
		dvs[i] = hotfmt.DocValue{
			Quality:     128,
			Lang:        r.Lang,
			DoclenBody:  uint8(min(bl, 255)),
			DoclenTitle: uint8(min(tl, 15)),
		}
		snippet := r.Text
		if len(snippet) > 120 {
			snippet = snippet[:120]
		}
		err := docsW.Add(hotfmt.DocRecord{
			URL:     []byte(r.URL),
			Title:   []byte(r.Title),
			Snippet: []byte(snippet),
			URLFP:   r.URLFP,
		})
		if err != nil {
			return nil, err
		}
	}

	var dictW hotfmt.DictWriter
	var postingsBand, skipsBand, positionsBand []byte
	terms := slices.Sorted(maps.Keys(post))
	for _, term := range terms {
		m := post[term]
		ps := make([]hotfmt.Posting, 0, len(m))
		for _, d := range slices.Sorted(maps.Keys(m)) {
			o := m[d]
			var mask uint8
			for f := range 2 {
				if len(o.pos[f]) > 0 {
					mask |= 1 << f
				}
			}
			tf := uint8(min(o.tf, 255))
			ps = append(ps, hotfmt.Posting{Docid: d, TF: tf, Mask: mask, Impact: tf})
		}
		e := hotfmt.DictEntry{DF: uint32(len(ps))}
		if len(ps) <= 4 {
			for _, p := range ps {
				e.Inline = append(e.Inline, hotfmt.InlinePosting{Docid: p.Docid, TF: p.TF, Mask: p.Mask})
			}
		} else {
			enc, l1, err := hotfmt.EncodePostings(ps)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", term, err)
			}
			var cf uint64
			runOffs := make([]uint64, 0, len(ps))
			for _, p := range ps {
				cf += uint64(p.TF)
				runOffs = append(runOffs, uint64(len(positionsBand)))
				o := m[p.Docid]
				var fields []hotfmt.FieldPositions
				for f := range 2 {
					if len(o.pos[f]) > 0 {
						fields = append(fields, hotfmt.FieldPositions{Field: uint8(f), Positions: o.pos[f]})
					}
				}
				positionsBand, err = hotfmt.AppendPositionRun(positionsBand, p.Mask, fields)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", term, err)
				}
			}
			posOffs := make([]uint64, len(l1))
			for bi := range l1 {
				posOffs[bi] = runOffs[bi*hotfmt.PostingsBlockLen]
			}
			e = hotfmt.DictEntry{
				DF:          uint32(len(ps)),
				CF:          cf,
				PostingsOff: uint64(len(postingsBand)),
				PostingsLen: uint32(len(enc)),
				SkipOff:     uint64(len(skipsBand)),
			}
			postingsBand = append(postingsBand, enc...)
			skipsBand, err = hotfmt.EncodeSkips(skipsBand, l1, posOffs)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", term, err)
			}
		}
		if err := dictW.Add([]byte(term), &e); err != nil {
			return nil, fmt.Errorf("%s: %w", term, err)
		}
	}
	dictBand, err := dictW.Seal()
	if err != nil {
		return nil, err
	}
	dvBand, err := hotfmt.EncodeDocValues(dvs)
	if err != nil {
		return nil, err
	}
	docBand, err := docsW.Seal()
	if err != nil {
		return nil, err
	}

	h := &hotfmt.FileHeader{
		Shard: 0, Generation: 1, Kind: hotfmt.KindBase,
		DocCount: docCount, TermCount: uint64(len(terms)),
		TokenizerVer: 1, QuantScale: 1, Writer: 1,
	}
	meta := &hotfmt.Meta{
		LawVer: 1, TokenizerVer: 1, QuantScale: 1, QuantPolicy: 1,
		DocCount: docCount,
		Fields: []hotfmt.MetaField{
			{ID: 0, Name: "body", SumLen: sumLen[0]},
			{ID: 1, Name: "title", SumLen: sumLen[1]},
		},
		Lineage: []uint64{1},
	}
	stats := &hotfmt.FieldStats{
		K1: 1.2, Alpha: 1, Beta: 1,
		Fields: []hotfmt.FieldStat{
			{ID: 0, TotalTokens: sumLen[0], AvgLen: float32(sumLen[0]) / float32(docCount), Weight: 1, B: 0.75},
			{ID: 1, TotalTokens: sumLen[1], AvgLen: float32(sumLen[1]) / float32(docCount), Weight: 2.5, B: 0.6},
		},
	}
	prov := &hotfmt.Provenance{Builder: 1, BuildMS: uint64(time.Now().UnixMilli()), Watermarks: []uint64{1}}
	return hotfmt.EncodeFile(h, meta, stats, map[byte][]byte{
		hotfmt.BandDict:      dictBand,
		hotfmt.BandPostings:  postingsBand,
		hotfmt.BandSkips:     skipsBand,
		hotfmt.BandPositions: positionsBand,
		hotfmt.BandDocvalues: dvBand,
		hotfmt.BandDocband:   docBand,
	}, prov)
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
