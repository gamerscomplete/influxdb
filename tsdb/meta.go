package tsdb

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/influxdata/influxdb/influxql"
	"github.com/influxdata/influxdb/models"
	"github.com/influxdata/influxdb/pkg/escape"
	internal "github.com/influxdata/influxdb/tsdb/internal"

	"github.com/gogo/protobuf/proto"
)

//go:generate protoc --gogo_out=. internal/meta.proto

const (
	statDatabaseSeries              = "numSeries"              // number of series in this database
	statDatabaseMeasurements        = "numMeasurements"        // number of measurements in this database
	statDatabaseSeriesDropped       = "numSeriesDropped"       // number of series dropped from database
	statDatabaseMeasurementsDropped = "numMeasurementsDropped" // number of measurements dropped from database
)

// DatabaseIndex is the in memory index of a collection of measurements, time series, and their tags.
// Exported functions are goroutine safe while un-exported functions assume the caller will use the appropriate locks.
type DatabaseIndex struct {
	// in memory metadata index, built on load and updated when new series come in
	mu           sync.RWMutex
	measurements map[string]*Measurement // measurement name to object and index
	series       map[string]*Series      // map series key to the Series object
	lastID       uint64                  // last used series ID. They're in memory only for this shard

	name string // name of the database represented by this index

	stats       *IndexStatistics
	defaultTags models.StatisticTags
}

// NewDatabaseIndex returns a new initialized DatabaseIndex.
func NewDatabaseIndex(name string) *DatabaseIndex {
	return &DatabaseIndex{
		measurements: make(map[string]*Measurement),
		series:       make(map[string]*Series),
		name:         name,
		stats:        &IndexStatistics{},
		defaultTags:  models.StatisticTags{"database": name},
	}
}

// IndexStatistics maintains statistics for the index.
type IndexStatistics struct {
	NumSeries              int64
	NumMeasurements        int64
	NumSeriesDropped       int64
	NumMeasurementsDropped int64
}

// Statistics returns statistics for periodic monitoring.
func (d *DatabaseIndex) Statistics(tags map[string]string) []models.Statistic {
	return []models.Statistic{{
		Name: "database",
		Tags: d.defaultTags.Merge(tags),
		Values: map[string]interface{}{
			statDatabaseSeries:              atomic.LoadInt64(&d.stats.NumSeries),
			statDatabaseMeasurements:        atomic.LoadInt64(&d.stats.NumMeasurements),
			statDatabaseSeriesDropped:       atomic.LoadInt64(&d.stats.NumSeriesDropped),
			statDatabaseMeasurementsDropped: atomic.LoadInt64(&d.stats.NumMeasurementsDropped),
		},
	}}
}

// Series returns a series by key.
func (d *DatabaseIndex) Series(key string) *Series {
	d.mu.RLock()
	s := d.series[key]
	d.mu.RUnlock()
	return s
}

// SeriesBytes returns a series by key.
func (d *DatabaseIndex) SeriesBytes(key []byte) *Series {
	d.mu.RLock()
	s := d.series[string(key)]
	d.mu.RUnlock()
	return s
}

// SeriesKeys returns a sorted slice of strings indicating all the series keys in the index.
func (d *DatabaseIndex) SeriesKeys() []string {
	d.mu.RLock()
	s := make([]string, 0, len(d.series))
	for k := range d.series {
		s = append(s, k)
	}
	d.mu.RUnlock()
	return s
}

// SeriesN returns the number of series.
func (d *DatabaseIndex) SeriesN() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.series)
}

// Measurement returns the measurement object from the index by the name.
func (d *DatabaseIndex) Measurement(name string) *Measurement {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.measurements[name]
}

// MeasurementsByName returns a list of all the measurements in the index that match any entry in names.
func (d *DatabaseIndex) MeasurementsByName(names []string) []*Measurement {
	d.mu.RLock()
	defer d.mu.RUnlock()

	a := make([]*Measurement, 0, len(names))
	for _, name := range names {
		if m := d.measurements[name]; m != nil {
			a = append(a, m)
		}
	}
	return a
}

// MeasurementSeriesCounts returns the number of measurements and series currently indexed by the database.
// Useful for reporting and monitoring.
func (d *DatabaseIndex) MeasurementSeriesCounts() (nMeasurements int, nSeries int) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	nMeasurements, nSeries = len(d.measurements), len(d.series)
	return
}

// SeriesShardN returns the series count for a shard.
func (d *DatabaseIndex) SeriesShardN(shardID uint64) int {
	var n int
	d.mu.RLock()
	for _, s := range d.series {
		if s.Assigned(shardID) {
			n++
		}
	}
	d.mu.RUnlock()
	return n
}

// CreateSeriesIndexIfNotExists adds the series for the given measurement to the index and sets its ID or returns the existing series object.
func (d *DatabaseIndex) CreateSeriesIndexIfNotExists(measurementName string, series *Series, forceCopy bool) *Series {
	d.mu.RLock()
	// if there is a measurement for this id, it's already been added
	ss := d.series[series.Key]
	if ss != nil {
		d.mu.RUnlock()
		return ss
	}
	d.mu.RUnlock()

	// get or create the measurement index
	m := d.CreateMeasurementIndexIfNotExists(measurementName)

	d.mu.Lock()
	// Check for the series again under a write lock
	ss = d.series[series.Key]
	if ss != nil {
		d.mu.Unlock()
		return ss
	}

	// set the in memory ID for query processing on this shard
	series.ID = d.lastID + 1
	d.lastID++

	series.measurement = m

	// Clone the tags to dereference any short-term buffers
	if forceCopy {
		series.CopyTags()
	}
	d.series[series.Key] = series

	m.AddSeries(series)

	atomic.AddInt64(&d.stats.NumSeries, 1)
	d.mu.Unlock()

	return series
}

// CreateMeasurementIndexIfNotExists creates or retrieves an in-memory index object for the measurement.
func (d *DatabaseIndex) CreateMeasurementIndexIfNotExists(name string) *Measurement {
	name = escape.UnescapeString(name)

	// See if the measurement exists using a read-lock
	d.mu.RLock()
	m := d.measurements[name]
	if m != nil {
		d.mu.RUnlock()
		return m
	}
	d.mu.RUnlock()

	// Doesn't exist, so lock the index to create it
	d.mu.Lock()
	defer d.mu.Unlock()

	// Make sure it was created in between the time we released our read-lock
	// and acquire the write lock
	m = d.measurements[name]
	if m == nil {
		m = NewMeasurement(name)
		d.measurements[name] = m
		atomic.AddInt64(&d.stats.NumMeasurements, 1)
	}
	return m
}

// AssignShard updates the index to indicate that series k exists in
// the given shardID.
func (d *DatabaseIndex) AssignShard(k string, shardID uint64) {
	ss := d.Series(k)
	if ss != nil {
		ss.AssignShard(shardID)
	}
}

