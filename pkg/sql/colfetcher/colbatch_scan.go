// Copyright 2016 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package colfetcher

import (
	"context"
	"sync"
	"time"

	"github.com/cockroachdb/cockroach/pkg/col/coldata"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/tabledesc"
	"github.com/cockroachdb/cockroach/pkg/sql/colexecerror"
	"github.com/cockroachdb/cockroach/pkg/sql/colexecop"
	"github.com/cockroachdb/cockroach/pkg/sql/colmem"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/rowinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/mon"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/errors"
)

// TODO(yuzefovich): reading the data through a pair of ColBatchScan and
// materializer turns out to be more efficient than through a table reader (at
// the moment, the exception is the case of reading very small number of rows
// because we still pre-allocate batches of 1024 size). Once we can control the
// initial size of pre-allocated batches (probably via a batch allocator), we
// should get rid off table readers entirely. We will have to be careful about
// propagating the metadata though.

// ColBatchScan is the exec.Operator implementation of TableReader. It reads a
// table from kv, presenting it as coldata.Batches via the exec.Operator
// interface.
type ColBatchScan struct {
	colexecop.ZeroInputNode
	colexecop.InitHelper
	execinfra.SpansWithCopy

	flowCtx         *execinfra.FlowCtx
	bsHeader        *roachpb.BoundedStalenessHeader
	rf              *cFetcher
	limitHint       rowinfra.RowLimit
	batchBytesLimit rowinfra.BytesLimit
	parallelize     bool
	// tracingSpan is created when the stats should be collected for the query
	// execution, and it will be finished when closing the operator.
	tracingSpan *tracing.Span
	mu          struct {
		syncutil.Mutex
		// rowsRead contains the number of total rows this ColBatchScan has
		// returned so far.
		rowsRead int64
	}
	// ResultTypes is the slice of resulting column types from this operator.
	// It should be used rather than the slice of column types from the scanned
	// table because the scan might synthesize additional implicit system columns.
	ResultTypes []*types.T
}

// ScanOperator combines common interfaces between operators that perform KV
// scans, such as ColBatchScan and ColIndexJoin.
type ScanOperator interface {
	colexecop.KVReader
	execinfra.Releasable
	colexecop.ClosableOperator
}

var _ ScanOperator = &ColBatchScan{}

// Init initializes a ColBatchScan.
func (s *ColBatchScan) Init(ctx context.Context) {
	if !s.InitHelper.Init(ctx) {
		return
	}
	// If tracing is enabled, we need to start a child span so that the only
	// contention events present in the recording would be because of this
	// cFetcher. Note that ProcessorSpan method itself will check whether
	// tracing is enabled.
	s.Ctx, s.tracingSpan = execinfra.ProcessorSpan(s.Ctx, "colbatchscan")
	limitBatches := !s.parallelize
	if err := s.rf.StartScan(
		s.Ctx,
		s.flowCtx.Txn,
		s.Spans,
		s.bsHeader,
		limitBatches,
		s.batchBytesLimit,
		s.limitHint,
		s.flowCtx.EvalCtx.TestingKnobs.ForceProductionBatchSizes,
	); err != nil {
		colexecerror.InternalError(err)
	}
}

// Next is part of the Operator interface.
func (s *ColBatchScan) Next() coldata.Batch {
	bat, err := s.rf.NextBatch(s.Ctx)
	if err != nil {
		colexecerror.InternalError(err)
	}
	if bat.Selection() != nil {
		colexecerror.InternalError(errors.AssertionFailedf("unexpectedly a selection vector is set on the batch coming from CFetcher"))
	}
	s.mu.Lock()
	s.mu.rowsRead += int64(bat.Length())
	s.mu.Unlock()
	return bat
}

// DrainMeta is part of the colexecop.MetadataSource interface.
func (s *ColBatchScan) DrainMeta() []execinfrapb.ProducerMetadata {
	var trailingMeta []execinfrapb.ProducerMetadata
	if !s.flowCtx.Local {
		nodeID, ok := s.flowCtx.NodeID.OptionalNodeID()
		if ok {
			ranges := execinfra.MisplannedRanges(s.Ctx, s.SpansCopy, nodeID, s.flowCtx.Cfg.RangeCache)
			if ranges != nil {
				trailingMeta = append(trailingMeta, execinfrapb.ProducerMetadata{Ranges: ranges})
			}
		}
	}
	if tfs := execinfra.GetLeafTxnFinalState(s.Ctx, s.flowCtx.Txn); tfs != nil {
		trailingMeta = append(trailingMeta, execinfrapb.ProducerMetadata{LeafTxnFinalState: tfs})
	}
	meta := execinfrapb.GetProducerMeta()
	meta.Metrics = execinfrapb.GetMetricsMeta()
	meta.Metrics.BytesRead = s.GetBytesRead()
	meta.Metrics.RowsRead = s.GetRowsRead()
	trailingMeta = append(trailingMeta, *meta)
	if trace := execinfra.GetTraceData(s.Ctx); trace != nil {
		trailingMeta = append(trailingMeta, execinfrapb.ProducerMetadata{TraceData: trace})
	}
	return trailingMeta
}

