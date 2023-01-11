package phlaredb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bufbuild/connect-go"
	"github.com/dustin/go-humanize"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/gogo/status"
	"github.com/google/uuid"
	"github.com/grafana/dskit/multierror"
	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/tsdb/fileutil"
	"github.com/samber/lo"
	"go.uber.org/atomic"
	"google.golang.org/grpc/codes"

	profilev1 "github.com/grafana/phlare/api/gen/proto/go/google/v1"
	ingestv1 "github.com/grafana/phlare/api/gen/proto/go/ingester/v1"
	typesv1 "github.com/grafana/phlare/api/gen/proto/go/types/v1"
	"github.com/grafana/phlare/pkg/iter"
	phlaremodel "github.com/grafana/phlare/pkg/model"
	phlarecontext "github.com/grafana/phlare/pkg/phlare/context"
	"github.com/grafana/phlare/pkg/phlaredb/block"
	schemav1 "github.com/grafana/phlare/pkg/phlaredb/schemas/v1"
)

func copySlice[T any](in []T) []T {
	out := make([]T, len(in))
	copy(out, in)
	return out
}

type idConversionTable map[int64]int64

func (t idConversionTable) rewrite(idx *int64) {
	pos := *idx
	var ok bool
	*idx, ok = t[pos]
	if !ok {
		panic(fmt.Sprintf("unable to rewrite index %d", pos))
	}
}

func (t idConversionTable) rewriteUint64(idx *uint64) {
	pos := *idx
	v, ok := t[int64(pos)]
	if !ok {
		panic(fmt.Sprintf("unable to rewrite index %d", pos))
	}
	*idx = uint64(v)
}

type Models interface {
	*schemav1.Profile | *schemav1.Stacktrace | *profilev1.Location | *profilev1.Mapping | *profilev1.Function | string | *schemav1.StoredString
}

// rewriter contains slices to rewrite the per profile reference into per head references.
type rewriter struct {
	strings     stringConversionTable
	functions   idConversionTable
	mappings    idConversionTable
	locations   idConversionTable
	stacktraces idConversionTable
}

type Helper[M Models, K comparable] interface {
	key(M) K
	addToRewriter(*rewriter, idConversionTable)
	rewrite(*rewriter, M) error
	// some Models contain their own IDs within the struct, this allows to set them and keep track of the preexisting ID. It should return the oldID that is supposed to be rewritten.
	setID(existingSliceID uint64, newID uint64, element M) uint64

	// size returns a (rough estimation) of the size of a single element M
	size(M) uint64

	// clone copies parts that are not optimally sized from protobuf parsing
	clone(M) M
}

type Table interface {
	Name() string
	Size() uint64
	Init(path string, cfg *ParquetConfig) error
	Flush() (numRows uint64, numRowGroups uint64, err error)
	Close() error
}

type Head struct {
	logger  log.Logger
	metrics *headMetrics
	stopCh  chan struct{}
	wg      sync.WaitGroup

	headPath  string // path while block is actively appended to
	localPath string // path once block has been cut

	flushCh chan struct{} // this channel is closed once the Head should be flushed, should be used externally

	flushForcedTimer *time.Timer // this timer will phlare after the maximum

	metaLock sync.RWMutex
	meta     *block.Meta

	index           *profilesIndex
	parquetConfig   *ParquetConfig
	strings         deduplicatingSlice[string, string, *stringsHelper, *schemav1.StringPersister]
	mappings        deduplicatingSlice[*profilev1.Mapping, mappingsKey, *mappingsHelper, *schemav1.MappingPersister]
	functions       deduplicatingSlice[*profilev1.Function, functionsKey, *functionsHelper, *schemav1.FunctionPersister]
	locations       deduplicatingSlice[*profilev1.Location, locationsKey, *locationsHelper, *schemav1.LocationPersister]
	stacktraces     deduplicatingSlice[*schemav1.Stacktrace, stacktracesKey, *stacktracesHelper, *schemav1.StacktracePersister] // a stacktrace is a slice of location ids
	profiles        deduplicatingSlice[*schemav1.Profile, noKey, *profilesHelper, *schemav1.ProfilePersister]
	totalSamples    *atomic.Uint64
	tables          []Table
	delta           *deltaProfiles
	pprofLabelCache labelCache
}