// UnassignShard updates the index to indicate that series k does not exist in
// the given shardID.
func (d *DatabaseIndex) UnassignShard(k string, shardID uint64) {
	ss := d.Series(k)
	if ss != nil {
		if ss.Assigned(shardID) {
			// Remove the shard from any series
			ss.UnassignShard(shardID)

			// If this series no longer has shards assigned, remove the series
			if ss.ShardN() == 0 {

				// Remove the series the measurements
				ss.measurement.DropSeries(ss)

				// If the measurement no longer has any series, remove it as well
				if !ss.measurement.HasSeries() {
					d.mu.Lock()
					d.dropMeasurement(ss.measurement.Name)
					d.mu.Unlock()
				}

				// Remove the series key from the series index
				d.mu.Lock()
				delete(d.series, k)
				atomic.AddInt64(&d.stats.NumSeries, -1)
				atomic.AddInt64(&d.stats.NumSeriesDropped, 1)
				d.mu.Unlock()
			}
		}
	}
}

// RemoveShard removes all references to shardID from any series or measurements
// in the index.  If the shard was the only owner of data for the series, the series
// is removed from the index.
func (d *DatabaseIndex) RemoveShard(shardID uint64) {
	for _, k := range d.SeriesKeys() {
		d.UnassignShard(k, shardID)
	}
}

// TagsForSeries returns the tag map for the passed in series
func (d *DatabaseIndex) TagsForSeries(key string) models.Tags {
	d.mu.RLock()
	defer d.mu.RUnlock()

	ss := d.series[key]
	if ss == nil {
		return nil
	}
	return ss.Tags()
}

// MeasurementsByExpr takes an expression containing only tags and returns a
// list of matching *Measurement. The bool return argument returns if the
// expression was a measurement expression. It is used to differentiate a list
// of no measurements because all measurements were filtered out (when the bool
// is true) against when there are no measurements because the expression
// wasn't evaluated (when the bool is false).
func (d *DatabaseIndex) MeasurementsByExpr(expr influxql.Expr) (Measurements, bool, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.measurementsByExpr(expr)
}

func (d *DatabaseIndex) measurementsByExpr(expr influxql.Expr) (Measurements, bool, error) {
	if expr == nil {
		return nil, false, nil
	}

	switch e := expr.(type) {
	case *influxql.BinaryExpr:
		switch e.Op {
		case influxql.EQ, influxql.NEQ, influxql.EQREGEX, influxql.NEQREGEX:
			tag, ok := e.LHS.(*influxql.VarRef)
			if !ok {
				return nil, false, fmt.Errorf("left side of '%s' must be a tag key", e.Op.String())
			}

			tf := &TagFilter{
				Op:  e.Op,
				Key: tag.Val,
			}

			if influxql.IsRegexOp(e.Op) {
				re, ok := e.RHS.(*influxql.RegexLiteral)
				if !ok {
					return nil, false, fmt.Errorf("right side of '%s' must be a regular expression", e.Op.String())
				}
				tf.Regex = re.Val
			} else {
				s, ok := e.RHS.(*influxql.StringLiteral)
				if !ok {
					return nil, false, fmt.Errorf("right side of '%s' must be a tag value string", e.Op.String())
				}
				tf.Value = s.Val
			}

			// Match on name, if specified.
			if tag.Val == "_name" {
				return d.measurementsByNameFilter(tf.Op, tf.Value, tf.Regex), true, nil
			} else if influxql.IsSystemName(tag.Val) {
				return nil, false, nil
			}

			return d.measurementsByTagFilters([]*TagFilter{tf}), true, nil
		case influxql.OR, influxql.AND:
			lhsIDs, lhsOk, err := d.measurementsByExpr(e.LHS)
			if err != nil {
				return nil, false, err
			}

			rhsIDs, rhsOk, err := d.measurementsByExpr(e.RHS)
			if err != nil {
				return nil, false, err
			}

			if lhsOk && rhsOk {
				if e.Op == influxql.OR {
					return lhsIDs.union(rhsIDs), true, nil
				}

				return lhsIDs.intersect(rhsIDs), true, nil
			} else if lhsOk {
				return lhsIDs, true, nil
			} else if rhsOk {
				return rhsIDs, true, nil
			}
			return nil, false, nil
		default:
			return nil, false, fmt.Errorf("invalid tag comparison operator")
		}
	case *influxql.ParenExpr:
		return d.measurementsByExpr(e.Expr)
	}
	return nil, false, fmt.Errorf("%#v", expr)
}

// measurementsByNameFilter returns the sorted measurements matching a name.
func (d *DatabaseIndex) measurementsByNameFilter(op influxql.Token, val string, regex *regexp.Regexp) Measurements {
	var measurements Measurements
	for _, m := range d.measurements {
		var matched bool
		switch op {
		case influxql.EQ:
			matched = m.Name == val
		case influxql.NEQ:
			matched = m.Name != val
		case influxql.EQREGEX:
			matched = regex.MatchString(m.Name)
		case influxql.NEQREGEX:
			matched = !regex.MatchString(m.Name)
		}

		if !matched {
			continue
		}
		measurements = append(measurements, m)
	}
	sort.Sort(measurements)
	return measurements
}

// measurementsByTagFilters returns the sorted measurements matching the filters on tag values.
func (d *DatabaseIndex) measurementsByTagFilters(filters []*TagFilter) Measurements {
	// If no filters, then return all measurements.
	if len(filters) == 0 {
		measurements := make(Measurements, 0, len(d.measurements))
		for _, m := range d.measurements {
			measurements = append(measurements, m)
		}
		return measurements
	}

	// Build a list of measurements matching the filters.
	var measurements Measurements
	var tagMatch bool

	// Iterate through all measurements in the database.
	for _, m := range d.measurements {
		// Iterate filters seeing if the measurement has a matching tag.
		for _, f := range filters {
			m.mu.RLock()
			tagVals, ok := m.seriesByTagKeyValue[f.Key]
			m.mu.RUnlock()
			if !ok {
				continue
			}

			tagMatch = false

			// If the operator is non-regex, only check the specified value.
			if f.Op == influxql.EQ || f.Op == influxql.NEQ {
				if _, ok := tagVals[f.Value]; ok {
					tagMatch = true
				}
			} else {
				// Else, the operator is a regex and we have to check all tag
				// values against the regular expression.
				for tagVal := range tagVals {
					if f.Regex.MatchString(tagVal) {
						tagMatch = true
						break
					}
				}
			}

			isEQ := (f.Op == influxql.EQ || f.Op == influxql.EQREGEX)

			// tags match | operation is EQ | measurement matches
			// --------------------------------------------------
			//     True   |       True      |      True
			//     True   |       False     |      False
			//     False  |       True      |      False
			//     False  |       False     |      True

			if tagMatch == isEQ {
				measurements = append(measurements, m)
				break
			}
		}
	}

	sort.Sort(measurements)
	return measurements
}

// MeasurementsByRegex returns the measurements that match the regex.
func (d *DatabaseIndex) MeasurementsByRegex(re *regexp.Regexp) Measurements {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var matches Measurements
	for _, m := range d.measurements {
		if re.MatchString(m.Name) {
			matches = append(matches, m)
		}
	}
	return matches
}

// Measurements returns a list of all measurements.
func (d *DatabaseIndex) Measurements() Measurements {
	d.mu.RLock()
	measurements := make(Measurements, 0, len(d.measurements))
	for _, m := range d.measurements {
		measurements = append(measurements, m)
	}
	d.mu.RUnlock()

	return measurements
}