// GetBytesRead is part of the colexecop.KVReader interface.
func (s *ColBatchScan) GetBytesRead() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Note that if Init() was never called, s.rf.fetcher will remain nil, and
	// GetBytesRead() will return 0. We are also holding the mutex, so a
	// concurrent call to Init() will have to wait, and the fetcher will remain
	// uninitialized until we return.
	return s.rf.fetcher.GetBytesRead()
}

// GetRowsRead is part of the colexecop.KVReader interface.
func (s *ColBatchScan) GetRowsRead() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mu.rowsRead
}

// GetCumulativeContentionTime is part of the colexecop.KVReader interface.
func (s *ColBatchScan) GetCumulativeContentionTime() time.Duration {
	return execinfra.GetCumulativeContentionTime(s.Ctx)
}

// GetScanStats is part of the colexecop.KVReader interface.
func (s *ColBatchScan) GetScanStats() execinfra.ScanStats {
	return execinfra.GetScanStats(s.Ctx)
}

var colBatchScanPool = sync.Pool{
	New: func() interface{} {
		return &ColBatchScan{}
	},
}

// NewColBatchScan creates a new ColBatchScan operator.
func NewColBatchScan(
	ctx context.Context,
	allocator *colmem.Allocator,
	kvFetcherMemAcc *mon.BoundAccount,
	flowCtx *execinfra.FlowCtx,
	evalCtx *tree.EvalContext,
	spec *execinfrapb.TableReaderSpec,
	post *execinfrapb.PostProcessSpec,
	estimatedRowCount uint64,
) (*ColBatchScan, error) {
	// NB: we hit this with a zero NodeID (but !ok) with multi-tenancy.
	if nodeID, ok := flowCtx.NodeID.OptionalNodeID(); nodeID == 0 && ok {
		return nil, errors.Errorf("attempting to create a ColBatchScan with uninitialized NodeID")
	}
	if spec.IsCheck {
		// cFetchers don't support these checks.
		return nil, errors.AssertionFailedf("attempting to create a cFetcher with the IsCheck flag set")
	}

	limitHint := rowinfra.RowLimit(execinfra.LimitHint(spec.LimitHint, post))
	// TODO(ajwerner): The need to construct an immutable here
	// indicates that we're probably doing this wrong. Instead we should be
	// just setting the ID and Version in the spec or something like that and
	// retrieving the hydrated immutable from cache.
	table := spec.BuildTableDescriptor()
	invertedColumn := tabledesc.FindInvertedColumn(table, spec.InvertedColumn)
	tableArgs, err := populateTableArgs(
		ctx, flowCtx, evalCtx, table, table.ActiveIndexes()[spec.IndexIdx],
		invertedColumn, spec.Visibility, spec.HasSystemColumns,
	)
	if err != nil {
		return nil, err
	}

	for _, neededColumn := range spec.NeededColumns {
		tableArgs.ValNeededForCol.Add(int(neededColumn))
	}

	fetcher := cFetcherPool.Get().(*cFetcher)
	fetcher.cFetcherArgs = cFetcherArgs{
		spec.LockingStrength,
		spec.LockingWaitPolicy,
		flowCtx.EvalCtx.SessionData().LockTimeout,
		execinfra.GetWorkMemLimit(flowCtx),
		estimatedRowCount,
		spec.Reverse,
		flowCtx.TraceKV,
	}

	if err = fetcher.Init(flowCtx.Codec(), allocator, kvFetcherMemAcc, tableArgs); err != nil {
		fetcher.Release()
		return nil, err
	}

	var bsHeader *roachpb.BoundedStalenessHeader
	if aost := evalCtx.AsOfSystemTime; aost != nil && aost.BoundedStaleness {
		ts := aost.Timestamp
		// If the descriptor's modification time is after the bounded staleness min bound,
		// we have to increase the min bound.
		// Otherwise, we would have table data which would not correspond to the correct
		// schema.
		if aost.Timestamp.Less(table.GetModificationTime()) {
			ts = table.GetModificationTime()
		}
		bsHeader = &roachpb.BoundedStalenessHeader{
			MinTimestampBound:       ts,
			MinTimestampBoundStrict: aost.NearestOnly,
			MaxTimestampBound:       evalCtx.AsOfSystemTime.MaxTimestampBound, // may be empty
		}
	}

	s := colBatchScanPool.Get().(*ColBatchScan)
	s.Spans = s.Spans[:0]
	specSpans := spec.Spans
	for i := range specSpans {
		//gcassert:bce
		s.Spans = append(s.Spans, specSpans[i].Span)
	}
	if !flowCtx.Local {
		// Make a copy of the spans so that we could get the misplanned ranges
		// info.
		allocator.AdjustMemoryUsage(s.Spans.MemUsage())
		s.MakeSpansCopy()
	}

	if spec.LimitHint > 0 || spec.BatchBytesLimit > 0 {
		// Parallelize shouldn't be set when there's a limit hint, but double-check
		// just in case.
		spec.Parallelize = false
	}
	var batchBytesLimit rowinfra.BytesLimit
	if !spec.Parallelize {
		batchBytesLimit = rowinfra.BytesLimit(spec.BatchBytesLimit)
		if batchBytesLimit == 0 {
			batchBytesLimit = rowinfra.DefaultBatchBytesLimit
		}
	}

	*s = ColBatchScan{
		SpansWithCopy:   s.SpansWithCopy,
		flowCtx:         flowCtx,
		bsHeader:        bsHeader,
		rf:              fetcher,
		limitHint:       limitHint,
		batchBytesLimit: batchBytesLimit,
		parallelize:     spec.Parallelize,
		ResultTypes:     tableArgs.typs,
	}
	return s, nil
}