const (
	pathHead          = "head"
	pathLocal         = "local"
	defaultFolderMode = 0o755
)

func NewHead(phlarectx context.Context, cfg Config) (*Head, error) {
	h := &Head{
		logger:  phlarecontext.Logger(phlarectx),
		metrics: contextHeadMetrics(phlarectx),

		stopCh: make(chan struct{}),

		meta:         block.NewMeta(),
		totalSamples: atomic.NewUint64(0),

		flushCh:          make(chan struct{}),
		flushForcedTimer: time.NewTimer(cfg.MaxBlockDuration),

		parquetConfig: defaultParquetConfig,
	}
	h.headPath = filepath.Join(cfg.DataPath, pathHead, h.meta.ULID.String())
	h.localPath = filepath.Join(cfg.DataPath, pathLocal, h.meta.ULID.String())
	h.metrics.setHead(h)

	if cfg.Parquet != nil {
		h.parquetConfig = cfg.Parquet
	}

	if err := os.MkdirAll(h.headPath, defaultFolderMode); err != nil {
		return nil, err
	}

	h.tables = []Table{
		&h.strings,
		&h.mappings,
		&h.functions,
		&h.locations,
		&h.stacktraces,
		&h.profiles,
	}
	for _, t := range h.tables {
		if err := t.Init(h.headPath, h.parquetConfig); err != nil {
			return nil, err
		}
	}

	index, err := newProfileIndex(32, h.metrics)
	if err != nil {
		return nil, err
	}
	h.index = index
	h.delta = newDeltaProfiles()

	h.pprofLabelCache.init()

	h.wg.Add(1)
	go h.loop()

	return h, nil
}

func (h *Head) Size() uint64 {
	var size uint64
	// TODO: Estimate size of TSDB index
	for _, t := range h.tables {
		size += t.Size()
	}

	return size
}

func (h *Head) loop() {
	defer h.wg.Done()

	tick := time.NewTicker(5 * time.Second)
	defer func() {
		tick.Stop()
		h.flushForcedTimer.Stop()
	}()

	for {
		select {
		case <-h.flushForcedTimer.C:
			level.Debug(h.logger).Log("msg", "max block duration reached, flush to disk")
			close(h.flushCh)
			return
		case <-tick.C:
			if currentSize := h.Size(); currentSize > h.parquetConfig.MaxBlockBytes {
				level.Debug(h.logger).Log(
					"msg", "max block bytes reached, flush to disk",
					"max_size", humanize.Bytes(h.parquetConfig.MaxBlockBytes),
					"current_head_size", humanize.Bytes(currentSize),
				)
				close(h.flushCh)
				return
			}
		case <-h.stopCh:
			return
		}
	}
}

func (h *Head) convertSamples(ctx context.Context, r *rewriter, in []*profilev1.Sample) ([][]*schemav1.Sample, error) {
	if len(in) == 0 {
		return nil, nil
	}

	// populate output
	var (
		out         = make([][]*schemav1.Sample, len(in[0].Value))
		stacktraces = make([]*schemav1.Stacktrace, len(in))
	)
	for idxType := range out {
		out[idxType] = make([]*schemav1.Sample, len(in))
	}

	for idxSample := range in {
		// populate samples
		labels := h.pprofLabelCache.rewriteLabels(r.strings, in[idxSample].Label)
		for idxType := range out {
			out[idxType][idxSample] = &schemav1.Sample{
				Value:  in[idxSample].Value[idxType],
				Labels: labels,
			}
		}

		// build full stack traces
		stacktraces[idxSample] = &schemav1.Stacktrace{
			// no copySlice necessary at this point,stacktracesHelper.clone
			// will copy it, if it is required to be retained.
			LocationIDs: in[idxSample].LocationId,
		}
	}

	// ingest stacktraces
	if err := h.stacktraces.ingest(ctx, stacktraces, r); err != nil {
		return nil, err
	}

	// reference stacktraces
	for idxType := range out {
		for idxSample := range out[idxType] {
			out[idxType][idxSample].StacktraceID = uint64(r.stacktraces[int64(idxSample)])
		}
	}

	return out, nil
}