// DropMeasurement removes the measurement and all of its underlying
// series from the database index.
func (d *DatabaseIndex) DropMeasurement(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dropMeasurement(name)
}

func (d *DatabaseIndex) dropMeasurement(name string) {
	m := d.measurements[name]
	if m == nil {
		return
	}

	delete(d.measurements, name)
	for _, s := range m.seriesByID {
		delete(d.series, s.Key)
	}

	atomic.AddInt64(&d.stats.NumSeries, int64(-len(m.seriesByID)))
	atomic.AddInt64(&d.stats.NumSeriesDropped, int64(len(m.seriesByID)))
	atomic.AddInt64(&d.stats.NumMeasurements, -1)
	atomic.AddInt64(&d.stats.NumMeasurementsDropped, 1)
}

// Measurement represents a collection of time series in a database. It also contains in-memory
// structures for indexing tags. Exported functions are goroutine safe while un-exported functions
// assume the caller will use the appropriate locks.
type Measurement struct {
	mu         sync.RWMutex
	Name       string `json:"name,omitempty"`
	fieldNames map[string]struct{}

	// in-memory index fields
	seriesByID          map[uint64]*Series              // lookup table for series by their id
	seriesByTagKeyValue map[string]map[string]SeriesIDs // map from tag key to value to sorted set of series ids

	// lazyily created sorted series IDs
	sortedSeriesIDs SeriesIDs // sorted list of series IDs in this measurement
}

// NewMeasurement allocates and initializes a new Measurement.
func NewMeasurement(name string) *Measurement {
	return &Measurement{
		Name:       name,
		fieldNames: make(map[string]struct{}),

		seriesByID:          make(map[uint64]*Series),
		seriesByTagKeyValue: make(map[string]map[string]SeriesIDs),
	}
}

// HasField returns true if the measurement has a field by the given name.
func (m *Measurement) HasField(name string) bool {
	m.mu.RLock()
	hasField := m.hasField(name)
	m.mu.RUnlock()
	return hasField
}

func (m *Measurement) hasField(name string) bool {
	_, hasField := m.fieldNames[name]
	return hasField
}

// SeriesByID returns a series by identifier.
func (m *Measurement) SeriesByID(id uint64) *Series {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.seriesByID[id]
}

// SeriesByIDSlice returns a list of series by identifiers.
func (m *Measurement) SeriesByIDSlice(ids []uint64) []*Series {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a := make([]*Series, len(ids))
	for i, id := range ids {
		a[i] = m.seriesByID[id]
	}
	return a
}

// AppendSeriesKeysByID appends keys for a list of series ids to a buffer.
func (m *Measurement) AppendSeriesKeysByID(dst []string, ids []uint64) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, id := range ids {
		if s := m.seriesByID[id]; s != nil {
			dst = append(dst, s.Key)
		}
	}
	return dst
}

// SeriesKeys returns the keys of every series in this measurement.
func (m *Measurement) SeriesKeys() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	keys := make([]string, 0, len(m.seriesByID))
	for _, s := range m.seriesByID {
		keys = append(keys, s.Key)
	}
	return keys
}

func (m *Measurement) SeriesIDs() SeriesIDs {
	m.mu.RLock()
	if len(m.sortedSeriesIDs) == len(m.seriesByID) {
		s := m.sortedSeriesIDs
		m.mu.RUnlock()
		return s
	}
	m.mu.RUnlock()

	m.mu.Lock()
	if len(m.sortedSeriesIDs) == len(m.seriesByID) {
		s := m.sortedSeriesIDs
		m.mu.Unlock()
		return s
	}

	if cap(m.sortedSeriesIDs) < len(m.seriesByID) {
		m.sortedSeriesIDs = make(SeriesIDs, 0, len(m.seriesByID))
	}
	for k := range m.seriesByID {
		m.sortedSeriesIDs = append(m.sortedSeriesIDs, k)
	}
	sort.Sort(m.sortedSeriesIDs)
	s := m.sortedSeriesIDs
	m.mu.Unlock()
	return s
}

// ValidateGroupBy ensures that the GROUP BY is not a field.
func (m *Measurement) ValidateGroupBy(stmt *influxql.SelectStatement) error {
	for _, d := range stmt.Dimensions {
		switch e := d.Expr.(type) {
		case *influxql.VarRef:
			if m.HasField(e.Val) {
				return fmt.Errorf("can not use field in GROUP BY clause: %s", e.Val)
			}
		}
	}
	return nil
}

// HasTagKey returns true if at least one series in this measurement has written a value for the passed in tag key.
func (m *Measurement) HasTagKey(k string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, hasTag := m.seriesByTagKeyValue[k]
	return hasTag
}

// HasTagKeyValue returns true if at least one series in this measurement has written a value the given tag key and tag value.
func (m *Measurement) HasTagKeyValue(k, v []byte) bool {
	m.mu.RLock()
	if vals, ok := m.seriesByTagKeyValue[string(k)]; ok {
		_, ok := vals[string(v)]
		m.mu.RUnlock()
		return ok
	}
	m.mu.RUnlock()
	return false
}

// HasSeries returns true if there is at least 1 series under this measurement.
func (m *Measurement) HasSeries() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.seriesByID) > 0
}

// Cardinality returns the number of values associated with the given tag key.
func (m *Measurement) Cardinality(key string) int {
	var n int
	m.mu.RLock()
	n = m.cardinality(key)
	m.mu.RUnlock()
	return n
}

func (m *Measurement) cardinality(key string) int {
	return len(m.seriesByTagKeyValue[key])
}

// CardinalityBytes returns the number of values associated with the given tag key.
func (m *Measurement) CardinalityBytes(key []byte) int {
	var n int
	m.mu.RLock()
	n = len(m.seriesByTagKeyValue[string(key)])
	m.mu.RUnlock()
	return n
}

