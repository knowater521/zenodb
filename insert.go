package zenodb

import (
	"fmt"
	"hash"
	"reflect"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/getlantern/bytemap"
	"github.com/getlantern/errors"
	"github.com/getlantern/wal"
	"github.com/getlantern/zenodb/encoding"
)

func (db *DB) Insert(stream string, ts time.Time, dims map[string]interface{}, vals map[string]interface{}) error {
	return db.InsertRaw(stream, ts, bytemap.New(dims), bytemap.New(vals))
}

func (db *DB) InsertRaw(stream string, ts time.Time, dims bytemap.ByteMap, vals bytemap.ByteMap) error {
	if db.opts.Follow != nil {
		return errors.New("Declining to insert data directly to follower")
	}

	stream = strings.TrimSpace(strings.ToLower(stream))
	db.tablesMutex.Lock()
	w := db.streams[stream]
	db.tablesMutex.Unlock()
	if w == nil {
		return fmt.Errorf("No wal found for stream %v", stream)
	}

	if len(db.opts.WhitelistedDimensions) > 0 {
		if db.log.IsTraceEnabled() {
			db.log.Tracef("Whitelist Dims Original dims: %v", dims.AsMap())
			db.log.Tracef("Whitelist Dims Slicing dims with whitelist: %v", db.opts.WhitelistedDimensions)
		}
		dims = dims.Slice(db.opts.WhitelistedDimensions...)
		if db.log.IsTraceEnabled() {
			db.log.Tracef("Whitelist Dims Sliced dims: %v", dims.AsMap())
		}
	}

	tsd := make([]byte, encoding.Width64bits)
	encoding.EncodeTime(tsd, ts)
	dimsLen := make([]byte, encoding.Width32bits)
	encoding.WriteInt32(dimsLen, len(dims))
	valsLen := make([]byte, encoding.Width32bits)
	encoding.WriteInt32(valsLen, len(vals))
	if db.log.IsTraceEnabled() {
		db.log.Tracef("Writing to wal with dims length %d: %v", len(dims), bytemap.ByteMap(dims).AsMap())
	}
	err := w.Write(tsd, dimsLen, dims, valsLen, vals)
	if err != nil {
		db.log.Error(err)
	}
	return err
}

type walRead struct {
	data   []byte
	offset wal.Offset
	source int
}

func (t *table) processWALInserts() {
	in := make(chan *walRead)
	t.db.Go(func(stop <-chan interface{}) {
		t.processInserts(in, stop)
	})

	for {
		data, err := t.wal.Read()
		if err != nil {
			t.db.Panic(fmt.Errorf("Unable to read from WAL: %v", err))
		}
		in <- &walRead{data, t.wal.Offset(), 0}
	}
}

func (t *table) processInserts(in chan *walRead, stop <-chan interface{}) {
	isFollower := t.db.opts.Follow != nil
	start := time.Now()
	inserted := 0
	skipped := 0
	bytesRead := 0

	h := partitionHash()
loop:
	for {
		select {
		case <-stop:
			return
		case read := <-in:
			if read.data == nil {
				// Ignore empty data
				continue loop
			}
			bytesRead += len(read.data)
			if t.insert(read.data, isFollower, h, read.offset, read.source) {
				inserted++
			} else {
				// Did not insert (probably due to WHERE clause)
				t.skip(read.offset, read.source)
				skipped++
			}
			t.db.walBuffers.Put(read.data)
			delta := time.Now().Sub(start)
			if delta > 1*time.Minute {
				t.log.Debugf("Read %v at %v per second", humanize.Bytes(uint64(bytesRead)), humanize.Bytes(uint64(float64(bytesRead)/delta.Seconds())))
				t.log.Debugf("Inserted %v points at %v per second", humanize.Comma(int64(inserted)), humanize.Commaf(float64(inserted)/delta.Seconds()))
				t.log.Debugf("Skipped %v points at %v per second", humanize.Comma(int64(skipped)), humanize.Commaf(float64(skipped)/delta.Seconds()))
				inserted = 0
				skipped = 0
				bytesRead = 0
				start = time.Now()
			}
		}
	}
}