func (h *Head) Ingest(ctx context.Context, p *profilev1.Profile, id uuid.UUID, externalLabels ...*typesv1.LabelPair) error {
	metricName := phlaremodel.Labels(externalLabels).Get(model.MetricNameLabel)
	labels, seriesFingerprints := labelsForProfile(p, externalLabels...)

	// create a rewriter state
	rewrites := &rewriter{}

	if err := h.strings.ingest(ctx, p.StringTable, rewrites); err != nil {
		return err
	}

	if err := h.mappings.ingest(ctx, p.Mapping, rewrites); err != nil {
		return err
	}

	if err := h.functions.ingest(ctx, p.Function, rewrites); err != nil {
		return err
	}

	if err := h.locations.ingest(ctx, p.Location, rewrites); err != nil {
		return err
	}

	samplesPerType, err := h.convertSamples(ctx, rewrites, p.Sample)
	if err != nil {
		return err
	}

	var profileIngested bool
	for idxType := range samplesPerType {
		profile := &schemav1.Profile{
			ID:                id,
			SeriesFingerprint: seriesFingerprints[idxType],
			Samples:           samplesPerType[idxType],
			DropFrames:        p.DropFrames,
			KeepFrames:        p.KeepFrames,
			TimeNanos:         p.TimeNanos,
			DurationNanos:     p.DurationNanos,
			Comments:          copySlice(p.Comment),
			DefaultSampleType: p.DefaultSampleType,
		}

		profile = h.delta.computeDelta(profile, labels[idxType])

		if profile == nil {
			continue
		}

		if err := h.profiles.ingest(ctx, []*schemav1.Profile{profile}, rewrites); err != nil {
			return err
		}

		h.index.Add(profile, labels[idxType], metricName)

		profileIngested = true
	}

	if !profileIngested {
		return nil
	}

	h.metaLock.Lock()
	v := model.TimeFromUnixNano(p.TimeNanos)
	if v < h.meta.MinTime {
		h.meta.MinTime = v
	}
	if v > h.meta.MaxTime {
		h.meta.MaxTime = v
	}
	h.metaLock.Unlock()

	samplesInProfile := len(samplesPerType[0]) * len(labels)
	h.totalSamples.Add(sampleSize)
	h.metrics.sampleValuesIngested.WithLabelValues(metricName).Add(float64(samplesInProfile))
	h.metrics.sampleValuesReceived.WithLabelValues(metricName).Add(float64(len(p.Sample) * len(labels)))

	return nil
}

func labelsForProfile(p *profilev1.Profile, externalLabels ...*typesv1.LabelPair) ([]phlaremodel.Labels, []model.Fingerprint) {
	// build label set per sample type before references are rewritten
	var (
		sb                                             strings.Builder
		lbls                                           = phlaremodel.NewLabelsBuilder(externalLabels)
		sampleType, sampleUnit, periodType, periodUnit string
		metricName                                     = phlaremodel.Labels(externalLabels).Get(model.MetricNameLabel)
	)

	// set common labels
	if p.PeriodType != nil {
		periodType = p.StringTable[p.PeriodType.Type]
		lbls.Set(phlaremodel.LabelNamePeriodType, periodType)
		periodUnit = p.StringTable[p.PeriodType.Unit]
		lbls.Set(phlaremodel.LabelNamePeriodUnit, periodUnit)
	}

	profilesLabels := make([]phlaremodel.Labels, len(p.SampleType))
	seriesRefs := make([]model.Fingerprint, len(p.SampleType))
	for pos := range p.SampleType {
		sampleType = p.StringTable[p.SampleType[pos].Type]
		lbls.Set(phlaremodel.LabelNameType, sampleType)
		sampleUnit = p.StringTable[p.SampleType[pos].Unit]
		lbls.Set(phlaremodel.LabelNameUnit, sampleUnit)

		sb.Reset()
		_, _ = sb.WriteString(metricName)
		_, _ = sb.WriteRune(':')
		_, _ = sb.WriteString(sampleType)
		_, _ = sb.WriteRune(':')
		_, _ = sb.WriteString(sampleUnit)
		_, _ = sb.WriteRune(':')
		_, _ = sb.WriteString(periodType)
		_, _ = sb.WriteRune(':')
		_, _ = sb.WriteString(periodUnit)
		t := sb.String()
		lbls.Set(phlaremodel.LabelNameProfileType, t)
		lbs := lbls.Labels().Clone()
		profilesLabels[pos] = lbs
		seriesRefs[pos] = model.Fingerprint(lbs.Hash())

	}
	return profilesLabels, seriesRefs
}