// AddSeries adds a series to the measurement's index.
// It returns true if the series was added successfully or false if the series was already present.
func (m *Measurement) AddSeries(s *Series) bool {
	m.mu.RLock()
	if _, ok := m.seriesByID[s.ID]; ok {
		m.mu.RUnlock()
		return false
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.seriesByID[s.ID]; ok {
		return false
	}

	m.seriesByID[s.ID] = s

	if len(m.sortedSeriesIDs) == 0 || s.ID > m.sortedSeriesIDs[len(m.sortedSeriesIDs)-1] {
		m.sortedSeriesIDs = append(m.sortedSeriesIDs, s.ID)
	}

	// add this series id to the tag index on the measurement
	s.ForEachTag(func(t models.Tag) {
		valueMap := m.seriesByTagKeyValue[string(t.Key)]
		if valueMap == nil {
			valueMap = make(map[string]SeriesIDs)
			m.seriesByTagKeyValue[string(t.Key)] = valueMap
		}
		ids := valueMap[string(t.Value)]
		ids = append(ids, s.ID)

		// most of the time the series ID will be higher than all others because it's a new
		// series. So don't do the sort if we don't have to.
		if len(ids) > 1 && ids[len(ids)-1] < ids[len(ids)-2] {
			sort.Sort(ids)
		}
		valueMap[string(t.Value)] = ids
	})

	return true
}

// DropSeries removes a series from the measurement's index.
func (m *Measurement) DropSeries(series *Series) {
	seriesID := series.ID
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.seriesByID[seriesID]; !ok {
		return
	}
	delete(m.seriesByID, seriesID)

	// clear our lazily sorted set of ids
	m.sortedSeriesIDs = m.sortedSeriesIDs[:0]

	// remove this series id from the tag index on the measurement
	// s.seriesByTagKeyValue is defined as map[string]map[string]SeriesIDs
	series.ForEachTag(func(t models.Tag) {
		values := m.seriesByTagKeyValue[string(t.Key)][string(t.Value)]
		ids := filter(values, seriesID)
		// Check to see if we have any ids, if not, remove the key
		if len(ids) == 0 {
			delete(m.seriesByTagKeyValue[string(t.Key)], string(t.Value))
		} else {
			m.seriesByTagKeyValue[string(t.Key)][string(t.Value)] = ids
		}

		// If we have no values, then we delete the key
		if len(m.seriesByTagKeyValue[string(t.Key)]) == 0 {
			delete(m.seriesByTagKeyValue, string(t.Key))
		}
	})

	return
}

// filters walks the where clause of a select statement and returns a map with all series ids
// matching the where clause and any filter expression that should be applied to each.
func (m *Measurement) filters(condition influxql.Expr) ([]uint64, map[uint64]influxql.Expr, error) {
	if condition == nil || influxql.OnlyTimeExpr(condition) {
		return m.SeriesIDs(), nil, nil
	}
	return m.walkWhereForSeriesIds(condition)
}

// TagSets returns the unique tag sets that exist for the given tag keys. This is used to determine
// what composite series will be created by a group by. i.e. "group by region" should return:
//   {"region":"uswest"}, {"region":"useast"}
// or region, service returns
//   {"region": "uswest", "service": "redis"}, {"region": "uswest", "service": "mysql"}, ...
// This will also populate the TagSet objects with the series IDs that match each tagset and any
// influx filter expression that goes with the series
// TODO: this shouldn't be exported. However, until tx.go and the engine get refactored into tsdb, we need it.
func (m *Measurement) TagSets(shardID uint64, opt influxql.IteratorOptions) ([]*influxql.TagSet, error) {
	m.mu.RLock()

	// get the unique set of series ids and the filters that should be applied to each
	ids, filters, err := m.filters(opt.Condition)
	if err != nil {
		m.mu.RUnlock()
		return nil, err
	}

	// For every series, get the tag values for the requested tag keys i.e. dimensions. This is the
	// TagSet for that series. Series with the same TagSet are then grouped together, because for the
	// purpose of GROUP BY they are part of the same composite series.
	tagSets := make(map[string]*influxql.TagSet, 64)
	var seriesN int
	for _, id := range ids {
		// Abort if the query was killed
		select {
		case <-opt.InterruptCh:
			return nil, influxql.ErrQueryInterrupted
		default:
		}

		if opt.MaxSeriesN > 0 && seriesN > opt.MaxSeriesN {
			return nil, fmt.Errorf("max-select-series limit exceeded: (%d/%d)", seriesN, opt.MaxSeriesN)
		}

		s := m.seriesByID[id]
		if !s.Assigned(shardID) {
			continue
		}
		tags := make(map[string]string, len(opt.Dimensions))

		// Build the TagSet for this series.
		for _, dim := range opt.Dimensions {
			tags[dim] = s.GetTagString(dim)
		}

		// Convert the TagSet to a string, so it can be added to a map allowing TagSets to be handled
		// as a set.
		tagsAsKey := MarshalTags(tags)
		tagSet, ok := tagSets[string(tagsAsKey)]
		if !ok {
			// This TagSet is new, create a new entry for it.
			tagSet = &influxql.TagSet{
				Tags: tags,
				Key:  tagsAsKey,
			}
		}
		// Associate the series and filter with the Tagset.
		tagSet.AddFilter(m.seriesByID[id].Key, filters[id])
		seriesN++

		// Ensure it's back in the map.
		tagSets[string(tagsAsKey)] = tagSet
	}
	// Release the lock while we sort all the tags
	m.mu.RUnlock()

	// Sort the series in each tag set.
	for _, t := range tagSets {
		// Abort if the query was killed
		select {
		case <-opt.InterruptCh:
			return nil, influxql.ErrQueryInterrupted
		default:
		}

		sort.Sort(t)
	}

	// The TagSets have been created, as a map of TagSets. Just send
	// the values back as a slice, sorting for consistency.
	sortedTagsSets := make([]*influxql.TagSet, 0, len(tagSets))
	for _, v := range tagSets {
		sortedTagsSets = append(sortedTagsSets, v)
	}
	sort.Sort(byTagKey(sortedTagsSets))

	return sortedTagsSets, nil
}

// intersectSeriesFilters performs an intersection for two sets of ids and filter expressions.
func intersectSeriesFilters(lids, rids SeriesIDs, lfilters, rfilters FilterExprs) (SeriesIDs, FilterExprs) {
	// We only want to allocate a slice and map of the smaller size.
	var ids []uint64
	if len(lids) > len(rids) {
		ids = make([]uint64, 0, len(rids))
	} else {
		ids = make([]uint64, 0, len(lids))
	}

	var filters FilterExprs
	if len(lfilters) > len(rfilters) {
		filters = make(FilterExprs, len(rfilters))
	} else {
		filters = make(FilterExprs, len(lfilters))
	}

	// They're in sorted order so advance the counter as needed.
	// This is, don't run comparisons against lower values that we've already passed.
	for len(lids) > 0 && len(rids) > 0 {
		lid, rid := lids[0], rids[0]
		if lid == rid {
			ids = append(ids, lid)

			var expr influxql.Expr
			lfilter := lfilters[lid]
			rfilter := rfilters[rid]

			if lfilter != nil && rfilter != nil {
				be := &influxql.BinaryExpr{
					Op:  influxql.AND,
					LHS: lfilter,
					RHS: rfilter,
				}
				expr = influxql.Reduce(be, nil)
			} else if lfilter != nil {
				expr = lfilter
			} else if rfilter != nil {
				expr = rfilter
			}

			if expr != nil {
				filters[lid] = expr
			}
			lids, rids = lids[1:], rids[1:]
		} else if lid < rid {
			lids = lids[1:]
		} else {
			rids = rids[1:]
		}
	}
	return ids, filters
}

// unionSeriesFilters performs a union for two sets of ids and filter expressions.
func unionSeriesFilters(lids, rids SeriesIDs, lfilters, rfilters FilterExprs) (SeriesIDs, FilterExprs) {
	ids := make([]uint64, 0, len(lids)+len(rids))

	// Setup the filters with the smallest size since we will discard filters
	// that do not have a match on the other side.
	var filters FilterExprs
	if len(lfilters) < len(rfilters) {
		filters = make(FilterExprs, len(lfilters))
	} else {
		filters = make(FilterExprs, len(rfilters))
	}

	for len(lids) > 0 && len(rids) > 0 {
		lid, rid := lids[0], rids[0]
		if lid == rid {
			ids = append(ids, lid)

			// If one side does not have a filter, then the series has been
			// included on one side of the OR with no condition. Eliminate the
			// filter in this case.
			var expr influxql.Expr
			lfilter := lfilters[lid]
			rfilter := rfilters[rid]
			if lfilter != nil && rfilter != nil {
				be := &influxql.BinaryExpr{
					Op:  influxql.OR,
					LHS: lfilter,
					RHS: rfilter,
				}
				expr = influxql.Reduce(be, nil)
			}

			if expr != nil {
				filters[lid] = expr
			}
			lids, rids = lids[1:], rids[1:]
		} else if lid < rid {
			ids = append(ids, lid)

			filter := lfilters[lid]
			if filter != nil {
				filters[lid] = filter
			}
			lids = lids[1:]
		} else {
			ids = append(ids, rid)

			filter := rfilters[rid]
			if filter != nil {
				filters[rid] = filter
			}
			rids = rids[1:]
		}
	}

	// Now append the remainder.
	if len(lids) > 0 {
		for i := 0; i < len(lids); i++ {
			ids = append(ids, lids[i])

			filter := lfilters[lids[i]]
			if filter != nil {
				filters[lids[i]] = filter
			}
		}
	} else if len(rids) > 0 {
		for i := 0; i < len(rids); i++ {
			ids = append(ids, rids[i])

			filter := rfilters[rids[i]]
			if filter != nil {
				filters[rids[i]] = filter
			}
		}
	}
	return ids, filters
}

// IDsForExpr returns the series IDs that are candidates to match the given expression.
func (m *Measurement) IDsForExpr(n *influxql.BinaryExpr) SeriesIDs {
	ids, _, _ := m.idsForExpr(n)
	return ids
}

// idsForExpr returns a collection of series ids and a filter expression that should
// be used to filter points from those series.
func (m *Measurement) idsForExpr(n *influxql.BinaryExpr) (SeriesIDs, influxql.Expr, error) {
	// If this binary expression has another binary expression, then this
	// is some expression math and we should just pass it to the underlying query.
	if _, ok := n.LHS.(*influxql.BinaryExpr); ok {
		return m.SeriesIDs(), n, nil
	} else if _, ok := n.RHS.(*influxql.BinaryExpr); ok {
		return m.SeriesIDs(), n, nil
	}

	// Retrieve the variable reference from the correct side of the expression.
	name, ok := n.LHS.(*influxql.VarRef)
	value := n.RHS
	if !ok {
		name, ok = n.RHS.(*influxql.VarRef)
		if !ok {
			return nil, nil, fmt.Errorf("invalid expression: %s", n.String())
		}
		value = n.LHS
	}

	// For time literals, return all series IDs and "true" as the filter.
	if _, ok := value.(*influxql.TimeLiteral); ok || name.Val == "time" {
		return m.SeriesIDs(), &influxql.BooleanLiteral{Val: true}, nil
	}

	// For fields, return all series IDs from this measurement and return
	// the expression passed in, as the filter.
	if name.Val != "_name" && ((name.Type == influxql.Unknown && m.hasField(name.Val)) || name.Type == influxql.AnyField || (name.Type != influxql.Tag && name.Type != influxql.Unknown)) {
		return m.SeriesIDs(), n, nil
	} else if value, ok := value.(*influxql.VarRef); ok {
		// Check if the RHS is a variable and if it is a field.
		if value.Val != "_name" && ((value.Type == influxql.Unknown && m.hasField(value.Val)) || name.Type == influxql.AnyField || (value.Type != influxql.Tag && value.Type != influxql.Unknown)) {
			return m.SeriesIDs(), n, nil
		}
	}

	// Retrieve list of series with this tag key.
	tagVals := m.seriesByTagKeyValue[name.Val]

	// if we're looking for series with a specific tag value
	if str, ok := value.(*influxql.StringLiteral); ok {
		var ids SeriesIDs

		// Special handling for "_name" to match measurement name.
		if name.Val == "_name" {
			if (n.Op == influxql.EQ && str.Val == m.Name) || (n.Op == influxql.NEQ && str.Val != m.Name) {
				return m.SeriesIDs(), nil, nil
			}
			return nil, nil, nil
		}

		if n.Op == influxql.EQ {
			if str.Val != "" {
				// return series that have a tag of specific value.
				ids = tagVals[str.Val]
			} else {
				// Make a copy of all series ids and mark the ones we need to evict.
				seriesIDs := newEvictSeriesIDs(m.SeriesIDs())

				// Go through each slice and mark the values we find as zero so
				// they can be removed later.
				for _, a := range tagVals {
					seriesIDs.mark(a)
				}

				// Make a new slice with only the remaining ids.
				ids = seriesIDs.evict()
			}
		} else if n.Op == influxql.NEQ {
			if str.Val != "" {
				ids = m.SeriesIDs().Reject(tagVals[str.Val])
			} else {
				for k := range tagVals {
					ids = append(ids, tagVals[k]...)
				}
				sort.Sort(ids)
			}
		}
		return ids, nil, nil
	}

	// if we're looking for series with a tag value that matches a regex
	if re, ok := value.(*influxql.RegexLiteral); ok {
		var ids SeriesIDs

		// Special handling for "_name" to match measurement name.
		if name.Val == "_name" {
			match := re.Val.MatchString(m.Name)
			if (n.Op == influxql.EQREGEX && match) || (n.Op == influxql.NEQREGEX && !match) {
				return m.SeriesIDs(), &influxql.BooleanLiteral{Val: true}, nil
			}
			return nil, nil, nil
		}

		// Check if we match the empty string to see if we should include series
		// that are missing the tag.
		empty := re.Val.MatchString("")

		// Gather the series that match the regex. If we should include the empty string,
		// start with the list of all series and reject series that don't match our condition.
		// If we should not include the empty string, include series that match our condition.
		if empty && n.Op == influxql.EQREGEX {
			// See comments above for EQ with a StringLiteral.
			seriesIDs := newEvictSeriesIDs(m.SeriesIDs())
			for k := range tagVals {
				if !re.Val.MatchString(k) {
					seriesIDs.mark(tagVals[k])
				}
			}
			ids = seriesIDs.evict()
		} else if empty && n.Op == influxql.NEQREGEX {
			ids = make(SeriesIDs, 0, len(m.SeriesIDs()))
			for k := range tagVals {
				if !re.Val.MatchString(k) {
					ids = append(ids, tagVals[k]...)
				}
			}
			sort.Sort(ids)
		} else if !empty && n.Op == influxql.EQREGEX {
			ids = make(SeriesIDs, 0, len(m.SeriesIDs()))
			for k := range tagVals {
				if re.Val.MatchString(k) {
					ids = append(ids, tagVals[k]...)
				}
			}
			sort.Sort(ids)
		} else if !empty && n.Op == influxql.NEQREGEX {
			// See comments above for EQ with a StringLiteral.
			seriesIDs := newEvictSeriesIDs(m.SeriesIDs())
			for k := range tagVals {
				if re.Val.MatchString(k) {
					seriesIDs.mark(tagVals[k])
				}
			}
			ids = seriesIDs.evict()
		}
		return ids, nil, nil
	}

	// compare tag values
	if ref, ok := value.(*influxql.VarRef); ok {
		var ids SeriesIDs

		if n.Op == influxql.NEQ {
			ids = m.SeriesIDs()
		}

		rhsTagVals := m.seriesByTagKeyValue[ref.Val]
		for k := range tagVals {
			tags := tagVals[k].Intersect(rhsTagVals[k])
			if n.Op == influxql.EQ {
				ids = ids.Union(tags)
			} else if n.Op == influxql.NEQ {
				ids = ids.Reject(tags)
			}
		}
		return ids, nil, nil
	}

	if n.Op == influxql.NEQ || n.Op == influxql.NEQREGEX {
		return m.SeriesIDs(), nil, nil
	}
	return nil, nil, nil
}

// FilterExprs represents a map of series IDs to filter expressions.
type FilterExprs map[uint64]influxql.Expr

// DeleteBoolLiteralTrues deletes all elements whose filter expression is a boolean literal true.
func (fe FilterExprs) DeleteBoolLiteralTrues() {
	for id, expr := range fe {
		if e, ok := expr.(*influxql.BooleanLiteral); ok && e.Val {
			delete(fe, id)
		}
	}
}

// Len returns the number of elements.
func (fe FilterExprs) Len() int {
	if fe == nil {
		return 0
	}
	return len(fe)
}

// walkWhereForSeriesIds recursively walks the WHERE clause and returns an ordered set of series IDs and
// a map from those series IDs to filter expressions that should be used to limit points returned in
// the final query result.
func (m *Measurement) walkWhereForSeriesIds(expr influxql.Expr) (SeriesIDs, FilterExprs, error) {
	switch n := expr.(type) {
	case *influxql.BinaryExpr:
		switch n.Op {
		case influxql.EQ, influxql.NEQ, influxql.LT, influxql.LTE, influxql.GT, influxql.GTE, influxql.EQREGEX, influxql.NEQREGEX:
			// Get the series IDs and filter expression for the tag or field comparison.
			ids, expr, err := m.idsForExpr(n)
			if err != nil {
				return nil, nil, err
			}

			if len(ids) == 0 {
				return ids, nil, nil
			}

			// If the expression is a boolean literal that is true, ignore it.
			if b, ok := expr.(*influxql.BooleanLiteral); ok && b.Val {
				expr = nil
			}

			var filters FilterExprs
			if expr != nil {
				filters = make(FilterExprs, len(ids))
				for _, id := range ids {
					filters[id] = expr
				}
			}

			return ids, filters, nil
		case influxql.AND, influxql.OR:
			// Get the series IDs and filter expressions for the LHS.
			lids, lfilters, err := m.walkWhereForSeriesIds(n.LHS)
			if err != nil {
				return nil, nil, err
			}

			// Get the series IDs and filter expressions for the RHS.
			rids, rfilters, err := m.walkWhereForSeriesIds(n.RHS)
			if err != nil {
				return nil, nil, err
			}

			// Combine the series IDs from the LHS and RHS.
			if n.Op == influxql.AND {
				ids, filters := intersectSeriesFilters(lids, rids, lfilters, rfilters)
				return ids, filters, nil
			} else {
				ids, filters := unionSeriesFilters(lids, rids, lfilters, rfilters)
				return ids, filters, nil
			}
		}

		ids, _, err := m.idsForExpr(n)
		return ids, nil, err
	case *influxql.ParenExpr:
		// walk down the tree
		return m.walkWhereForSeriesIds(n.Expr)
	default:
		return nil, nil, nil
	}
}

// SeriesIDsAllOrByExpr walks an expressions for matching series IDs
// or, if no expression is given, returns all series IDs for the measurement.
func (m *Measurement) SeriesIDsAllOrByExpr(expr influxql.Expr) (SeriesIDs, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.seriesIDsAllOrByExpr(expr)
}

func (m *Measurement) seriesIDsAllOrByExpr(expr influxql.Expr) (SeriesIDs, error) {
	// If no expression given or the measurement has no series,
	// we can take just return the ids or nil accordingly.
	if expr == nil {
		return m.SeriesIDs(), nil
	} else if len(m.seriesByID) == 0 {
		return nil, nil
	}

	// Get series IDs that match the WHERE clause.
	ids, _, err := m.walkWhereForSeriesIds(expr)
	if err != nil {
		return nil, err
	}

	return ids, nil
}

// tagKeysByExpr extracts the tag keys wanted by the expression.
func (m *Measurement) TagKeysByExpr(expr influxql.Expr) (stringSet, bool, error) {
	switch e := expr.(type) {
	case *influxql.BinaryExpr:
		switch e.Op {
		case influxql.EQ, influxql.NEQ, influxql.EQREGEX, influxql.NEQREGEX:
			tag, ok := e.LHS.(*influxql.VarRef)
			if !ok {
				return nil, false, fmt.Errorf("left side of '%s' must be a tag key", e.Op.String())
			}

			if tag.Val != "_tagKey" {
				return nil, false, nil
			}

			tf := TagFilter{
				Op: e.Op,
			}

			if influxql.IsRegexOp(e.Op) {
				re, ok := e.RHS.(*influxql.RegexLiteral)
				if !ok {
					return nil, false, fmt.Errorf("right side of '%s' must be a regular expression", e.Op.String())
				}
				tf.Regex = re.Val
			} else {
				s, ok := e.RHS.(*influxql.StringLiteral)
				if !ok {
					return nil, false, fmt.Errorf("right side of '%s' must be a tag value string", e.Op.String())
				}
				tf.Value = s.Val
			}
			return m.tagKeysByFilter(tf.Op, tf.Value, tf.Regex), true, nil
		case influxql.AND, influxql.OR:
			lhsKeys, lhsOk, err := m.TagKeysByExpr(e.LHS)
			if err != nil {
				return nil, false, err
			}

			rhsKeys, rhsOk, err := m.TagKeysByExpr(e.RHS)
			if err != nil {
				return nil, false, err
			}

			if lhsOk && rhsOk {
				if e.Op == influxql.OR {
					return lhsKeys.union(rhsKeys), true, nil
				}

				return lhsKeys.intersect(rhsKeys), true, nil
			} else if lhsOk {
				return lhsKeys, true, nil
			} else if rhsOk {
				return rhsKeys, true, nil
			}
			return nil, false, nil
		default:
			return nil, false, fmt.Errorf("invalid operator")
		}
	case *influxql.ParenExpr:
		return m.TagKeysByExpr(e.Expr)
	}
	return nil, false, fmt.Errorf("%#v", expr)
}

// tagKeysByFilter will filter the tag keys for the measurement.
func (m *Measurement) tagKeysByFilter(op influxql.Token, val string, regex *regexp.Regexp) stringSet {
	ss := newStringSet()
	for _, key := range m.TagKeys() {
		var matched bool
		switch op {
		case influxql.EQ:
			matched = key == val
		case influxql.NEQ:
			matched = key != val
		case influxql.EQREGEX:
			matched = regex.MatchString(key)
		case influxql.NEQREGEX:
			matched = !regex.MatchString(key)
		}

		if !matched {
			continue
		}
		ss.add(key)
	}
	return ss
}

// Measurements represents a list of *Measurement.
type Measurements []*Measurement

// Len implements sort.Interface.
func (a Measurements) Len() int { return len(a) }

// Less implements sort.Interface.
func (a Measurements) Less(i, j int) bool { return a[i].Name < a[j].Name }

// Swap implements sort.Interface.
func (a Measurements) Swap(i, j int) { a[i], a[j] = a[j], a[i] }

func (a Measurements) intersect(other Measurements) Measurements {
	l := a
	r := other

	// we want to iterate through the shortest one and stop
	if len(other) < len(a) {
		l = other
		r = a
	}

	// they're in sorted order so advance the counter as needed.
	// That is, don't run comparisons against lower values that we've already passed
	var i, j int

	result := make(Measurements, 0, len(l))
	for i < len(l) && j < len(r) {
		if l[i].Name == r[j].Name {
			result = append(result, l[i])
			i++
			j++
		} else if l[i].Name < r[j].Name {
			i++
		} else {
			j++
		}
	}

	return result
}

func (a Measurements) union(other Measurements) Measurements {
	result := make(Measurements, 0, len(a)+len(other))
	var i, j int
	for i < len(a) && j < len(other) {
		if a[i].Name == other[j].Name {
			result = append(result, a[i])
			i++
			j++
		} else if a[i].Name < other[j].Name {
			result = append(result, a[i])
			i++
		} else {
			result = append(result, other[j])
			j++
		}
	}

	// now append the remainder
	if i < len(a) {
		result = append(result, a[i:]...)
	} else if j < len(other) {
		result = append(result, other[j:]...)
	}

	return result
}

// Series belong to a Measurement and represent unique time series in a database.
type Series struct {
	mu          sync.RWMutex
	Key         string
	tags        models.Tags
	ID          uint64
	measurement *Measurement
	shardIDs    map[uint64]struct{} // shards that have this series defined
}

// NewSeries returns an initialized series struct.
func NewSeries(key string, tags models.Tags) *Series {
	return &Series{
		Key:      key,
		tags:     tags,
		shardIDs: make(map[uint64]struct{}),
	}
}

// AssignShard adds shardID to the list of shards this series is assigned to.
func (s *Series) AssignShard(shardID uint64) {
	s.mu.Lock()
	s.shardIDs[shardID] = struct{}{}
	s.mu.Unlock()
}

// UnassignShard removes the shardID from the list of shards this series is assigned to.
func (s *Series) UnassignShard(shardID uint64) {
	s.mu.Lock()
	delete(s.shardIDs, shardID)
	s.mu.Unlock()
}

// Assigned returns whether this series is assigned to the given shard.
func (s *Series) Assigned(shardID uint64) bool {
	s.mu.RLock()
	_, ok := s.shardIDs[shardID]
	s.mu.RUnlock()
	return ok
}

// ShardN returns the number of shards this series is assigned to.
func (s *Series) ShardN() int {
	s.mu.RLock()
	n := len(s.shardIDs)
	s.mu.RUnlock()
	return n
}

// ForEachTag executes fn for every tag. Iteration occurs under lock.
func (s *Series) ForEachTag(fn func(models.Tag)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.tags {
		fn(t)
	}
}

// Tags returns a copy of the tags under lock.
func (s *Series) Tags() models.Tags {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tags
}

// CopyTags clones the tags on the series in-place,
func (s *Series) CopyTags() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tags = s.tags.Clone()
}

