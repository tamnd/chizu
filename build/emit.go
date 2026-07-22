package build

import (
	"bufio"
	"bytes"
	"container/heap"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tamnd/chizu/hotfmt"
	"github.com/tamnd/chizu/tokenize"
)

// Emit pass, doc 06 section 7: a k-way merge over the shard pass's
// sorted runs turns records into a sealed .hot. The dictionary and
// docvalues stay in memory (tens of bytes per term and 8 per doc); the
// postings, skips, and positions bands spool to builder NVMe as terms
// complete and then stream into the file, so RAM stays bounded by the
// merge heap plus one term's postings.

// Field scoring constants, doc 05 section 8. Flat quantization this
// milestone: impact is the capped tf and the true bound law arrives
// with scoring at C3b.
const (
	lawVer      = 1
	quantPolicy = 1
	bm25K1      = 1.2
	bodyWeight  = 1.0
	bodyB       = 0.75
	titleWeight = 2.5
	titleB      = 0.6
)

// EmitConfig carries everything the emit pass may not invent for
// itself. B2 needs the output to be a pure function of the runs and
// this struct, so provenance arrives here instead of from the clock.
type EmitConfig struct {
	SpoolDir   string // scratch dir for the band spools
	Shard      uint16
	Generation uint64
	Writer     uint64 // builder node id, lands in the header
	Builder    uint64
	BuildMS    uint64
	Watermarks []uint64
}

// mergeItem is one run's cursor in the k-way heap.
type mergeItem struct {
	r   *RunReader
	rec Rec
}

type mergeHeap []*mergeItem

func (h mergeHeap) Len() int { return len(h) }
func (h mergeHeap) Less(i, j int) bool {
	if c := bytes.Compare(h[i].rec.Term, h[j].rec.Term); c != 0 {
		return c < 0
	}
	return h[i].rec.Docid < h[j].rec.Docid
}
func (h mergeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *mergeHeap) Push(x any)   { *h = append(*h, x.(*mergeItem)) }
func (h *mergeHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return it
}

// spool is one band accumulating on disk.
type spool struct {
	f   *os.File
	bw  *bufio.Writer
	off uint64
}

func newSpool(dir, name string) (*spool, error) {
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	return &spool{f: f, bw: bufio.NewWriterSize(f, 1<<20)}, nil
}

func (s *spool) write(b []byte) error {
	n, err := s.bw.Write(b)
	s.off += uint64(n)
	return err
}

// reader flushes and rewinds the spool for the band stream.
func (s *spool) reader() (io.Reader, error) {
	if err := s.bw.Flush(); err != nil {
		return nil, err
	}
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return bufio.NewReaderSize(s.f, 1<<20), nil
}

func (s *spool) close() {
	_ = s.f.Close()
	_ = os.Remove(s.f.Name())
}

// termState accumulates one term across the merge.
type termState struct {
	term    []byte
	ps      []hotfmt.Posting
	cf      uint64
	posBuf  []byte   // this term's position runs
	runOffs []uint64 // per-posting offset into posBuf
}

func (t *termState) reset(term []byte) {
	t.term = append(t.term[:0], term...)
	t.ps = t.ps[:0]
	t.cf = 0
	t.posBuf = t.posBuf[:0]
	t.runOffs = t.runOffs[:0]
}

