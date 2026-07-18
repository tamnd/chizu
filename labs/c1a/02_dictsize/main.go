// Lab dict-size (doc 05 section 14): dictionary bytes per term and
// lookup latency at front-coding blocks 32/64/128 on the fixture
// vocabulary, with a DAFSA as the FST size floor. Gates the block-64
// default and seeds the M1 dictionary line.
//
// Output is TSV; the size and fst arms print byte columns, the lookup
// arm prints rates. Every row starts: label arm block dim.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"time"

	"github.com/tamnd/chizu/hotfmt"
)

func main() {
	label := flag.String("label", "local", "row label, name the host")
	secs := flag.Float64("sec", 1.0, "seconds per lookup config")
	seed := flag.Int64("seed", 2107, "rng seed")
	nterms := flag.Int("terms", 4_000_000, "vocabulary size")
	arms := flag.String("arms", "size,fst,lookup", "comma list")
	flag.Parse()

	want := map[string]bool{}
	for _, a := range splitComma(*arms) {
		want[a] = true
	}
	rng := rand.New(rand.NewSource(*seed))
	terms := fixtureVocab(rng, *nterms)
	entries := makeEntries(rng, *nterms)
	fmt.Fprintf(os.Stderr, "vocab: %d terms\n", len(terms))

	if want["size"] {
		runSize(*label, terms, entries)
	}
	if want["fst"] {
		runFST(*label, terms)
	}
	if want["lookup"] {
		runLookup(*label, rng, *secs, terms, entries)
	}
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

// makeEntries draws the doc 05 entry mix: ~40% of a web vocabulary is
// df<=4 and inlines, the rest carries band offsets.
func makeEntries(rng *rand.Rand, n int) []labEntry {
	entries := make([]labEntry, n)
	var poff, soff uint64
	for i := range entries {
		if rng.Intn(100) < 40 {
			df := 1 + rng.Intn(4)
			inl := make([]labInlinePosting, df)
			d := uint32(rng.Intn(1000))
			for j := range inl {
				d += uint32(1 + rng.Intn(100000))
				inl[j] = labInlinePosting{Docid: d, TF: uint8(1 + rng.Intn(3)), Mask: uint8(1 + rng.Intn(3))}
			}
			entries[i] = labEntry{DF: uint32(df), Inline: inl}
		} else {
			df := uint32(5 + rng.Intn(100000))
			plen := df/4 + 8
			entries[i] = labEntry{
				DF: df, CF: uint64(df) + uint64(rng.Intn(1000)),
				PostingsOff: poff, PostingsLen: plen, SkipOff: soff,
			}
			poff += uint64(plen)
			soff += 64
		}
	}
	return entries
}

func buildLab(terms [][]byte, entries []labEntry, blockSize int) (*labDictWriter, []byte) {
	w := &labDictWriter{blockSize: blockSize}
	for i, t := range terms {
		if err := w.add(t, &entries[i]); err != nil {
			fmt.Fprintln(os.Stderr, "dictsize:", err)
			os.Exit(1)
		}
	}
	band, err := w.seal()
	if err != nil {
		fmt.Fprintln(os.Stderr, "dictsize:", err)
		os.Exit(1)
	}
	return w, band
}

// runSize reports the byte budget per block size: front-coded term
// bytes, the RAM block index, and the fixed entries, plus per-term
// figures for the M1 extrapolation.
func runSize(label string, terms [][]byte, entries []labEntry) {
	fmt.Println("# size: label arm block dim nterms term_bytes index_bytes entry_bytes struct_B_per_term total_B_per_term")
	for _, bs := range []int{32, 64, 128} {
		w, band := buildLab(terms, entries, bs)
		n := float64(len(terms))
		structB := w.termBytes + w.indexBytes()
		fmt.Printf("%s\tsize\t%d\tfc\t%d\t%d\t%d\t%d\t%.2f\t%.2f\n",
			label, bs, len(terms), w.termBytes, w.indexBytes(), w.entryBytes,
			float64(structB)/n, float64(structB+w.entryBytes)/n)
		_ = band
	}
}

// runFST builds the DAFSA floor over the same vocabulary.
func runFST(label string, terms [][]byte) {
	d := newDafsa()
	for _, t := range terms {
		d.add(t)
	}
	root := d.finish()
	states, arcs, bytes := sizeDafsa(root)
	fmt.Println("# fst: label arm block dim nterms states arcs bytes B_per_term")
	fmt.Printf("%s\tfst\t0\tdafsa\t%d\t%d\t%d\t%d\t%.2f\n",
		label, len(terms), states, arcs, bytes, float64(bytes)/float64(len(terms)))
}

func measure(budget float64, fn func()) (int, time.Duration) {
	fn() // warm
	start := time.Now()
	passes := 0
	for time.Since(start).Seconds() < budget {
		fn()
		passes++
	}
	return passes, time.Since(start)
}

const lookupBatch = 4096

// runLookup measures uniform random present-term lookups per second:
// binary search the block index, scan one block, parse its entries,
// production's allocation behavior included. The hotfmt row is the
// production anchor at block 64.
func runLookup(label string, rng *rand.Rand, secs float64, terms [][]byte, entries []labEntry) {
	queries := make([]int, lookupBatch)
	for i := range queries {
		queries[i] = rng.Intn(len(terms))
	}
	fmt.Println("# lookup: label arm block dim lookups M_per_s")
	for _, bs := range []int{32, 64, 128} {
		_, band := buildLab(terms, entries, bs)
		d, err := openLabDict(band, bs)
		if err != nil {
			fmt.Fprintln(os.Stderr, "dictsize:", err)
			os.Exit(1)
		}
		var sink uint32
		passes, dur := measure(secs, func() {
			for _, qi := range queries {
				e, ok, err := d.lookup(terms[qi])
				if err != nil || !ok {
					fmt.Fprintln(os.Stderr, "dictsize: lookup miss at", qi, err)
					os.Exit(1)
				}
				sink += e.DF
			}
		})
		_ = sink
		n := passes * lookupBatch
		fmt.Printf("%s\tlookup\t%d\tfc\t%d\t%.3f\n", label, bs, n, float64(n)/dur.Seconds()/1e6)
	}

	hw := &hotfmt.DictWriter{}
	for i, t := range terms {
		if err := hw.Add(t, hotEntry(&entries[i])); err != nil {
			fmt.Fprintln(os.Stderr, "dictsize:", err)
			os.Exit(1)
		}
	}
	band, err := hw.Seal()
	if err != nil {
		fmt.Fprintln(os.Stderr, "dictsize:", err)
		os.Exit(1)
	}
	hd, err := hotfmt.OpenDict(band)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dictsize:", err)
		os.Exit(1)
	}
	var sink uint32
	passes, dur := measure(secs, func() {
		for _, qi := range queries {
			e, ok, err := hd.Lookup(terms[qi])
			if err != nil || !ok {
				fmt.Fprintln(os.Stderr, "dictsize: hotfmt lookup miss at", qi, err)
				os.Exit(1)
			}
			sink += e.DF
		}
	})
	_ = sink
	n := passes * lookupBatch
	fmt.Printf("%s\tlookup\t64\thotfmt\t%d\t%.3f\n", label, n, float64(n)/dur.Seconds()/1e6)
}

func hotEntry(e *labEntry) *hotfmt.DictEntry {
	he := &hotfmt.DictEntry{
		DF: e.DF, CF: e.CF,
		PostingsOff: e.PostingsOff, PostingsLen: e.PostingsLen, SkipOff: e.SkipOff,
	}
	for _, p := range e.Inline {
		he.Inline = append(he.Inline, hotfmt.InlinePosting{Docid: p.Docid, TF: p.TF, Mask: p.Mask})
	}
	return he
}
