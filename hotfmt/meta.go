package hotfmt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Meta and fieldstats bands, doc 05 section 2: the frozen build
// parameters and the per-field statistics ride inside the artifact, so
// a fleet mid-rollout serves two generations built under different
// parameters and each scores by its own bands. No config-skew failure
// mode, no coordinated parameter deploy.

func f32bits(f float32) uint32 { return math.Float32bits(f) }
func f32from(b uint32) float32 { return math.Float32frombits(b) }

// maxFieldName bounds a schema field name; the real schema is body,
// title, anchor, url.
const maxFieldName = 64

// MetaField is one entry of the frozen field schema plus its summed
// length, doc 08's per-field corpus statistic.
type MetaField struct {
	ID     byte
	Name   string
	SumLen uint64
}

// Meta is the meta band: build parameters frozen at build time.
type Meta struct {
	LawVer       uint16
	TokenizerVer uint16
	QuantScale   float32
	QuantPolicy  byte
	DocCount     uint32
	Fields       []MetaField
	Lineage      []uint64 // generations, base first, this file last
}

// EncodeMeta lays out the meta band: fixed head, then the field schema,
// then the delta lineage.
func EncodeMeta(m *Meta) ([]byte, error) {
	if len(m.Fields) > 255 || len(m.Lineage) > 255 {
		return nil, errors.New("hotfmt: meta table exceeds u8 count")
	}
	out := binary.LittleEndian.AppendUint16(nil, m.LawVer)
	out = binary.LittleEndian.AppendUint16(out, m.TokenizerVer)
	out = binary.LittleEndian.AppendUint32(out, f32bits(m.QuantScale))
	out = append(out, m.QuantPolicy)
	out = binary.LittleEndian.AppendUint32(out, m.DocCount)
	out = append(out, byte(len(m.Fields)))
	for i, f := range m.Fields {
		if i > 0 && f.ID <= m.Fields[i-1].ID {
			return nil, errors.New("hotfmt: field ids not strictly increasing")
		}
		if len(f.Name) == 0 || len(f.Name) > maxFieldName {
			return nil, fmt.Errorf("hotfmt: field name length %d", len(f.Name))
		}
		out = append(out, f.ID, byte(len(f.Name)))
		out = append(out, f.Name...)
		out = binary.LittleEndian.AppendUint64(out, f.SumLen)
	}
	out = append(out, byte(len(m.Lineage)))
	for _, g := range m.Lineage {
		out = binary.LittleEndian.AppendUint64(out, g)
	}
	return out, nil
}

// ParseMeta decodes the meta band; the bytes must be consumed exactly.
func ParseMeta(data []byte) (*Meta, error) {
	if len(data) < 14 {
		return nil, errors.New("hotfmt: meta band too short")
	}
	m := &Meta{
		LawVer:       binary.LittleEndian.Uint16(data),
		TokenizerVer: binary.LittleEndian.Uint16(data[2:]),
		QuantScale:   f32from(binary.LittleEndian.Uint32(data[4:])),
		QuantPolicy:  data[8],
		DocCount:     binary.LittleEndian.Uint32(data[9:]),
	}
	nfields := int(data[13])
	rest := data[14:]
	m.Fields = make([]MetaField, 0, nfields)
	for i := range nfields {
		if len(rest) < 2 {
			return nil, errors.New("hotfmt: meta field truncated")
		}
		id, nameLen := rest[0], int(rest[1])
		if nameLen == 0 || nameLen > maxFieldName {
			return nil, fmt.Errorf("hotfmt: field name length %d", nameLen)
		}
		if len(rest) < 2+nameLen+8 {
			return nil, errors.New("hotfmt: meta field truncated")
		}
		if i > 0 && id <= m.Fields[i-1].ID {
			return nil, errors.New("hotfmt: field ids not strictly increasing")
		}
		m.Fields = append(m.Fields, MetaField{
			ID:     id,
			Name:   string(rest[2 : 2+nameLen]),
			SumLen: binary.LittleEndian.Uint64(rest[2+nameLen:]),
		})
		rest = rest[2+nameLen+8:]
	}
	if len(rest) < 1 {
		return nil, errors.New("hotfmt: meta lineage truncated")
	}
	nlineage := int(rest[0])
	rest = rest[1:]
	if len(rest) != nlineage*8 {
		return nil, errors.New("hotfmt: meta lineage length mismatch")
	}
	m.Lineage = make([]uint64, 0, nlineage)
	for i := range nlineage {
		m.Lineage = append(m.Lineage, binary.LittleEndian.Uint64(rest[i*8:]))
	}
	return m, nil
}