// GetTagString returns a tag value under lock.
func (s *Series) GetTagString(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tags.GetString(key)
}

// MarshalBinary encodes the object to a binary format.
func (s *Series) MarshalBinary() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var pb internal.Series
	pb.Key = &s.Key
	for _, t := range s.tags {
		pb.Tags = append(pb.Tags, &internal.Tag{Key: proto.String(string(t.Key)), Value: proto.String(string(t.Value))})
	}
	return proto.Marshal(&pb)
}

// UnmarshalBinary decodes the object from a binary format.
func (s *Series) UnmarshalBinary(buf []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var pb internal.Series
	if err := proto.Unmarshal(buf, &pb); err != nil {
		return err
	}
	s.Key = pb.GetKey()
	s.tags = make(models.Tags, len(pb.Tags))
	for i, t := range pb.Tags {
		s.tags[i] = models.Tag{Key: []byte(t.GetKey()), Value: []byte(t.GetValue())}
	}
	return nil
}

// SeriesIDs is a convenience type for sorting, checking equality, and doing
// union and intersection of collections of series ids.
type SeriesIDs []uint64

// Len implements sort.Interface.
func (a SeriesIDs) Len() int { return len(a) }

// Less implements sort.Interface.
func (a SeriesIDs) Less(i, j int) bool { return a[i] < a[j] }

