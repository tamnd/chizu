package main

// The FST size reference for PRED-CHIZU-C1A-DICT is a minimal acyclic
// DFA (DAFSA) over the same vocabulary, built with the Daciuk
// incremental algorithm for sorted input. A real term-dictionary FST
// (Lucene's) carries per-term outputs on top of this automaton, so the
// DAFSA bytes are a floor: front coding within 1.3x of the floor is
// within 1.3x of any real FST on the vocabulary.
//
// Serialized size uses a Lucene-like arc encoding: label u8 + flags u8
// (last-arc, target-final, target-has-no-arcs), plus a vbyte target
// address when the target has arcs. Children serialize before parents
// so every referenced address is known and small.

type fstState struct {
	arcs  []fstArc
	final bool
	id    int // registry id, stable once minimized
	addr  int // serialized address, set during sizing
	sized bool
}

type fstArc struct {
	label byte
	to    *fstState
}

type dafsa struct {
	root     *fstState
	register map[string]*fstState
	path     []*fstState
	prev     []byte
}

func newDafsa() *dafsa {
	d := &dafsa{root: &fstState{}, register: make(map[string]*fstState)}
	d.path = append(d.path, d.root)
	return d
}

// add inserts one word; words must arrive in strictly increasing byte
// order.
func (d *dafsa) add(word []byte) {
	p := 0
	for p < len(word) && p < len(d.prev) && word[p] == d.prev[p] {
		p++
	}
	d.minimizePath(p)
	st := d.path[p]
	for _, c := range word[p:] {
		ns := &fstState{}
		st.arcs = append(st.arcs, fstArc{label: c, to: ns})
		d.path = append(d.path, ns)
		st = ns
	}
	st.final = true
	d.prev = append(d.prev[:0], word...)
}

// finish minimizes the remaining path and returns the root.
func (d *dafsa) finish() *fstState {
	d.minimizePath(0)
	return d.root
}

func (d *dafsa) minimizePath(downTo int) {
	for i := len(d.path) - 1; i > downTo; i-- {
		child := d.path[i]
		parent := d.path[i-1]
		parent.arcs[len(parent.arcs)-1].to = d.lookupOrRegister(child)
	}
	d.path = d.path[:downTo+1]
}

// lookupOrRegister replaces a state with its registered equivalent, or
// registers it. A state's children are always registered before it, so
// the signature can use stable child ids.
func (d *dafsa) lookupOrRegister(s *fstState) *fstState {
	key := make([]byte, 0, 1+len(s.arcs)*6)
	if s.final {
		key = append(key, 1)
	} else {
		key = append(key, 0)
	}
	for _, a := range s.arcs {
		id := a.to.id
		key = append(key, a.label, byte(id), byte(id>>8), byte(id>>16), byte(id>>24))
	}
	if r, ok := d.register[string(key)]; ok {
		return r
	}
	s.id = len(d.register) + 1
	d.register[string(key)] = s
	return s
}

// contains walks the automaton; the test uses it to pin membership.
func (d *dafsa) contains(word []byte) bool {
	st := d.root
outer:
	for _, c := range word {
		for _, a := range st.arcs {
			if a.label == c {
				st = a.to
				continue outer
			}
		}
		return false
	}
	return st.final
}

func uvarintLen(v int) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

// sizeDafsa serializes bottom-up and returns state, arc, and byte
// counts. A state's bytes are its arc list; final and no-arc target
// facts ride in arc flags for free, matching how compact FST encodings
// avoid per-state overhead.
func sizeDafsa(root *fstState) (states, arcs, bytes int) {
	total := 0
	var visit func(s *fstState)
	visit = func(s *fstState) {
		if s.sized {
			return
		}
		s.sized = true
		for _, a := range s.arcs {
			visit(a.to)
		}
		size := 0
		for _, a := range s.arcs {
			size += 2 // label + flags
			if len(a.to.arcs) > 0 {
				size += uvarintLen(a.to.addr + 1)
			}
		}
		s.addr = total
		total += size
		states++
		arcs += len(s.arcs)
	}
	visit(root)
	return states, arcs, total
}
