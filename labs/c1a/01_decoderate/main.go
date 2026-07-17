// Lab decode-rate (doc 05 section 14): pure-Go postings unpack kernels
// on the gate box, versus width, versus a vbyte baseline, block sizes
// 64/128/256, tombstone bit test in and out of the loop. Feeds the K2
// assembly-clause decision and the block-128 flat-part prediction.
//
// Output is TSV: label arm block width_or_tier reps postings_per_rep
// M/s GBin/s GBout/s. GBout is decoded uint32 bytes (the literature's
// convention); GBin is packed input bytes.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"time"

	"github.com/tamnd/chizu/hotfmt"
)

// workTarget is the decoded working set per config, big enough that
// the packed input cannot live in L2 across a pass.
const workTarget = 32 << 20 // decoded uint32s bytes

func main() {
	label := flag.String("label", "local", "row label, name the host")
	secs := flag.Float64("sec", 0.7, "seconds per config")
	seed := flag.Int64("seed", 2107, "rng seed")
	arms := flag.String("arms", "unpack,vbyte,block,hot,tomb", "comma list")
	flag.Parse()

	want := map[string]bool{}
	for _, a := range splitComma(*arms) {
		want[a] = true
	}
	rng := rand.New(rand.NewSource(*seed))
	if want["unpack"] {
		runUnpack(*label, rng, *secs)
	}
	if want["vbyte"] {
		runVbyte(*label, rng, *secs)
	}
	if want["block"] {
		runBlock(*label, rng, *secs)
	}
	if want["hot"] {
		runHot(*label, rng, *secs)
	}
	if want["tomb"] {
		runTomb(*label, rng, *secs)
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

func row(label, arm string, block int, dim string, n int, dur time.Duration, inBytes int) {
	secs := dur.Seconds()
	ms := float64(n) / secs / 1e6
	gin := float64(inBytes) / secs / 1e9
	gout := float64(n) * 4 / secs / 1e9
	fmt.Printf("%s\t%s\t%d\t%s\t%d\t%.1f\t%.3f\t%.3f\n", label, arm, block, dim, n, ms, gin, gout)
}

// measure repeats fn until budget elapses and returns total passes and
// wall time; fn decodes one full working set.
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

// runUnpack sweeps the raw bitpack kernel: width menu x block sizes.
func runUnpack(label string, rng *rand.Rand, secs float64) {
	for _, block := range []int{64, 128, 256} {
		nblocks := workTarget / 4 / block
		vals := make([]uint32, block)
		for _, w := range widthMenu {
			limit := uint32(uint64(1)<<w - 1)
			src := make([]uint32, block)
			packed := make([]byte, 0, nblocks*packedLen(block, w))
			for range nblocks {
				for i := range src {
					src[i] = uint32(rng.Intn(int(limit) + 1))
				}
				packed = appendPacked(packed, src, w)
			}
			bl := packedLen(block, w)
			var sink uint32
			passes, dur := measure(secs, func() {
				pos := 0
				for range nblocks {
					unpack(packed[pos:pos+bl], vals, w)
					pos += bl
					sink += vals[block-1]
				}
			})
			_ = sink
			row(label, "unpack", block, fmt.Sprintf("w%d", w), passes*nblocks*block, dur, passes*len(packed))
		}
	}
}

// runVbyte decodes the same value distributions varint-encoded, the
// baseline the FOR menu must beat.
func runVbyte(label string, rng *rand.Rand, secs float64) {
	const block = 128
	nblocks := workTarget / 4 / block
	vals := make([]uint32, block)
	for _, w := range widthMenu {
		limit := uint32(uint64(1)<<w - 1)
		src := make([]uint32, block)
		var packed []byte
		offs := make([]int, 0, nblocks+1)
		for range nblocks {
			offs = append(offs, len(packed))
			for i := range src {
				src[i] = uint32(rng.Intn(int(limit) + 1))
			}
			packed = appendVbyte(packed, src)
		}
		offs = append(offs, len(packed))
		var sink uint32
		passes, dur := measure(secs, func() {
			for b := range nblocks {
				unpackVbyte(packed[offs[b]:offs[b+1]], vals)
				sink += vals[block-1]
			}
		})
		_ = sink
		row(label, "vbyte", block, fmt.Sprintf("w%d", w), passes*nblocks*block, dur, passes*len(packed))
	}
}

// tiers are the gap regimes a real traversal sees: dense postings of a
// stop-word-class term, the mid-df bulk, and sparse long-tail terms.
var tiers = []struct {
	name    string
	w       uint8
	tailPct int
}{
	{"dense", 3, 2},
	{"mid", 7, 3},
	{"sparse", 16, 2},
}

// docidCap bounds lab docids at the production shard size (doc 05:
// 500M docs per shard). When a stream would cross it, the list
// restarts at prev -1 the way a new term's postings do, so gap
// distributions stay honest instead of wandering the full u32 space.
const docidCap = 1 << 29

type labList struct {
	data  []byte
	offs  []int   // per-block byte offset
	ns    []int   // per-block entry count
	prevs []int64 // per-block prev docid (-1 at list starts), as skips supply it
}

func buildLabList(rng *rand.Rand, block int, w uint8, tailPct, nblocks int) (labList, error) {
	var l labList
	prev := int64(-1)
	for range nblocks {
		gaps := gapStream(rng, block, w, tailPct)
		tfs := tfStream(rng, block)
		span := int64(0)
		for _, g := range gaps {
			span += int64(g) + 1
		}
		if prev+span >= docidCap {
			prev = -1
		}
		l.offs = append(l.offs, len(l.data))
		l.ns = append(l.ns, block)
		l.prevs = append(l.prevs, prev)
		var err error
		l.data, err = encodeLabBlock(l.data, gaps, tfs, prev)
		if err != nil {
			return labList{}, err
		}
		prev += span
	}
	return l, nil
}

// runBlock measures the full production-shape block decode (header,
// gap unpack, patches, prefix resolve, tf unpack) at 64/128/256.
func runBlock(label string, rng *rand.Rand, secs float64) {
	for _, block := range []int{64, 128, 256} {
		nblocks := workTarget / 4 / block
		docids := make([]uint32, block)
		tfs := make([]uint8, block)
		masks := make([]uint8, block)
		for _, tier := range tiers {
			l, err := buildLabList(rng, block, tier.w, tier.tailPct, nblocks)
			if err != nil {
				fmt.Fprintln(os.Stderr, "decoderate:", err)
				os.Exit(1)
			}
			var sink uint32
			passes, dur := measure(secs, func() {
				for b := range nblocks {
					if _, err := decodeLabBlock(l.data[l.offs[b]:], l.ns[b], l.prevs[b], docids, tfs, masks); err != nil {
						fmt.Fprintln(os.Stderr, "decoderate:", err)
						os.Exit(1)
					}
					sink += docids[block-1]
				}
			})
			_ = sink
			row(label, "block", block, tier.name, passes*nblocks*block, dur, passes*len(l.data))
		}
	}
}

// hotLabList is one term's postings: real hotfmt bytes so the
// production decoder runs on its own encoding, the anchor row the lab
// copies are checked against.
type hotLabList struct {
	enc []byte
	l1  []hotfmt.SkipL1
	ps  []hotfmt.Posting
}

// hotLists cuts the gap stream into per-term lists whenever the docid
// would cross the shard cap, exactly the list restart buildLabList
// applies, then trims each list to full FOR blocks (no vbyte tail).
func hotLists(rng *rand.Rand, w uint8, tailPct, count int) []hotLabList {
	const block = hotfmt.PostingsBlockLen
	gaps := gapStream(rng, count, w, tailPct)
	tfs := tfStream(rng, count)
	var lists []hotLabList
	var ps []hotfmt.Posting
	flush := func() {
		n := len(ps) - len(ps)%block
		if n == 0 {
			ps = nil
			return
		}
		enc, l1, err := hotfmt.EncodePostings(ps[:n])
		if err != nil {
			fmt.Fprintln(os.Stderr, "decoderate:", err)
			os.Exit(1)
		}
		lists = append(lists, hotLabList{enc: enc, l1: l1, ps: ps[:n]})
		ps = nil
	}
	d := int64(-1)
	for i := range gaps {
		nd := d + int64(gaps[i]) + 1
		if nd >= docidCap {
			flush()
			nd = int64(gaps[i])
		}
		d = nd
		ps = append(ps, hotfmt.Posting{Docid: uint32(d), TF: tfs[i], Mask: 1, Impact: labImpact})
	}
	flush()
	return lists
}

// runHot is the production anchor: hotfmt.DecodePostingsBlock on
// hotfmt.EncodePostings bytes, block 128 fixed, per tier.
func runHot(label string, rng *rand.Rand, secs float64) {
	const block = hotfmt.PostingsBlockLen
	count := workTarget / 4
	var docids [block]uint32
	var tfs, masks [block]uint8
	for _, tier := range tiers {
		lists := hotLists(rng, tier.w, tier.tailPct, count)
		total, inBytes := 0, 0
		for _, l := range lists {
			total += len(l.l1) * block
			inBytes += len(l.enc)
		}
		var sink uint32
		passes, dur := measure(secs, func() {
			for _, l := range lists {
				prev := int64(-1)
				for bi := range l.l1 {
					b, _, err := hotfmt.DecodePostingsBlock(l.enc[l.l1[bi].Off:], prev, docids[:], tfs[:], masks[:])
					if err != nil {
						fmt.Fprintln(os.Stderr, "decoderate:", err)
						os.Exit(1)
					}
					prev = int64(b.LastDocid)
					sink += docids[block-1]
				}
			}
		})
		_ = sink
		row(label, "hot", block, tier.name, passes*total, dur, passes*inBytes)
	}
}

// runTomb runs the hot decode with the exclusion bit test off, fused
// into the per-posting loop, and as a separate pass, 10% deleted.
func runTomb(label string, rng *rand.Rand, secs float64) {
	const block = hotfmt.PostingsBlockLen
	count := workTarget / 4
	tier := tiers[1] // mid
	lists := hotLists(rng, tier.w, tier.tailPct, count)
	// The exclusion bitset covers the whole shard docid space, the
	// resident shape doc 05 section 10 mandates, so the test pays the
	// real cache-miss cost of probing a shard-sized set.
	set := make(tombSet, docidCap/64)
	delRng := rand.New(rand.NewSource(rng.Int63()))
	total, inBytes := 0, 0
	for _, l := range lists {
		total += len(l.l1) * block
		inBytes += len(l.enc)
		for _, p := range l.ps {
			if delRng.Intn(10) == 0 {
				set[p.Docid>>6] |= 1 << (p.Docid & 63)
			}
		}
	}
	var docids [block]uint32
	var tfs, masks [block]uint8

	for _, mode := range []string{"off", "inloop", "outloop"} {
		var live uint64
		passes, dur := measure(secs, func() {
			for _, l := range lists {
				prev := int64(-1)
				for bi := range l.l1 {
					b, _, err := hotfmt.DecodePostingsBlock(l.enc[l.l1[bi].Off:], prev, docids[:], tfs[:], masks[:])
					if err != nil {
						fmt.Fprintln(os.Stderr, "decoderate:", err)
						os.Exit(1)
					}
					prev = int64(b.LastDocid)
					n := int(b.NEntries)
					switch mode {
					case "off":
						live += uint64(n)
					case "inloop":
						for i := range n {
							if !set.test(docids[i]) {
								live++
							}
						}
					case "outloop":
						// Same work as a second pass over the block's
						// decoded docids after scoring-independent decode.
						for i := range n {
							_ = docids[i]
						}
						for i := range n {
							if !set.test(docids[i]) {
								live++
							}
						}
					}
				}
			}
		})
		_ = live
		row(label, "tomb", block, mode, passes*total, dur, passes*inBytes)
	}
}
