package fixture

import (
	"runtime"
	"sync"

	"github.com/tamnd/chizu/coldfmt"
)

// ToRow drops a fixture page onto the cold format's fifteen columns.
func ToRow(p *Page) coldfmt.PageRow {
	r := coldfmt.PageRow{
		URLFP:   p.URLFP,
		URL:     p.URL,
		FetchMS: p.FetchMS,
		Status:  p.Status,
		SHA256:  p.SHA256,
		Simhash: p.Simhash,
		Lang:    p.Lang,
		CanonFP: p.CanonFP,
		Title:   p.Title,
		Text:    p.Text,
		LawVer:  1,
	}
	if len(p.Outlinks) > 0 {
		r.Outlinks = make([]coldfmt.Outlink, len(p.Outlinks))
		for i, l := range p.Outlinks {
			r.Outlinks[i] = coldfmt.Outlink{Dst: l.DstFP, Anchor: l.Anchor}
		}
	}
	return r
}

// Segments streams the whole corpus into sealed page segments of
// rowsPerSeg pages each and hands each one to emit with its 1-based
// seq. Docid order is fetch order, so every segment satisfies the
// format's fetch-completion ordering. Memory is one segment of rows at
// a time.
func (c *Corpus) Segments(writer uint64, rowsPerSeg int, emit func(seq uint64, seg []byte) error) error {
	if rowsPerSeg <= 0 {
		rowsPerSeg = 1 << 16
	}
	workers := max(runtime.NumCPU()-1, 1)
	var seq uint64
	for start := uint64(0); start < c.N; start += uint64(rowsPerSeg) {
		seq++
		w := &coldfmt.PageSegmentWriter{Partition: 0, Epoch: 1, Seq: seq, Writer: writer}
		end := min(start+uint64(rowsPerSeg), c.N)

		// Pages are pure functions of docid, so a segment's rows fill
		// in parallel; only the segments themselves stay sequential.
		rows := make([]coldfmt.PageRow, end-start)
		var wg sync.WaitGroup
		for wk := range workers {
			wg.Go(func() {
				for i := uint64(wk); i < end-start; i += uint64(workers) {
					p := c.Page(start + i)
					rows[i] = ToRow(&p)
				}
			})
		}
		wg.Wait()
		for i := range rows {
			w.Add(rows[i])
		}
		seg, err := w.Seal()
		if err != nil {
			return err
		}
		if err := emit(seq, seg); err != nil {
			return err
		}
	}
	return nil
}