// Swap implements sort.Interface.
func (a SeriesIDs) Swap(i, j int) { a[i], a[j] = a[j], a[i] }

// Equals assumes that both are sorted.
func (a SeriesIDs) Equals(other SeriesIDs) bool {
	if len(a) != len(other) {
		return false
	}
	for i, s := range other {
		if a[i] != s {
			return false
		}
	}
	return true
}

// Intersect returns a new collection of series ids in sorted order that is the intersection of the two.
// The two collections must already be sorted.
func (a SeriesIDs) Intersect(other SeriesIDs) SeriesIDs {
	l := a
	r := other

	// we want to iterate through the shortest one and stop
	if len(other) < len(a) {
		l = other
		r = a
	}

	// they're in sorted order so advance the counter as needed.
	// That is, don't run comparisons against lower values that we've already passed
	var i, j int

	ids := make([]uint64, 0, len(l))
	for i < len(l) && j < len(r) {
		if l[i] == r[j] {
			ids = append(ids, l[i])
			i++
			j++
		} else if l[i] < r[j] {
			i++
		} else {
			j++
		}
	}

	return SeriesIDs(ids)
}

// Union returns a new collection of series ids in sorted order that is the union of the two.
// The two collections must already be sorted.
func (a SeriesIDs) Union(other SeriesIDs) SeriesIDs {
	l := a
	r := other
	ids := make([]uint64, 0, len(l)+len(r))
	var i, j int
	for i < len(l) && j < len(r) {
		if l[i] == r[j] {
			ids = append(ids, l[i])
			i++
			j++
		} else if l[i] < r[j] {
			ids = append(ids, l[i])
			i++
		} else {
			ids = append(ids, r[j])
			j++
		}
	}

	// now append the remainder
	if i < len(l) {
		ids = append(ids, l[i:]...)
	} else if j < len(r) {
		ids = append(ids, r[j:]...)
	}

	return ids
}