// LabelValues returns the possible label values for a given label name.
func (h *Head) LabelValues(ctx context.Context, req *connect.Request[ingestv1.LabelValuesRequest]) (*connect.Response[ingestv1.LabelValuesResponse], error) {
	values, err := h.index.ix.LabelValues(req.Msg.Name, nil)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ingestv1.LabelValuesResponse{
		Names: values,
	}), nil
}

// LabelValues returns the possible label values for a given label name.
func (h *Head) LabelNames(ctx context.Context, req *connect.Request[ingestv1.LabelNamesRequest]) (*connect.Response[ingestv1.LabelNamesResponse], error) {
	values, err := h.index.ix.LabelNames(nil)
	if err != nil {
		return nil, err
	}
	sort.Strings(values)
	return connect.NewResponse(&ingestv1.LabelNamesResponse{
		Names: values,
	}), nil
}

// ProfileTypes returns the possible profile types.
func (h *Head) ProfileTypes(ctx context.Context, req *connect.Request[ingestv1.ProfileTypesRequest]) (*connect.Response[ingestv1.ProfileTypesResponse], error) {
	values, err := h.index.ix.LabelValues(phlaremodel.LabelNameProfileType, nil)
	if err != nil {
		return nil, err
	}
	sort.Strings(values)

	profileTypes := make([]*typesv1.ProfileType, len(values))
	for i, v := range values {
		tp, err := phlaremodel.ParseProfileTypeSelector(v)
		if err != nil {
			return nil, err
		}
		profileTypes[i] = tp
	}

	return connect.NewResponse(&ingestv1.ProfileTypesResponse{
		ProfileTypes: profileTypes,
	}), nil
}

func (h *Head) InRange(start, end model.Time) bool {
	h.metaLock.RLock()
	b := &minMax{
		min: h.meta.MinTime,
		max: h.meta.MaxTime,
	}
	h.metaLock.RUnlock()
	return b.InRange(start, end)
}

func (h *Head) SelectMatchingProfiles(ctx context.Context, params *ingestv1.SelectProfilesRequest) (iter.Iterator[Profile], error) {
	sp, _ := opentracing.StartSpanFromContext(ctx, "SelectMatchingProfiles - Head")
	defer sp.Finish()
	selectors, err := parser.ParseMetricSelector(params.LabelSelector)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "failed to parse label selectors: "+err.Error())
	}
	selectors = append(selectors, phlaremodel.SelectorFromProfileType(params.Type))
	return h.index.SelectProfiles(selectors, model.Time(params.Start), model.Time(params.End))
}