func (t *table) insert(data []byte, isFollower bool, h hash.Hash32, offset wal.Offset, source int) bool {
	defer func() {
		p := recover()
		if p != nil {
			t.log.Errorf("Panic in inserting: %v", p)
		}
	}()

	tsd, remain := encoding.Read(data, encoding.Width64bits)
	ts := encoding.TimeFromBytes(tsd)
	if ts.Before(t.truncateBefore()) {
		// Ignore old data
		return false
	}
	dimsLen, remain := encoding.ReadInt32(remain)
	dims, remain := encoding.Read(remain, dimsLen)
	if isFollower && !t.db.inPartition(h, dims, t.PartitionBy, t.db.opts.Partition) {
		// data not relevant to follower on this table
		if t.log.IsTraceEnabled() {
			t.log.Tracef("Not in partition: %v", bytemap.ByteMap(dims).AsMap())
		}
		return false
	}

	valsLen, remain := encoding.ReadInt32(remain)
	vals, _ := encoding.Read(remain, valsLen)
	// Split the dims and vals so that holding on to one doesn't force holding on
	// to the other. Also, we need copies for both because the WAL read buffer
	// will change on next call to wal.Read().
	dimsBM := make(bytemap.ByteMap, len(dims))
	valsBM := make(bytemap.ByteMap, len(vals))
	copy(dimsBM, dims)
	copy(valsBM, vals)
	if t.log.IsTraceEnabled() {
		t.log.Tracef("Length of data: %d", len(data))
		if len(vals) > 0 {
			t.log.Tracef("Length of vals: %d", len(valsBM))
			t.log.Tracef("Vals are %v", valsBM.AsMap())
		}
		if len(dims) > 0 {
			t.log.Tracef("Length of dims: %d", len(dimsBM))
			t.log.Tracef("Dims are %v", dimsBM.AsMap())
		}
	}
	return t.doInsert(ts, dimsBM, valsBM, offset, source)
}

// Skip informs the table of a new offset so that we can store it
func (t *table) skip(offset wal.Offset, source int) {
	t.rowStore.insert(&insert{nil, nil, nil, offset, source})
}

func (t *table) doInsert(ts time.Time, dims bytemap.ByteMap, vals bytemap.ByteMap, offset wal.Offset, source int) bool {
	where := t.getWhere()

	if where != nil {
		ok := where.Eval(dims)
		if !ok.(bool) {
			if t.log.IsTraceEnabled() {
				t.log.Tracef("Filtering out inbound point at %v due to %v: %v", ts, where, dims.AsMap())
			}
			t.statsMutex.Lock()
			t.stats.FilteredPoints++
			t.statsMutex.Unlock()
			return false
		}
	}
	t.db.clock.Advance(ts)

	if t.log.IsTraceEnabled() {
		t.log.Tracef("Including inbound point at %v: %v", ts, dims.AsMap())
	}

	var key bytemap.ByteMap
	if len(t.GroupBy) == 0 {
		key = dims
	} else {
		// Reslice dimensions
		names := make([]string, 0, len(t.GroupBy))
		values := make([]interface{}, 0, len(t.GroupBy))
		for _, groupBy := range t.GroupBy {
			val := groupBy.Expr.Eval(dims)
			if val != nil {
				names = append(names, groupBy.Name)
				values = append(values, val)
			}
		}
		key = bytemap.FromSortedKeysAndValues(names, values)
	}

	// Do separate inserts rows for array values if necessary
	var additionalVals []bytemap.ByteMap
	hasMainValue := false
	mainVals := bytemap.Build(func(_include func(string, interface{})) {
		include := func(key string, val float64) {
			_include(key, val)
			hasMainValue = true
		}
		vals.IterateValues(func(key string, value interface{}) bool {
			switch v := value.(type) {
			case float64:
				include(key, v)
			case int:
				include(key, float64(v))
			case []float64:
				// include first value with main vals
				include(key, v[0])
				// do separate inserts for additional values
				for i := 1; i < len(v); i++ {
					subVals := bytemap.Build(func(subInclude func(string, interface{})) {
						subInclude(key, v[i])
					}, nil, true)
					additionalVals = append(additionalVals, subVals)
				}
			case []int:
				// include first value with main vals
				include(key, float64(v[0]))
				// do separate inserts for additional values
				for i := 1; i < len(v); i++ {
					subVals := bytemap.Build(func(subInclude func(string, interface{})) {
						subInclude(key, float64(v[i]))
					}, nil, true)
					additionalVals = append(additionalVals, subVals)
				}
			default:
				t.log.Errorf("Key %v contained value '%v' of unsupported type %v, ignoring", key, value, reflect.TypeOf(value))
			}
			return true
		})
	}, nil, true)

	t.db.capMemorySize(true)
	inserted := len(additionalVals)
	if hasMainValue {
		t.rowStore.insert(&insert{key, encoding.NewTSParams(ts, mainVals), dims, offset, source})
		inserted++
	}
	for _, subVals := range additionalVals {
		t.rowStore.insert(&insert{key, encoding.NewTSParams(ts, subVals), dims, offset, source})
	}
	t.statsMutex.Lock()
	t.stats.InsertedPoints += int64(inserted)
	t.statsMutex.Unlock()

	return true
}

func (t *table) recordQueued() {
	t.statsMutex.Lock()
	t.stats.QueuedPoints++
	t.statsMutex.Unlock()
}
