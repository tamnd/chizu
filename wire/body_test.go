package wire

import (
	"bytes"
	"reflect"
	"testing"
)

func fixtureQuery() *Query {
	return &Query{
		BudgetUS:   8_000,
		Tier:       1,
		Algo:       2,
		K:          10,
		Candidates: 64,
		Shards:     []uint16{0, 3, 17},
		Terms: []QueryTerm{
			{Term: []byte("web"), DFHint: 1_000_000, Op: OpShould},
			{Term: []byte("search"), DFHint: 400_000, Op: OpMust},
			{Term: []byte("spam"), DFHint: 90_000, Op: OpNot},
		},
	}
}

func fixtureQResult() *QResult {
	r := &QResult{Degrade: 0b101, Stats: []byte("stats")}
	for i := range 64 {
		r.Entries = append(r.Entries, ResultEntry{
			Docid: uint64(i) << 20,
			Score: uint16(60000 - i*100),
			Flags: byte(i % 3),
		})
	}
	return r
}

func fixtureSnip() *Snip {
	return &Snip{
		BudgetUS: 3_000,
		Terms:    [][]byte{[]byte("web"), []byte("search")},
		Docids:   []uint64{5, 900, 1 << 40},
	}
}

func fixtureSnipResult() *SnipResult {
	return &SnipResult{Records: []SnippetRecord{
		{Docid: 5, URL: []byte("https://a.test/x"), Title: []byte("A"), Snippet: []byte("about web search")},
		{Docid: 900, URL: []byte("https://b.test/y"), Title: nil, Snippet: nil},
	}}
}

func TestBodyRoundTrips(t *testing.T) {
	h := &Hello{Version: 1, NodeID: 12, MaxInflight: 128, MaxFrame: MaxFrame}
	hb, err := AppendHello(nil, h)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ParseHello(hb); err != nil || !reflect.DeepEqual(got, h) {
		t.Fatalf("hello: %v %+v", err, got)
	}

	q := fixtureQuery()
	qb, err := AppendQuery(nil, q)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ParseQuery(qb); err != nil || !reflect.DeepEqual(got, q) {
		t.Fatalf("query: %v %+v", err, got)
	}

	r := fixtureQResult()
	rb, err := AppendQResult(nil, r)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ParseQResult(rb); err != nil || !reflect.DeepEqual(got, r) {
		t.Fatalf("qresult: %v", err)
	}
	empty := &QResult{Stats: []byte{}, Entries: []ResultEntry{}}
	eb, err := AppendQResult(nil, empty)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ParseQResult(eb); err != nil || len(got.Entries) != 0 {
		t.Fatalf("empty qresult: %v %+v", err, got)
	}

	s := fixtureSnip()
	sb, err := AppendSnip(nil, s)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ParseSnip(sb); err != nil || !reflect.DeepEqual(got, s) {
		t.Fatalf("snip: %v %+v", err, got)
	}

	sr := fixtureSnipResult()
	srb, err := AppendSnipResult(nil, sr)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseSnipResult(srb)
	if err != nil || len(got.Records) != 2 {
		t.Fatalf("snipresult: %v", err)
	}
	if !bytes.Equal(got.Records[0].Snippet, sr.Records[0].Snippet) || got.Records[1].Docid != 900 {
		t.Fatal("snipresult drifted")
	}
}

