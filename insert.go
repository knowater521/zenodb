package zenodb

import (
	"strings"
	"time"

	"github.com/getlantern/bytemap"
)

type Point struct {
	Ts   time.Time              `json:"ts,omitempty"`
	Dims map[string]interface{} `json:"dims,omitempty"`
	Vals map[string]float64     `json:"vals,omitempty"`
}

// Get implements the interface goexpr.Params
func (p *Point) Get(name string) interface{} {
	result := p.Dims[name]
	if result == nil {
		result = ""
	}
	return result
}

type insert struct {
	key  bytemap.ByteMap
	vals tsparams
}

func (db *DB) Insert(stream string, point *Point) error {
	stream = strings.TrimSpace(strings.ToLower(stream))
	db.tablesMutex.Lock()
	s := db.streams[stream]
	db.tablesMutex.Unlock()

	for _, t := range s {
		log.Tracef("Insert into '%v': %v", t.Name, point)
		t.insert(point)
	}
	return nil
}

func (t *table) insert(point *Point) {
	t.whereMutex.RLock()
	where := t.Where
	t.whereMutex.RUnlock()

	if where != nil {
		ok := where.Eval(point)
		if !ok.(bool) {
			t.log.Tracef("Filtering out inbound point: %v", point.Dims)
			t.statsMutex.Lock()
			t.stats.FilteredPoints++
			t.statsMutex.Unlock()
			return
		}
	}
	t.db.clock.Advance(point.Ts)

	if len(t.GroupBy) > 0 {
		// Reslice dimensions
		newDims := make(map[string]interface{}, len(t.GroupBy))
		for _, dim := range t.GroupBy {
			newDims[dim] = point.Dims[dim]
		}
		point = &Point{
			Ts:   point.Ts,
			Dims: newDims,
			Vals: point.Vals,
		}
	}

	key := bytemap.New(point.Dims)
	vals := newTSParams(point.Ts, bytemap.NewFloat(point.Vals))

	if t.db.opts.DiscardOnBackPressure {
		select {
		case t.inserts <- &insert{key, vals}:
			t.recordQueued()
		default:
			t.statsMutex.Lock()
			t.stats.DroppedPoints++
			t.statsMutex.Unlock()
		}
	} else {
		t.inserts <- &insert{key, vals}
		t.recordQueued()
	}
}

func (t *table) processInserts() {
	for insert := range t.inserts {
		t.rowStore.insert(insert)
		t.statsMutex.Lock()
		t.stats.QueuedPoints--
		t.stats.InsertedPoints++
		t.statsMutex.Unlock()
	}
}

func (t *table) recordQueued() {
	t.statsMutex.Lock()
	t.stats.QueuedPoints++
	t.statsMutex.Unlock()
}
