package build

import (
	"bytes"
	"fmt"
	"math/bits"
	"path/filepath"
	"slices"

	"github.com/tamnd/chizu/coldfmt"
	"github.com/tamnd/chizu/hotfmt"
	"github.com/tamnd/chizu/tokenize"
)

// Shard pass, doc 06 section 7: tokenize documents in local-docid order,
// appending (term, docid, tf, fieldmask, positions) records to sort runs
// spilled zstd-framed to builder NVMe; the doc band and docvalues write
// directly in docid order during the same scan, and the snippet source
// rides along (doc 05 section 9). Docid is insertion order this
// milestone; quality-descending assignment arrives at C3a.

// DefaultRunBudget is the doc 06 run size: ~2 GB of records per sorted
// run keeps a 500M-doc shard to one merge pass.
const DefaultRunBudget = 2 << 30

// snippetBudget bounds the stored snippet source per doc (doc 05
// section 9 stores ~1 KiB).
const snippetBudget = 1024

const (
	fieldBody  = 0
	fieldTitle = 1
)

// ShardPass consumes rows in fetch order and spills sorted runs.
type ShardPass struct {
	dir    string
	budget int

	docid   uint32
	recs    []Rec
	recSize int
	runs    []string

	docs    hotfmt.DocBandWriter
	dvs     []hotfmt.DocValue
	sumLen  [NumFields]uint64
	stats   tokenize.Stats
	occ     map[string]*docOcc
	occKeys []string
}

// ShardOutput is what the emit pass consumes: sorted runs on disk plus
// the bands that were finished in stream.
type ShardOutput struct {
	Runs      []string
	DocBand   []byte
	DocValues []hotfmt.DocValue
	SumLen    [NumFields]uint64
	DocCount  uint32
	Stats     tokenize.Stats
}

// docOcc is one term's occurrences within the doc being scanned.
type docOcc struct {
	tf   uint32
	mask uint8
	pos  [NumFields][]uint16
}

// NewShardPass spills runs into dir; budget caps the estimated record
// bytes held before a spill, and 0 means DefaultRunBudget.
func NewShardPass(dir string, budget int) *ShardPass {
	if budget <= 0 {
		budget = DefaultRunBudget
	}
	return &ShardPass{dir: dir, budget: budget, occ: make(map[string]*docOcc)}
}

// AddRow tokenizes one page into the current run and writes its doc
// band record and docvalues entry. Rows must arrive in fetch order;
// docid is the arrival index.
func (p *ShardPass) AddRow(row *coldfmt.PageRow) error {
	docid := p.docid
	if docid == 0xFFFFFFFF {
		return fmt.Errorf("build: shard exceeds %d docs", uint64(0xFFFFFFFF))
	}
	p.docid++

	bl := p.addField(fieldBody, row.Text)
	tl := p.addField(fieldTitle, row.Title)
	p.sumLen[fieldBody] += bl
	p.sumLen[fieldTitle] += tl

	// One record per term, docids strictly increasing across calls, so
	// a by-term sort at spill time yields (term, docid) order.
	p.occKeys = p.occKeys[:0]
	for t := range p.occ {
		p.occKeys = append(p.occKeys, t)
	}
	slices.Sort(p.occKeys)
	for _, t := range p.occKeys {
		o := p.occ[t]
		rec := Rec{Term: []byte(t), Docid: docid, TF: o.tf, Mask: o.mask}
		for f := range NumFields {
			rec.Pos[f] = slices.Clone(o.pos[f])
		}
		p.recs = append(p.recs, rec)
		p.recSize += len(t) + 16 + 2*(len(rec.Pos[0])+len(rec.Pos[1]))
		delete(p.occ, t)
	}

	if err := p.docs.Add(hotfmt.DocRecord{
		URL:     []byte(row.URL),
		Title:   []byte(row.Title),
		Snippet: snippetSource(row.Text),
		URLFP:   row.URLFP,
	}); err != nil {
		return err
	}
	p.dvs = append(p.dvs, hotfmt.DocValue{
		Quality:     128, // flat until the doc 08 priors land (C3)
		Lang:        row.Lang,
		DoclenBody:  logBucket(bl, 255),
		DoclenTitle: logBucket(tl, 15),
	})

	if p.recSize >= p.budget {
		return p.spill()
	}
	return nil
}