func (h *Head) MergeByStacktraces(ctx context.Context, rows iter.Iterator[Profile]) (*ingestv1.MergeProfilesStacktracesResult, error) {
	sp, _ := opentracing.StartSpanFromContext(ctx, "MergeByStacktraces - Head")
	defer sp.Finish()

	stacktraceSamples := map[uint64]*ingestv1.StacktraceSample{}
	names := []string{}
	functions := map[int64]int{}

	defer rows.Close()

	h.stacktraces.lock.RLock()
	h.locations.lock.RLock()
	h.functions.lock.RLock()
	h.strings.lock.RLock()
	defer func() {
		h.stacktraces.lock.RUnlock()
		h.locations.lock.RUnlock()
		h.functions.lock.RUnlock()
		h.strings.lock.RUnlock()
	}()

	for rows.Next() {
		p, ok := rows.At().(ProfileWithLabels)
		if !ok {
			return nil, errors.New("expected ProfileWithLabels")
		}
		for _, s := range p.Samples {
			if s.Value == 0 {
				continue
			}
			existing, ok := stacktraceSamples[s.StacktraceID]
			if ok {
				existing.Value += s.Value
				continue
			}
			locs := h.stacktraces.slice[s.StacktraceID].LocationIDs
			fnIds := make([]int32, 0, 2*len(locs))
			for _, loc := range locs {
				for _, line := range h.locations.slice[loc].Line {
					fnNameID := h.functions.slice[line.FunctionId].Name
					pos, ok := functions[fnNameID]
					if !ok {
						functions[fnNameID] = len(names)
						fnIds = append(fnIds, int32(len(names)))
						names = append(names, h.strings.slice[h.functions.slice[line.FunctionId].Name])
						continue
					}
					fnIds = append(fnIds, int32(pos))
				}
			}
			stacktraceSamples[s.StacktraceID] = &ingestv1.StacktraceSample{
				FunctionIds: fnIds,
				Value:       s.Value,
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &ingestv1.MergeProfilesStacktracesResult{
		Stacktraces:   lo.Values(stacktraceSamples),
		FunctionNames: names,
	}, nil
}

func (h *Head) MergeByLabels(ctx context.Context, rows iter.Iterator[Profile], by ...string) ([]*typesv1.Series, error) {
	sp, _ := opentracing.StartSpanFromContext(ctx, "MergeByLabels - Head")
	defer sp.Finish()

	labelsByFingerprint := map[model.Fingerprint]string{}
	seriesByLabels := map[string]*typesv1.Series{}
	labelBuf := make([]byte, 0, 1024)
	defer rows.Close()

	for rows.Next() {
		p, ok := rows.At().(ProfileWithLabels)
		if !ok {
			return nil, errors.New("expected ProfileWithLabels")
		}
		labelsByString, ok := labelsByFingerprint[p.fp]
		if !ok {
			labelBuf = p.Labels().BytesWithLabels(labelBuf, by...)
			labelsByString = string(labelBuf)
			labelsByFingerprint[p.fp] = labelsByString
			if _, ok := seriesByLabels[labelsByString]; !ok {
				seriesByLabels[labelsByString] = &typesv1.Series{
					Labels: p.Labels().WithLabels(by...),
					Points: []*typesv1.Point{
						{
							Timestamp: int64(p.Timestamp()),
							Value:     float64(p.Total()),
						},
					},
				}
				continue
			}
		}
		series := seriesByLabels[labelsByString]
		series.Points = append(series.Points, &typesv1.Point{
			Timestamp: int64(p.Timestamp()),
			Value:     float64(p.Total()),
		})

	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := lo.Values(seriesByLabels)
	sort.Slice(result, func(i, j int) bool {
		return phlaremodel.CompareLabelPairs(result[i].Labels, result[j].Labels) < 0
	})
	// we have to sort the points in each series because labels reduction may have changed the order
	for _, s := range result {
		sort.Slice(s.Points, func(i, j int) bool {
			return s.Points[i].Timestamp < s.Points[j].Timestamp
		})
	}
	return result, nil
}

func (h *Head) Sort(in []Profile) []Profile {
	return in
}

type ProfileSelectorIterator struct {
	batch   chan []Profile
	current iter.Iterator[Profile]
	once    sync.Once
}

func NewProfileSelectorIterator() *ProfileSelectorIterator {
	return &ProfileSelectorIterator{
		batch: make(chan []Profile, 1),
	}
}

func (it *ProfileSelectorIterator) Push(batch []Profile) {
	if len(batch) == 0 {
		return
	}
	it.batch <- batch
}

func (it *ProfileSelectorIterator) Next() bool {
	if it.current == nil {
		batch, ok := <-it.batch
		if !ok {
			return false
		}
		it.current = iter.NewSliceIterator(batch)
	}
	if !it.current.Next() {
		it.current = nil
		return it.Next()
	}
	return true
}

func (it *ProfileSelectorIterator) At() Profile {
	if it.current == nil {
		return ProfileWithLabels{}
	}
	return it.current.At()
}

func (it *ProfileSelectorIterator) Close() error {
	it.once.Do(func() {
		close(it.batch)
	})
	return nil
}

func (it *ProfileSelectorIterator) Err() error {
	return nil
}

func (h *Head) Series(ctx context.Context, req *connect.Request[ingestv1.SeriesRequest]) (*connect.Response[ingestv1.SeriesResponse], error) {
	selectors := make([][]*labels.Matcher, 0, len(req.Msg.Matchers))
	for _, m := range req.Msg.Matchers {
		s, err := parser.ParseMetricSelector(m)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "failed to label selector")
		}
		selectors = append(selectors, s)
	}
	response := &ingestv1.SeriesResponse{}
	uniqu := map[model.Fingerprint]struct{}{}
	for _, selector := range selectors {
		if err := h.index.forMatchingLabels(selector, func(lbs phlaremodel.Labels, fp model.Fingerprint) error {
			if _, ok := uniqu[fp]; ok {
				return nil
			}
			uniqu[fp] = struct{}{}
			response.LabelsSet = append(response.LabelsSet, &typesv1.Labels{Labels: lbs})
			return nil
		}); err != nil {
			return nil, err
		}
	}
	sort.Slice(response.LabelsSet, func(i, j int) bool {
		return phlaremodel.CompareLabelPairs(response.LabelsSet[i].Labels, response.LabelsSet[j].Labels) < 0
	})
	return connect.NewResponse(response), nil
}

// Flush closes the head and writes data to disk
func (h *Head) Close() error {
	close(h.stopCh)

	var merr multierror.MultiError
	for _, t := range h.tables {
		merr.Add(t.Close())
	}

	h.wg.Wait()
	return merr.Err()
}

// Flush closes the head and writes data to disk
func (h *Head) Flush(ctx context.Context) error {
	if len(h.profiles.slice) == 0 {
		level.Info(h.logger).Log("msg", "head empty - no block written")
		return os.RemoveAll(h.headPath)
	}

	files := make([]block.File, len(h.tables)+1)

	// write index
	indexPath := filepath.Join(h.headPath, block.IndexFilename)
	if err := h.index.WriteTo(ctx, indexPath); err != nil {
		return errors.Wrap(err, "flushing of index")
	}
	files[0].RelPath = block.IndexFilename
	h.meta.Stats.NumSeries = uint64(h.index.totalSeries.Load())
	files[0].TSDB = &block.TSDBFile{
		NumSeries: h.meta.Stats.NumSeries,
	}

	// add index file size
	if stat, err := os.Stat(indexPath); err == nil {
		files[0].SizeBytes = uint64(stat.Size())
	}

	for idx, t := range h.tables {
		numRows, numRowGroups, err := t.Flush()
		if err != nil {
			return errors.Wrapf(err, "flushing of table %s", t.Name())
		}
		h.metrics.rowsWritten.WithLabelValues(t.Name()).Add(float64(numRows))
		files[idx+1].Parquet = &block.ParquetFile{
			NumRowGroups: numRowGroups,
			NumRows:      numRows,
		}
	}

	for idx, t := range h.tables {
		if err := t.Close(); err != nil {
			return errors.Wrapf(err, "closing of table %s", t.Name())
		}

		// add file size
		files[idx+1].RelPath = t.Name() + block.ParquetSuffix
		if stat, err := os.Stat(filepath.Join(h.headPath, files[idx+1].RelPath)); err == nil {
			files[idx+1].SizeBytes = uint64(stat.Size())
		}
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].RelPath < files[j].RelPath
	})
	h.meta.Files = files
	h.meta.Stats.NumProfiles = uint64(h.index.totalProfiles.Load())
	h.meta.Stats.NumSamples = h.totalSamples.Load()

	if _, err := h.meta.WriteToFile(h.logger, h.headPath); err != nil {
		return err
	}

	// move block to the local directory
	if err := os.MkdirAll(filepath.Dir(h.localPath), defaultFolderMode); err != nil {
		return err
	}
	if err := fileutil.Rename(h.headPath, h.localPath); err != nil {
		return err
	}

	level.Info(h.logger).Log("msg", "head successfully written to block", "block_path", h.localPath)

	return nil
}