// Reject returns a new collection of series ids in sorted order with the passed in set removed from the original.
// This is useful for the NOT operator. The two collections must already be sorted.
func (a SeriesIDs) Reject(other SeriesIDs) SeriesIDs {
	l := a
	r := other
	var i, j int

	ids := make([]uint64, 0, len(l))
	for i < len(l) && j < len(r) {
		if l[i] == r[j] {
			i++
			j++
		} else if l[i] < r[j] {
			ids = append(ids, l[i])
			i++
		} else {
			j++
		}
	}

	// Append the remainder
	if i < len(l) {
		ids = append(ids, l[i:]...)
	}

	return SeriesIDs(ids)
}

// seriesID is a series id that may or may not have been evicted from the
// current id list.
type seriesID struct {
	val   uint64
	evict bool
}

// evictSeriesIDs is a slice of SeriesIDs with an extra field to mark if the
// field should be evicted or not.
type evictSeriesIDs struct {
	ids []seriesID
	sz  int
}

// newEvictSeriesIDs copies the ids into a new slice that can be used for
// evicting series from the slice.
func newEvictSeriesIDs(ids []uint64) evictSeriesIDs {
	a := make([]seriesID, len(ids))
	for i, id := range ids {
		a[i].val = id
	}
	return evictSeriesIDs{
		ids: a,
		sz:  len(a),
	}
}

