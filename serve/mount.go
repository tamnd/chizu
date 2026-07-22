// Package serve is the serving plane: it mounts .hot shard generations
// from local NVMe and executes queries against them. This file owns
// the residency plan of spec 2107 doc 05 section 11 as amended
// 2026-07-23: the small bands (meta, fieldstats, dict, docvalues,
// tombstones) mmap as one contiguous prefix and mlock; the head-term
// L1 skip arrays mlock under a byte budget; everything else is pread
// through pooled aligned buffers, never mmap (doc 05 section 11 read
// discipline).
package serve

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"syscall"

	"github.com/tamnd/chizu/hotfmt"
)

// MlockMode says what happens when mlock fails at mount time.
type MlockMode int

const (
	// MlockTry locks what the rlimit allows and records the shortfall
	// in the residency accounting; the default, and what tests and dev
	// boxes run under.
	MlockTry MlockMode = iota
	// MlockRequired fails the mount on any mlock error; production
	// posture, because H-I7 says no query-path structure relies on
	// page-cache luck.
	MlockRequired
	// MlockOff maps without locking; only for tooling that inspects
	// shards without serving them.
	MlockOff
)

// Options tune a mount. The zero value is a serving default with the
// head-term L1 budget disabled.
type Options struct {
	Mlock MlockMode
	// HeadL1Budget is the byte ceiling for mlocked head-term skip
	// regions (the doc 05 section 11 knob, ≤ 1.5 GB in the M1 table).
	// Zero disables the head walk entirely.
	HeadL1Budget int64
	// HeadL1MinDF is the smallest df worth locking; below it a term's
	// whole skip region preads in one or two pages anyway. The default
	// 4096 (32 L1 blocks) applies when zero.
	HeadL1MinDF uint32
}

const defaultHeadL1MinDF = 4096

// pageSize is the mlock/mmap granularity; band alignment (4 KiB)
// matches it on every deployment target.
var pageSize = int64(os.Getpagesize())

// Residency is the mount's RAM accounting, the per-shard rows the M1
// gate holds against measured RSS.
type Residency struct {
	ResidentPrefix int64 // mapped bytes: header through tombstones band
	Dict           int64
	DocValues      int64
	MetaBand       int64
	FieldStats     int64
	Tombstones     int64
	HeadL1         int64 // mlocked head-term skip region bytes
	HeadL1Terms    int
	WantLocked     int64 // bytes the plan asked to lock
	Locked         int64 // bytes actually locked (rlimit can shorten this under MlockTry)
}

// Mount is one mmap-resident shard generation, query-ready before the
// page cache holds a single postings byte.
type Mount struct {
	Shard      *hotfmt.Shard
	Dict       *hotfmt.Dict
	DocValues  *hotfmt.DocValues
	Tombstones *hotfmt.Tombstones

	f        *os.File
	prefix   []byte // mapping of [0, postings band)
	skips    []byte // mapping of the skips band, present only with a head budget
	skipsOff uint64 // file offset of the skips mapping
	res      Residency
	closed   bool
}

// MountShard opens path per the doc 05 section 11 sequence: preads for
// tail, footer, header, meta, and fieldstats (hotfmt.Open), one mmap
// for the resident prefix, mlock per the plan, and the head-term L1
// walk when a budget says so.
func MountShard(path string, opts Options) (*Mount, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	m, err := mountFile(f, opts)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return m, nil
}