func TestBodyRejects(t *testing.T) {
	if _, err := AppendHello(nil, &Hello{}); err == nil {
		t.Error("zero hello accepted")
	}
	if _, err := ParseHello(make([]byte, 17)); err == nil {
		t.Error("short hello accepted")
	}

	badQueries := []*Query{
		{K: 0, Candidates: 10, Shards: []uint16{1}, Terms: fixtureQuery().Terms},
		{K: 10, Candidates: 9, Shards: []uint16{1}, Terms: fixtureQuery().Terms},
		{K: 1, Candidates: 1, Shards: nil, Terms: fixtureQuery().Terms},
		{K: 1, Candidates: 1, Shards: []uint16{1}, Terms: nil},
		{K: 1, Candidates: 1, Shards: []uint16{1}, Terms: []QueryTerm{{Term: nil}}},
		{K: 1, Candidates: 1, Shards: []uint16{1}, Terms: []QueryTerm{{Term: bytes.Repeat([]byte("t"), maxTermLen+1)}}},
		{K: 1, Candidates: 1, Shards: []uint16{1}, Terms: []QueryTerm{{Term: []byte("t"), Op: 3}}},
	}
	for i, q := range badQueries {
		if _, err := AppendQuery(nil, q); err == nil {
			t.Errorf("bad query %d accepted", i)
		}
	}
	qb, err := AppendQuery(nil, fixtureQuery())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseQuery(qb[:len(qb)-1]); err == nil {
		t.Error("truncated query accepted")
	}
	if _, err := ParseQuery(append(bytes.Clone(qb), 0)); err == nil {
		t.Error("query with trailing bytes accepted")
	}

	rb, err := AppendQResult(nil, fixtureQResult())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseQResult(rb[:len(rb)-1]); err == nil {
		t.Error("truncated qresult accepted")
	}
	if _, err := ParseQResult(append(bytes.Clone(rb), 0)); err == nil {
		t.Error("qresult with trailing bytes accepted")
	}

	if _, err := AppendSnip(nil, &Snip{Terms: [][]byte{[]byte("t")}}); err == nil {
		t.Error("snip with no docids accepted")
	}
	sb, err := AppendSnip(nil, fixtureSnip())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSnip(sb[:len(sb)-1]); err == nil {
		t.Error("truncated snip accepted")
	}

	if _, err := AppendSnipResult(nil, &SnipResult{}); err == nil {
		t.Error("empty snipresult accepted")
	}
	if _, err := AppendSnipResult(nil, &SnipResult{Records: []SnippetRecord{{Docid: 1}}}); err == nil {
		t.Error("snippet record with empty url accepted")
	}
	srb, err := AppendSnipResult(nil, fixtureSnipResult())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSnipResult(append(bytes.Clone(srb), 0)); err == nil {
		t.Error("snipresult with trailing bytes accepted")
	}
}

func FuzzParseHello(f *testing.F) {
	if b, err := AppendHello(nil, &Hello{Version: 1, NodeID: 3, MaxInflight: 8, MaxFrame: 1 << 20}); err == nil {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		h, err := ParseHello(data)
		if err != nil {
			return
		}
		out, err := AppendHello(nil, h)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		if !bytes.Equal(out, data) {
			t.Fatal("accepted hello is not canonical")
		}
	})
}

func FuzzParseQuery(f *testing.F) {
	if b, err := AppendQuery(nil, fixtureQuery()); err == nil {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		q, err := ParseQuery(data)
		if err != nil {
			return
		}
		out, err := AppendQuery(nil, q)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		if !bytes.Equal(out, data) {
			t.Fatal("accepted query is not canonical")
		}
	})
}

func FuzzParseQResult(f *testing.F) {
	if b, err := AppendQResult(nil, fixtureQResult()); err == nil {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := ParseQResult(data)
		if err != nil {
			return
		}
		out, err := AppendQResult(nil, r)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		if !bytes.Equal(out, data) {
			t.Fatal("accepted qresult is not canonical")
		}
	})
}

func FuzzParseSnip(f *testing.F) {
	if b, err := AppendSnip(nil, fixtureSnip()); err == nil {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := ParseSnip(data)
		if err != nil {
			return
		}
		out, err := AppendSnip(nil, s)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		if !bytes.Equal(out, data) {
			t.Fatal("accepted snip is not canonical")
		}
	})
}

func FuzzParseSnipResult(f *testing.F) {
	if b, err := AppendSnipResult(nil, fixtureSnipResult()); err == nil {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := ParseSnipResult(data)
		if err != nil {
			return
		}
		out, err := AppendSnipResult(nil, r)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		if !bytes.Equal(out, data) {
			t.Fatal("accepted snipresult is not canonical")
		}
	})
}