// Emit merges the shard pass's runs and writes the sealed .hot to w.
// The caller owns w; a local file is the normal destination, with
// UploadHot moving it to the bucket afterwards.
func Emit(w io.Writer, out *ShardOutput, cfg *EmitConfig) (err error) {
	if len(out.Runs) == 0 {
		return errors.New("build: emit with no runs")
	}

	spools := make(map[string]*spool, 3)
	defer func() {
		for _, s := range spools {
			s.close()
		}
	}()
	for _, name := range []string{"postings", "skips", "positions"} {
		s, serr := newSpool(cfg.SpoolDir, name+".spool")
		if serr != nil {
			return serr
		}
		spools[name] = s
	}
	postings, skips, positions := spools["postings"], spools["skips"], spools["positions"]

	var dictW hotfmt.DictWriter
	var termCount uint64
	var cur termState
	var skipScratch []byte
	var posOffs []uint64

	flushTerm := func() error {
		if len(cur.ps) == 0 {
			return nil
		}
		termCount++
		df := uint32(len(cur.ps))
		e := hotfmt.DictEntry{DF: df}
		if df <= 4 {
			// Inline entry: the term touches no other band, so the
			// buffered position runs are dropped with it (doc 05: no
			// positions for inline terms this milestone).
			for _, p := range cur.ps {
				e.Inline = append(e.Inline, hotfmt.InlinePosting{Docid: p.Docid, TF: p.TF, Mask: p.Mask})
			}
			return dictW.Add(cur.term, &e)
		}
		enc, l1, perr := hotfmt.EncodePostings(cur.ps)
		if perr != nil {
			return fmt.Errorf("%s: %w", cur.term, perr)
		}
		posOffs = posOffs[:0]
		for bi := range l1 {
			posOffs = append(posOffs, positions.off+cur.runOffs[bi*hotfmt.PostingsBlockLen])
		}
		e = hotfmt.DictEntry{
			DF:          df,
			CF:          cur.cf,
			PostingsOff: postings.off,
			PostingsLen: uint32(len(enc)),
			SkipOff:     skips.off,
		}
		if err := postings.write(enc); err != nil {
			return err
		}
		skipScratch, perr = hotfmt.EncodeSkips(skipScratch[:0], l1, posOffs)
		if perr != nil {
			return fmt.Errorf("%s: %w", cur.term, perr)
		}
		if err := skips.write(skipScratch); err != nil {
			return err
		}
		if err := positions.write(cur.posBuf); err != nil {
			return err
		}
		return dictW.Add(cur.term, &e)
	}

	consume := func(rec *Rec) error {
		if !bytes.Equal(rec.Term, cur.term) {
			if err := flushTerm(); err != nil {
				return err
			}
			cur.reset(rec.Term)
		}
		tf := uint8(min(rec.TF, 255))
		cur.cf += uint64(rec.TF)
		cur.runOffs = append(cur.runOffs, uint64(len(cur.posBuf)))
		var fields []hotfmt.FieldPositions
		for f := range NumFields {
			if rec.Mask&(1<<f) != 0 {
				fields = append(fields, hotfmt.FieldPositions{Field: uint8(f), Positions: rec.Pos[f]})
			}
		}
		var perr error
		cur.posBuf, perr = hotfmt.AppendPositionRun(cur.posBuf, rec.Mask, fields)
		if perr != nil {
			return fmt.Errorf("%s doc %d: %w", rec.Term, rec.Docid, perr)
		}
		cur.ps = append(cur.ps, hotfmt.Posting{Docid: rec.Docid, TF: tf, Mask: rec.Mask, Impact: tf})
		return nil
	}

	h := make(mergeHeap, 0, len(out.Runs))
	defer func() {
		for _, it := range h {
			_ = it.r.Close()
		}
	}()
	for _, path := range out.Runs {
		r, oerr := OpenRun(path)
		if oerr != nil {
			return oerr
		}
		it := &mergeItem{r: r}
		if nerr := r.Next(&it.rec); nerr != nil {
			_ = r.Close()
			if nerr == io.EOF {
				continue
			}
			return nerr
		}
		h = append(h, it)
	}
	heap.Init(&h)
	for h.Len() > 0 {
		it := h[0]
		if err := consume(&it.rec); err != nil {
			return err
		}
		switch nerr := it.r.Next(&it.rec); nerr {
		case nil:
			heap.Fix(&h, 0)
		case io.EOF:
			_ = heap.Pop(&h).(*mergeItem).r.Close()
		default:
			return nerr
		}
	}
	if err := flushTerm(); err != nil {
		return err
	}

	dictBand, err := dictW.Seal()
	if err != nil {
		return err
	}
	dvBand, err := hotfmt.EncodeDocValues(out.DocValues)
	if err != nil {
		return err
	}

	header := &hotfmt.FileHeader{
		Shard: cfg.Shard, Generation: cfg.Generation, Kind: hotfmt.KindBase,
		DocCount: out.DocCount, TermCount: termCount,
		TokenizerVer: tokenize.Version, QuantScale: 1, Writer: cfg.Writer,
	}
	meta := &hotfmt.Meta{
		LawVer: lawVer, TokenizerVer: tokenize.Version, QuantScale: 1, QuantPolicy: quantPolicy,
		DocCount: out.DocCount,
		Fields: []hotfmt.MetaField{
			{ID: fieldBody, Name: "body", SumLen: out.SumLen[fieldBody]},
			{ID: fieldTitle, Name: "title", SumLen: out.SumLen[fieldTitle]},
		},
		Lineage: []uint64{cfg.Generation},
	}
	stats := &hotfmt.FieldStats{
		K1: bm25K1, Alpha: 1, Beta: 1,
		Fields: []hotfmt.FieldStat{
			{ID: fieldBody, TotalTokens: out.SumLen[fieldBody], AvgLen: avgLen(out.SumLen[fieldBody], out.DocCount), Weight: bodyWeight, B: bodyB},
			{ID: fieldTitle, TotalTokens: out.SumLen[fieldTitle], AvgLen: avgLen(out.SumLen[fieldTitle], out.DocCount), Weight: titleWeight, B: titleB},
		},
	}

	fw, err := hotfmt.NewFileWriter(w, header, meta, stats)
	if err != nil {
		return err
	}
	if err := fw.WriteBandBytes(hotfmt.BandDict, dictBand); err != nil {
		return err
	}
	if err := fw.WriteBandBytes(hotfmt.BandDocvalues, dvBand); err != nil {
		return err
	}
	if err := fw.WriteBand(hotfmt.BandTombstones, bytes.NewReader(nil)); err != nil {
		return err
	}
	for _, band := range []struct {
		id byte
		s  *spool
	}{
		{hotfmt.BandPostings, postings},
		{hotfmt.BandSkips, skips},
		{hotfmt.BandPositions, positions},
	} {
		r, rerr := band.s.reader()
		if rerr != nil {
			return rerr
		}
		if err := fw.WriteBand(band.id, r); err != nil {
			return err
		}
	}
	if err := fw.WriteBandBytes(hotfmt.BandDocband, out.DocBand); err != nil {
		return err
	}
	return fw.Finish(&hotfmt.Provenance{
		Builder: cfg.Builder, BuildMS: cfg.BuildMS, Watermarks: cfg.Watermarks,
	})
}

func avgLen(sum uint64, docs uint32) float32 {
	if docs == 0 {
		return 0
	}
	return float32(sum) / float32(docs)
}