// FieldStat is one field's statistics plus the BM25F parameters used
// at build time (doc 08 section 2): weight and per-field length
// normalization b.
type FieldStat struct {
	ID          byte
	TotalTokens uint64
	AvgLen      float32
	Weight      float32
	B           float32
}

// FieldStats is the fieldstats band: the scoring parameter block the
// server reads instead of agreeing on configuration with the builder.
type FieldStats struct {
	K1     float32
	Alpha  float32 // prior weight on quality
	Beta   float32 // prior weight on spam
	Fields []FieldStat
}

const fieldStatSize = 21

// EncodeFieldStats lays out the fieldstats band: global parameters,
// then one fixed 21-byte record per field.
func EncodeFieldStats(s *FieldStats) ([]byte, error) {
	if len(s.Fields) > 255 {
		return nil, errors.New("hotfmt: fieldstats exceed u8 count")
	}
	out := binary.LittleEndian.AppendUint32(nil, f32bits(s.K1))
	out = binary.LittleEndian.AppendUint32(out, f32bits(s.Alpha))
	out = binary.LittleEndian.AppendUint32(out, f32bits(s.Beta))
	out = append(out, byte(len(s.Fields)))
	for i, f := range s.Fields {
		if i > 0 && f.ID <= s.Fields[i-1].ID {
			return nil, errors.New("hotfmt: fieldstat ids not strictly increasing")
		}
		out = append(out, f.ID)
		out = binary.LittleEndian.AppendUint64(out, f.TotalTokens)
		out = binary.LittleEndian.AppendUint32(out, f32bits(f.AvgLen))
		out = binary.LittleEndian.AppendUint32(out, f32bits(f.Weight))
		out = binary.LittleEndian.AppendUint32(out, f32bits(f.B))
	}
	return out, nil
}

// ParseFieldStats decodes the fieldstats band; exact consumption.
func ParseFieldStats(data []byte) (*FieldStats, error) {
	if len(data) < 13 {
		return nil, errors.New("hotfmt: fieldstats band too short")
	}
	s := &FieldStats{
		K1:    f32from(binary.LittleEndian.Uint32(data)),
		Alpha: f32from(binary.LittleEndian.Uint32(data[4:])),
		Beta:  f32from(binary.LittleEndian.Uint32(data[8:])),
	}
	nfields := int(data[12])
	rest := data[13:]
	if len(rest) != nfields*fieldStatSize {
		return nil, errors.New("hotfmt: fieldstats length mismatch")
	}
	s.Fields = make([]FieldStat, 0, nfields)
	for i := range nfields {
		r := rest[i*fieldStatSize:]
		if i > 0 && r[0] <= s.Fields[i-1].ID {
			return nil, errors.New("hotfmt: fieldstat ids not strictly increasing")
		}
		s.Fields = append(s.Fields, FieldStat{
			ID:          r[0],
			TotalTokens: binary.LittleEndian.Uint64(r[1:]),
			AvgLen:      f32from(binary.LittleEndian.Uint32(r[9:])),
			Weight:      f32from(binary.LittleEndian.Uint32(r[13:])),
			B:           f32from(binary.LittleEndian.Uint32(r[17:])),
		})
	}
	return s, nil
}