// mark marks all of the ids in the sorted slice to be evicted from the list of
// series ids. If an id to be evicted does not exist, it just gets ignored.
func (a *evictSeriesIDs) mark(ids []uint64) {
	seriesIDs := a.ids
	for _, id := range ids {
		if len(seriesIDs) == 0 {
			break
		}

		// Perform a binary search of the remaining slice if
		// the first element does not match the value we're
		// looking for.
		i := 0
		if seriesIDs[0].val < id {
			i = sort.Search(len(seriesIDs), func(i int) bool {
				return seriesIDs[i].val >= id
			})
		}

		if i >= len(seriesIDs) {
			break
		} else if seriesIDs[i].val == id {
			if !seriesIDs[i].evict {
				seriesIDs[i].evict = true
				a.sz--
			}
			// Skip over this series since it has been evicted and won't be
			// encountered again.
			i++
		}
		seriesIDs = seriesIDs[i:]
	}
}

// evict creates a new slice with only the series that have not been evicted.
func (a *evictSeriesIDs) evict() (ids SeriesIDs) {
	if a.sz == 0 {
		return ids
	}

	// Make a new slice with only the remaining ids.
	ids = make([]uint64, 0, a.sz)
	for _, id := range a.ids {
		if id.evict {
			continue
		}
		ids = append(ids, id.val)
	}
	return ids
}

// TagFilter represents a tag filter when looking up other tags or measurements.
type TagFilter struct {
	Op    influxql.Token
	Key   string
	Value string
	Regex *regexp.Regexp
}

// MarshalTags converts a tag set to bytes for use as a lookup key.
func MarshalTags(tags map[string]string) []byte {
	// Empty maps marshal to empty bytes.
	if len(tags) == 0 {
		return nil
	}

	// Extract keys and determine final size.
	sz := (len(tags) * 2) - 1 // separators
	keys := make([]string, 0, len(tags))
	for k, v := range tags {
		keys = append(keys, k)
		sz += len(k) + len(v)
	}
	sort.Strings(keys)

	// Generate marshaled bytes.
	b := make([]byte, sz)
	buf := b
	for _, k := range keys {
		copy(buf, k)
		buf[len(k)] = '|'
		buf = buf[len(k)+1:]
	}
	for i, k := range keys {
		v := tags[k]
		copy(buf, v)
		if i < len(keys)-1 {
			buf[len(v)] = '|'
			buf = buf[len(v)+1:]
		}
	}
	return b
}

// WalkTagKeys calls fn for each tag key associated with m.  The order of the
// keys is undefined.
func (m *Measurement) WalkTagKeys(fn func(k string)) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for k := range m.seriesByTagKeyValue {
		fn(k)
	}
}

// TagKeys returns a list of the measurement's tag names, in sorted order.
func (m *Measurement) TagKeys() []string {
	m.mu.RLock()
	keys := make([]string, 0, len(m.seriesByTagKeyValue))
	for k := range m.seriesByTagKeyValue {
		keys = append(keys, k)
	}
	m.mu.RUnlock()
	sort.Strings(keys)
	return keys
}

// TagValues returns all the values for the given tag key, in an arbitrary order.
func (m *Measurement) TagValues(key string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	values := make([]string, 0, len(m.seriesByTagKeyValue[key]))
	for v := range m.seriesByTagKeyValue[key] {
		values = append(values, v)
	}
	return values
}

// SetFieldName adds the field name to the measurement.
func (m *Measurement) SetFieldName(name string) {
	m.mu.RLock()
	if _, ok := m.fieldNames[name]; ok {
		m.mu.RUnlock()
		return
	}
	m.mu.RUnlock()

	m.mu.Lock()
	m.fieldNames[name] = struct{}{}
	m.mu.Unlock()
}

// FieldNames returns a list of the measurement's field names, in an arbitrary order.
func (m *Measurement) FieldNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	a := make([]string, 0, len(m.fieldNames))
	for n := range m.fieldNames {
		a = append(a, n)
	}
	return a
}

// stringSet represents a set of strings.
type stringSet map[string]struct{}

// newStringSet returns an empty stringSet.
func newStringSet() stringSet {
	return make(map[string]struct{})
}

// add adds strings to the set.
func (s stringSet) add(ss ...string) {
	for _, n := range ss {
		s[n] = struct{}{}
	}
}

// list returns the current elements in the set, in sorted order.
func (s stringSet) list() []string {
	l := make([]string, 0, len(s))
	for k := range s {
		l = append(l, k)
	}
	sort.Strings(l)
	return l
}

// union returns the union of this set and another.
func (s stringSet) union(o stringSet) stringSet {
	ns := newStringSet()
	for k := range s {
		ns[k] = struct{}{}
	}
	for k := range o {
		ns[k] = struct{}{}
	}
	return ns
}

// intersect returns the intersection of this set and another.
func (s stringSet) intersect(o stringSet) stringSet {
	shorter, longer := s, o
	if len(longer) < len(shorter) {
		shorter, longer = longer, shorter
	}

	ns := newStringSet()
	for k := range shorter {
		if _, ok := longer[k]; ok {
			ns[k] = struct{}{}
		}
	}
	return ns
}

// filter removes v from a if it exists.  a must be sorted in ascending
// order.
func filter(a []uint64, v uint64) []uint64 {
	// binary search for v
	i := sort.Search(len(a), func(i int) bool { return a[i] >= v })
	if i >= len(a) || a[i] != v {
		return a
	}

	// we found it, so shift the right half down one, overwriting v's position.
	copy(a[i:], a[i+1:])
	return a[:len(a)-1]
}

// MeasurementFromSeriesKey returns the name of the measurement from a key that
// contains a measurement name.
func MeasurementFromSeriesKey(key string) string {
	// Ignoring the error because the func returns "missing fields"
	k, _, _ := models.ParseKey([]byte(key))
	return escape.UnescapeString(k)
}

type uint64Slice []uint64

func (a uint64Slice) Len() int           { return len(a) }
func (a uint64Slice) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a uint64Slice) Less(i, j int) bool { return a[i] < a[j] }

type byTagKey []*influxql.TagSet

func (t byTagKey) Len() int           { return len(t) }
func (t byTagKey) Less(i, j int) bool { return bytes.Compare(t[i].Key, t[j].Key) < 0 }
func (t byTagKey) Swap(i, j int)      { t[i], t[j] = t[j], t[i] }