func mountFile(f *os.File, opts Options) (*Mount, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	shard, err := hotfmt.Open(f, st.Size())
	if err != nil {
		return nil, err
	}
	m := &Mount{Shard: shard, f: f}

	postingsOff, _, err := shard.Band(hotfmt.BandPostings)
	if err != nil {
		return nil, err
	}
	m.prefix, err = syscall.Mmap(int(f.Fd()), 0, int(postingsOff), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("serve: mmap resident prefix: %w", err)
	}
	m.res.ResidentPrefix = int64(postingsOff)

	view := func(id byte) ([]byte, error) {
		off, length, err := shard.Band(id)
		if err != nil {
			return nil, err
		}
		return m.prefix[off : off+length], nil
	}
	dictBand, err := view(hotfmt.BandDict)
	if err == nil {
		m.Dict, err = hotfmt.OpenDict(dictBand)
	}
	if err != nil {
		_ = m.unmap()
		return nil, err
	}
	m.res.Dict = int64(len(dictBand))
	dvBand, err := view(hotfmt.BandDocvalues)
	if err == nil {
		m.DocValues, err = hotfmt.OpenDocValues(dvBand, shard.Header.DocCount)
	}
	if err != nil {
		_ = m.unmap()
		return nil, err
	}
	m.res.DocValues = int64(len(dvBand))
	tombBand, err := view(hotfmt.BandTombstones)
	if err != nil {
		_ = m.unmap()
		return nil, err
	}
	if len(tombBand) > 0 {
		if m.Tombstones, err = hotfmt.ParseTombstones(tombBand); err != nil {
			_ = m.unmap()
			return nil, err
		}
	}
	m.res.Tombstones = int64(len(tombBand))
	if _, length, err := shard.Band(hotfmt.BandMeta); err == nil {
		m.res.MetaBand = int64(length)
	}
	if _, length, err := shard.Band(hotfmt.BandFieldstats); err == nil {
		m.res.FieldStats = int64(length)
	}

	if opts.Mlock != MlockOff {
		m.res.WantLocked = m.res.ResidentPrefix
		if err := mlock(m.prefix); err != nil {
			if opts.Mlock == MlockRequired {
				_ = m.unmap()
				return nil, fmt.Errorf("serve: mlock resident prefix: %w", err)
			}
		} else {
			m.res.Locked += m.res.ResidentPrefix
		}
	}

	if opts.HeadL1Budget > 0 {
		if err := m.lockHeadL1(opts); err != nil {
			_ = m.unmap()
			return nil, err
		}
	}
	return m, nil
}

// headTerm is one candidate for the head-term L1 lock.
type headTerm struct {
	df      uint32
	skipOff uint64
	size    int64
}

// lockHeadL1 walks the dictionary, ranks terms by df, and mlocks whole
// skip regions greedily until the budget runs out. The walk touches
// only the already-resident dictionary; regions live in a lazy mapping
// of the skips band that is never read through, only locked, so the
// planner's preads stay the one access path for skip bytes.
func (m *Mount) lockHeadL1(opts Options) error {
	minDF := opts.HeadL1MinDF
	if minDF == 0 {
		minDF = defaultHeadL1MinDF
	}
	var cands []headTerm
	err := m.Dict.Walk(func(_ []byte, e hotfmt.DictEntry) error {
		if e.DF >= minDF && len(e.Inline) == 0 {
			cands = append(cands, headTerm{e.DF, e.SkipOff, int64(hotfmt.SkipRegionSize(e.DF))})
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].df > cands[j].df })

	skipsOff, skipsLen, err := m.Shard.Band(hotfmt.BandSkips)
	if err != nil {
		return err
	}
	// Band alignment is 4 KiB but mmap offsets need the host page size
	// (16 KiB on Apple Silicon), so the mapping starts at the page
	// boundary at or before the band and skew carries the difference.
	mapOff := (int64(skipsOff) / pageSize) * pageSize
	skew := int64(skipsOff) - mapOff
	m.skips, err = syscall.Mmap(int(m.f.Fd()), mapOff, int(skew+int64(skipsLen)), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return fmt.Errorf("serve: mmap skips band: %w", err)
	}
	m.skipsOff = uint64(mapOff)

	spent := int64(0)
	for _, c := range cands {
		if spent+c.size > opts.HeadL1Budget {
			break
		}
		off := skew + int64(c.skipOff)
		lo := (off / pageSize) * pageSize
		hi := ((off + c.size + pageSize - 1) / pageSize) * pageSize
		hi = min(hi, int64(len(m.skips)))
		m.res.WantLocked += hi - lo
		if err := mlock(m.skips[lo:hi]); err != nil {
			if opts.Mlock == MlockRequired {
				return fmt.Errorf("serve: mlock head-term L1: %w", err)
			}
			continue
		}
		m.res.Locked += hi - lo
		m.res.HeadL1 += c.size
		m.res.HeadL1Terms++
		spent += c.size
	}
	return nil
}

// Residency reports the mount's RAM accounting.
func (m *Mount) Residency() Residency { return m.res }

// Closed reports whether Close has run; the registry uses it to prove
// drained generations really unmapped.
func (m *Mount) Closed() bool { return m.closed }

// Close unmaps everything and closes the file. The caller (normally
// the registry) must guarantee no query still reads the mappings.
func (m *Mount) Close() error {
	if m.closed {
		return errors.New("serve: mount closed twice")
	}
	m.closed = true
	err := m.unmap()
	if cerr := m.f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (m *Mount) unmap() error {
	var err error
	if m.prefix != nil {
		err = syscall.Munmap(m.prefix)
		m.prefix = nil
	}
	if m.skips != nil {
		if uerr := syscall.Munmap(m.skips); err == nil {
			err = uerr
		}
		m.skips = nil
	}
	return err
}

// mlock wraps the syscall for a zero-length-safe call.
func mlock(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return syscall.Mlock(b)
}
