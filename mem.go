// memory-backed carbon store: speaks graphite in, zipper out
package carbonmem

import (
	"math"

	"sort"
	"strings"
	"sync"
)

// Whisper is an in-memory whisper-like store
type Whisper struct {
	sync.Mutex
	t0     int32
	idx    int
	epochs []map[int]uint64

	// TODO(dgryski): move this to armon/go-radix to speed up prefix matching
	known map[int]int // metric -> #epochs it appears in

	l *lookup
}

func NewWhisper(t0 int32, cap int) *Whisper {

	epochs := make([]map[int]uint64, cap)
	epochs[0] = make(map[int]uint64)

	return &Whisper{
		t0:     t0,
		epochs: epochs,
		known:  make(map[int]int),
		l:      newLookup(),
	}
}

func (w *Whisper) Set(t int32, metric string, val uint64) {

	w.Lock()
	defer w.Unlock()

	// based on github.com/dgryski/go-timewindow

	if t == w.t0 {

		id := w.l.FindOrAdd(metric)

		m := w.epochs[w.idx]

		// have we seen this metric this epoch?
		_, ok := m[id]
		if !ok {
			// one more occurrence of this metric
			w.known[id]++
		}

		m[id] = val
		return
	}

	if t > w.t0 {
		// advance the buffer, decrementing counts for all entries in the
		// maps we pass by

		for w.t0 < t {
			w.t0++
			w.idx++
			if w.idx >= len(w.epochs) {
				w.idx = 0
			}

			m := w.epochs[w.idx]
			if m != nil {
				for id, _ := range m {
					w.known[id]--
					if w.known[id] == 0 {
						delete(w.known, id)
					}
				}
				w.epochs[w.idx] = nil
			}
		}

		id := w.l.FindOrAdd(metric)

		w.known[id]++
		w.epochs[w.idx] = map[int]uint64{id: val}
		return
	}

	// less common -- update the past
	back := int(w.t0 - t)

	if back >= len(w.epochs) {
		// too far in the past, ignore
		return
	}

	idx := w.idx - back

	if idx < 0 {
		// need to wrap around
		idx += len(w.epochs)
	}

	m := w.epochs[idx]
	if m == nil {
		m = make(map[int]uint64)
		w.epochs[idx] = m
	}

	id := w.l.FindOrAdd(metric)

	_, ok := m[id]
	if !ok {
		w.known[id]++
	}
	m[id] = val
}

type Fetched struct {
	From   int32
	Until  int32
	Step   int32
	Values []float64
}

func (w *Whisper) Fetch(metric string, from int32, until int32) *Fetched {

	w.Lock()
	defer w.Unlock()

	if from > w.t0 {
		return nil
	}

	id, ok := w.l.Find(metric)
	if !ok {
		// unknown metric
		return nil
	}

	if _, ok := w.known[id]; !ok {
		return nil
	}

	if until < from {
		return nil
	}

	if min := w.t0 - int32(len(w.epochs)) + 1; from < min {
		from = min
	}

	idx := w.idx - int(w.t0-from)
	if idx < 0 {
		idx += len(w.epochs)
	}

	points := until - from + 1 // inclusive of 'until'
	r := &Fetched{
		From:   from,
		Until:  until,
		Step:   1,
		Values: make([]float64, points),
	}

	for p, t := 0, idx; p < int(points); p, t = p+1, t+1 {
		if t >= len(w.epochs) {
			t = 0
		}

		m := w.epochs[t]
		if v, ok := m[id]; ok {
			r.Values[p] = float64(v)
		} else {
			r.Values[p] = math.NaN()
		}
	}

	return r
}

type Glob struct {
	Metric string
	IsLeaf bool
}

type GlobByName []Glob

func (g GlobByName) Len() int {
	return len(g)
}

func (g GlobByName) Swap(i, j int) {
	g[i], g[j] = g[j], g[i]
}

func (g GlobByName) Less(i, j int) bool {
	return g[i].Metric < g[j].Metric
}

// TODO(dgryski): only does prefix matching for the moment

func (w *Whisper) Find(query string) []Glob {

	w.Lock()
	defer w.Unlock()

	var response []Glob
	l := len(query)
	for id, _ := range w.known {
		k := w.l.Reverse(id)
		if strings.HasPrefix(k, query) {
			// figure out if we're a leaf or not
			dot := strings.IndexByte(k[l:], '.')
			var leaf bool
			m := k
			if dot == -1 {
				leaf = true
			} else {
				m = k[:dot+l]
			}
			response = appendIfUnique(response, Glob{Metric: m, IsLeaf: leaf})
		}
	}

	sort.Sort(GlobByName(response))

	return response
}

// TODO(dgryski): replace with something faster if needed

func appendIfUnique(response []Glob, g Glob) []Glob {

	for i := range response {
		if response[i].Metric == g.Metric {
			return response
		}
	}

	return append(response, g)
}

type lookup struct {
	keys    map[string]int
	revKeys map[int]string
	numKeys int
}

func newLookup() *lookup {
	return &lookup{
		keys:    make(map[string]int),
		revKeys: make(map[int]string),
	}
}

func (l *lookup) Find(key string) (int, bool) {
	id, ok := l.keys[key]
	return id, ok
}

func (l *lookup) FindOrAdd(key string) int {

	id, ok := l.keys[key]

	if ok {
		return id
	}

	id = l.numKeys
	l.numKeys++

	l.keys[key] = id
	l.revKeys[id] = key

	return id
}

func (l *lookup) Reverse(id int) string {

	key, ok := l.revKeys[id]

	if !ok {
		panic("looked up invalid key")
	}

	return key
}