type cFetcherTableArgs struct {
	desc  catalog.TableDescriptor
	index catalog.Index
	// ColIdxMap is a mapping from ColumnID of each column to its ordinal.
	ColIdxMap        catalog.TableColMap
	isSecondaryIndex bool
	// cols are all columns of the table.
	cols []catalog.Column
	// The indexes (0 to # of columns - 1) of the columns to return.
	ValNeededForCol util.FastIntSet
	// typs are types of all columns of the table.
	typs []*types.T
}

var cFetcherTableArgsPool = sync.Pool{
	New: func() interface{} {
		return &cFetcherTableArgs{}
	},
}

func (a *cFetcherTableArgs) Release() {
	oldCols := a.cols
	for i := range oldCols {
		oldCols[i] = nil
	}
	*a = cFetcherTableArgs{
		// The types are small objects, so we don't bother deeply resetting this
		// slice.
		typs: a.typs[:0],
	}
	a.cols = oldCols[:0]
	cFetcherTableArgsPool.Put(a)
}

// populateTableArgs fills all fields of the cFetcherTableArgs except for
// ValNeededForCol.
func populateTableArgs(
	ctx context.Context,
	flowCtx *execinfra.FlowCtx,
	evalCtx *tree.EvalContext,
	table catalog.TableDescriptor,
	index catalog.Index,
	invertedCol catalog.Column,
	visibility execinfrapb.ScanVisibility,
	hasSystemColumns bool,
) (*cFetcherTableArgs, error) {
	args := cFetcherTableArgsPool.Get().(*cFetcherTableArgs)
	cols := args.cols[:0]
	if visibility == execinfra.ScanVisibilityPublicAndNotPublic {
		cols = append(cols, table.ReadableColumns()...)
	} else {
		cols = append(cols, table.PublicColumns()...)
	}
	if invertedCol != nil {
		for i, col := range cols {
			if col.GetID() == invertedCol.GetID() {
				cols[i] = invertedCol
				break
			}
		}
	}
	if hasSystemColumns {
		cols = append(cols, table.SystemColumns()...)
	}

	*args = cFetcherTableArgs{
		desc:             table,
		index:            index,
		ColIdxMap:        catalog.ColumnIDToOrdinalMap(cols),
		isSecondaryIndex: !index.Primary(),
		cols:             cols,
	}
	if cap(args.typs) < len(cols) {
		args.typs = make([]*types.T, len(cols))
	} else {
		args.typs = args.typs[:len(cols)]
	}
	for i := range cols {
		args.typs[i] = cols[i].GetType()
	}

	// Before we can safely use types from the table descriptor, we need to
	// make sure they are hydrated. In row execution engine it is done during
	// the processor initialization, but neither ColBatchScan nor cFetcher are
	// processors, so we need to do the hydration ourselves.
	resolver := flowCtx.TypeResolverFactory.NewTypeResolver(evalCtx.Txn)
	return args, resolver.HydrateTypeSlice(ctx, args.typs)
}

// Release implements the execinfra.Releasable interface.
func (s *ColBatchScan) Release() {
	s.rf.Release()
	// Deeply reset the spans so that we don't hold onto the keys of the spans.
	s.SpansWithCopy.Reset()
	*s = ColBatchScan{
		SpansWithCopy: s.SpansWithCopy,
	}
	colBatchScanPool.Put(s)
}

// Close implements the colexecop.Closer interface.
func (s *ColBatchScan) Close() error {
	s.rf.Close(s.EnsureCtx())
	if s.tracingSpan != nil {
		s.tracingSpan.Finish()
		s.tracingSpan = nil
	}
	return nil
}