// addField tokenizes one field into the per-doc occurrence map and
// returns its token count. Positions are uint16; occurrences past
// 65535 tokens count toward tf and doclen but carry no position.
func (p *ShardPass) addField(field int, text string) uint64 {
	var n uint64
	tokenize.Text(text, &p.stats, func(term []byte, pos uint32) {
		n++
		o := p.occ[string(term)]
		if o == nil {
			o = &docOcc{}
			p.occ[string(term)] = o
		}
		o.tf++
		o.mask |= 1 << field
		if pos <= 0xFFFF {
			o.pos[field] = append(o.pos[field], uint16(pos))
		}
	})
	return n
}

// spill sorts the buffered records by (term, docid) and writes one run.
func (p *ShardPass) spill() error {
	if len(p.recs) == 0 {
		return nil
	}
	slices.SortFunc(p.recs, func(a, b Rec) int {
		if c := bytes.Compare(a.Term, b.Term); c != 0 {
			return c
		}
		return int(a.Docid) - int(b.Docid)
	})
	path := filepath.Join(p.dir, fmt.Sprintf("run-%04d.czrn", len(p.runs)))
	w, err := NewRunWriter(path)
	if err != nil {
		return err
	}
	for i := range p.recs {
		if err := w.Add(&p.recs[i]); err != nil {
			w.Close()
			return err
		}
	}
	if err := w.Close(); err != nil {
		return err
	}
	p.runs = append(p.runs, path)
	p.recs = p.recs[:0]
	p.recSize = 0
	return nil
}

// Finish spills the tail run and hands the emit pass its inputs.
func (p *ShardPass) Finish() (*ShardOutput, error) {
	if err := p.spill(); err != nil {
		return nil, err
	}
	docBand, err := p.docs.Seal()
	if err != nil {
		return nil, err
	}
	return &ShardOutput{
		Runs:      p.runs,
		DocBand:   docBand,
		DocValues: p.dvs,
		SumLen:    p.sumLen,
		DocCount:  p.docid,
		Stats:     p.stats,
	}, nil
}

// logBucket is the docvalues doclen norm (doc 05 section 8): the bit
// length of the token count, capped to the field's budget.
func logBucket(n uint64, limit uint8) uint8 {
	return uint8(min(bits.Len64(n), int(limit)))
}

// snippetSource picks the stored snippet passage (doc 05 section 9):
// sentences scored by distinct-word density with an early-position
// bonus, then the best sentence extended forward to the byte budget.
// Integer scoring only, so builds stay bit-identical across nodes.
func snippetSource(text string) []byte {
	if len(text) <= snippetBudget {
		return []byte(text)
	}
	bounds := sentenceBounds(text)
	best, bestScore := 0, -1
	seen := make(map[string]struct{})
	for i := range len(bounds) - 1 {
		s := text[bounds[i]:bounds[i+1]]
		total, distinct := 0, 0
		for w := range bytes.FieldsSeq([]byte(s)) {
			total++
			if _, ok := seen[string(w)]; !ok {
				seen[string(w)] = struct{}{}
				distinct++
			}
		}
		if total == 0 {
			continue
		}
		score := distinct*256/total + max(0, 64-i*4)
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	start := bounds[best]
	end := bounds[best+1]
	for i := best + 1; i < len(bounds)-1 && bounds[i+1]-start <= snippetBudget; i++ {
		end = bounds[i+1]
	}
	if end-start > snippetBudget {
		end = start + snippetBudget
	}
	return []byte(text[start:end])
}

// sentenceBounds returns byte offsets splitting text into sentences on
// terminal punctuation, with a 200-byte fallback split so punctuation-
// free text still yields passages.
func sentenceBounds(text string) []int {
	bounds := []int{0}
	last := 0
	for i := 0; i < len(text); i++ {
		c := text[i]
		if (c == '.' || c == '!' || c == '?') && i+1 < len(text) && text[i+1] == ' ' {
			bounds = append(bounds, i+2)
			last = i + 2
		} else if i-last >= 200 && c == ' ' {
			bounds = append(bounds, i+1)
			last = i + 1
		}
	}
	if bounds[len(bounds)-1] != len(text) {
		bounds = append(bounds, len(text))
	}
	return bounds
}
