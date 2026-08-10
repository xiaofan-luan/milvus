package writebuffer

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"

	"github.com/milvus-io/milvus-proto/go-api/v3/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v3/msgpb"
	"github.com/milvus-io/milvus-proto/go-api/v3/schemapb"
	"github.com/milvus-io/milvus/internal/allocator"
	"github.com/milvus-io/milvus/internal/flushcommon/metacache"
	"github.com/milvus-io/milvus/internal/flushcommon/metacache/pkoracle"
	"github.com/milvus-io/milvus/internal/flushcommon/syncmgr"
	"github.com/milvus-io/milvus/internal/storage"
	"github.com/milvus-io/milvus/internal/storagev2/packed"
	"github.com/milvus-io/milvus/pkg/v3/common"
	"github.com/milvus-io/milvus/pkg/v3/metrics"
	"github.com/milvus-io/milvus/pkg/v3/mq/msgstream"
	"github.com/milvus-io/milvus/pkg/v3/objectstorage"
	"github.com/milvus-io/milvus/pkg/v3/proto/datapb"
	"github.com/milvus-io/milvus/pkg/v3/util/conc"
	"github.com/milvus-io/milvus/pkg/v3/util/paramtable"
	"github.com/milvus-io/milvus/pkg/v3/util/testutils"
	"github.com/milvus-io/milvus/pkg/v3/util/tsoutil"
)

type L0WriteBufferSuite struct {
	testutils.PromMetricsSuite
	channelName string
	collID      int64
	collSchema  *schemapb.CollectionSchema
	pkSchema    *schemapb.FieldSchema
	syncMgr     *syncmgr.MockSyncManager
	metacache   *metacache.MockMetaCache
	allocator   *allocator.MockGIDAllocator
}

type fakeGrowingFlushSource struct {
	// tsafe is the consumption watermark this double reports. Zero means "far
	// ahead of anything the tests fence on", which is the usual case; a test
	// that wants the source to look behind sets it explicitly.
	tsafe uint64
	rows  int64
	// pkRows, when set, decides how many primary keys each call returns. Needed
	// where consecutive flushes carry different row counts: the fences are
	// timestamps, so the double cannot derive the count from them.
	pkRows      func() int64
	flushFunc   func(context.Context, uint64, uint64, *syncmgr.GrowingFlushConfig) (*syncmgr.GrowingFlushResult, error)
	releaseFunc func()
	commitFunc  func(uint64)
}

func runGrowingTaskWithCallbacks(ctx context.Context, task *syncmgr.GrowingSourceSyncTask, callbacks []func(error) error) error {
	err := runSyncTaskInline(ctx, task)
	if err != nil {
		task.HandleError(err)
	}
	for _, callback := range callbacks {
		err = callback(err)
	}
	return err
}

func (s fakeGrowingFlushSource) TSafe() uint64 {
	if s.tsafe > 0 {
		return s.tsafe
	}
	return math.MaxUint64
}

func (s fakeGrowingFlushSource) MaterializedFieldIDs(ctx context.Context) ([]int64, error) {
	return []int64{0, 1, 100, 101, 102}, nil
}

func (s fakeGrowingFlushSource) PrimaryKeys(ctx context.Context, startTs, endTs uint64) ([]storage.PrimaryKey, error) {
	rows := s.rows
	if s.pkRows != nil {
		rows = s.pkRows()
	}
	if rows == 0 {
		rows = 10
	}
	pks := make([]storage.PrimaryKey, 0, rows)
	for i := int64(0); i < rows; i++ {
		pks = append(pks, storage.NewInt64PrimaryKey(i))
	}
	return pks, nil
}

func (s fakeGrowingFlushSource) FlushGrowingData(ctx context.Context, startTs, endTs uint64, config *syncmgr.GrowingFlushConfig) (*syncmgr.GrowingFlushResult, error) {
	if s.flushFunc != nil {
		return s.flushFunc(ctx, startTs, endTs, config)
	}
	return &syncmgr.GrowingFlushResult{
		ManifestPath:           "manifest",
		NumRows:                10,
		TimestampFrom:          100,
		TimestampTo:            200,
		ColumnGroupMemorySizes: fakeColumnGroupMemorySizes(config, 80),
		FieldNullCounts:        map[int64]int64{},
	}, nil
}

func (s fakeGrowingFlushSource) Release() {
	if s.releaseFunc != nil {
		s.releaseFunc()
	}
}

func (s fakeGrowingFlushSource) CommitGrowingFlush(flushThroughTs uint64) {
	if s.commitFunc != nil {
		s.commitFunc(flushThroughTs)
	}
}

type fakeGrowingSourceProvider struct {
	source syncmgr.GrowingFlushSource
	state  syncmgr.GrowingSourceState
}

func fakeColumnGroupMemorySizes(config *syncmgr.GrowingFlushConfig, size int64) map[int64]int64 {
	if config == nil || len(config.ColumnGroups) == 0 {
		return nil
	}
	result := make(map[int64]int64, len(config.ColumnGroups))
	for _, columnGroup := range config.ColumnGroups {
		result[columnGroup.GroupID] = size
	}
	return result
}

func (p fakeGrowingSourceProvider) GetGrowingFlushSource(int64, *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
	return p.source, p.state
}

func (p fakeGrowingSourceProvider) PrepareGrowingSourceReleaseHandoff(context.Context, uint64) error {
	return nil
}

func (s *L0WriteBufferSuite) SetupSuite() {
	paramtable.Get().Init(paramtable.NewBaseTable())
	s.collID = 100
	s.collSchema = &schemapb.CollectionSchema{
		Name: "test_collection",
		Fields: []*schemapb.FieldSchema{
			{
				FieldID: common.RowIDField, Name: common.RowIDFieldName, DataType: schemapb.DataType_Int64,
			},
			{
				FieldID: common.TimeStampField, Name: common.TimeStampFieldName, DataType: schemapb.DataType_Int64,
			},
			{
				FieldID: 100, Name: "pk", DataType: schemapb.DataType_Int64, IsPrimaryKey: true,
			},
			{
				FieldID: 101, Name: "vector", DataType: schemapb.DataType_FloatVector,
				TypeParams: []*commonpb.KeyValuePair{
					{Key: common.DimKey, Value: "128"},
				},
			},
		},
	}

	for _, field := range s.collSchema.Fields {
		if field.GetIsPrimaryKey() {
			s.pkSchema = field
			break
		}
	}
	s.channelName = "by-dev-rootcoord-dml_0v0"
}

func (s *L0WriteBufferSuite) composeInsertMsg(segmentID int64, rowCount int, dim int, pkType schemapb.DataType) ([]int64, *msgstream.InsertMsg) {
	tss := lo.RepeatBy(rowCount, func(idx int) int64 { return int64(tsoutil.ComposeTSByTimeWithLogical(time.Now(), int64(idx))) })
	vectors := lo.RepeatBy(rowCount, func(_ int) []float32 {
		return lo.RepeatBy(dim, func(_ int) float32 { return rand.Float32() })
	})
	flatten := lo.Flatten(vectors)
	var pkField *schemapb.FieldData
	switch pkType {
	case schemapb.DataType_Int64:
		pkField = &schemapb.FieldData{
			FieldId: common.StartOfUserFieldID, FieldName: "pk", Type: schemapb.DataType_Int64,
			Field: &schemapb.FieldData_Scalars{
				Scalars: &schemapb.ScalarField{
					Data: &schemapb.ScalarField_LongData{
						LongData: &schemapb.LongArray{
							Data: tss,
						},
					},
				},
			},
		}
	case schemapb.DataType_VarChar:
		pkField = &schemapb.FieldData{
			FieldId: common.StartOfUserFieldID, FieldName: "pk", Type: schemapb.DataType_VarChar,
			Field: &schemapb.FieldData_Scalars{
				Scalars: &schemapb.ScalarField{
					Data: &schemapb.ScalarField_StringData{
						StringData: &schemapb.StringArray{
							Data: lo.Map(tss, func(v int64, _ int) string { return fmt.Sprintf("%v", v) }),
						},
					},
				},
			},
		}
	}
	return tss, &msgstream.InsertMsg{
		InsertRequest: &msgpb.InsertRequest{
			SegmentID:  segmentID,
			Version:    msgpb.InsertDataVersion_ColumnBased,
			RowIDs:     tss,
			Timestamps: lo.Map(tss, func(id int64, _ int) uint64 { return uint64(id) }),
			FieldsData: []*schemapb.FieldData{
				{
					FieldId: common.RowIDField, FieldName: common.RowIDFieldName, Type: schemapb.DataType_Int64,
					Field: &schemapb.FieldData_Scalars{
						Scalars: &schemapb.ScalarField{
							Data: &schemapb.ScalarField_LongData{
								LongData: &schemapb.LongArray{
									Data: tss,
								},
							},
						},
					},
				},
				{
					FieldId: common.TimeStampField, FieldName: common.TimeStampFieldName, Type: schemapb.DataType_Int64,
					Field: &schemapb.FieldData_Scalars{
						Scalars: &schemapb.ScalarField{
							Data: &schemapb.ScalarField_LongData{
								LongData: &schemapb.LongArray{
									Data: tss,
								},
							},
						},
					},
				},
				pkField,
				{
					FieldId: common.StartOfUserFieldID + 1, FieldName: "vector", Type: schemapb.DataType_FloatVector,
					Field: &schemapb.FieldData_Vectors{
						Vectors: &schemapb.VectorField{
							Dim: int64(dim),
							Data: &schemapb.VectorField_FloatVector{
								FloatVector: &schemapb.FloatArray{
									Data: flatten,
								},
							},
						},
					},
				},
			},
		},
	}
}

func (s *L0WriteBufferSuite) textSchema() *schemapb.CollectionSchema {
	return &schemapb.CollectionSchema{
		Name: "test_text_collection",
		Fields: append(append([]*schemapb.FieldSchema{}, s.collSchema.Fields...), &schemapb.FieldSchema{
			FieldID:  common.StartOfUserFieldID + 2,
			Name:     "text",
			DataType: schemapb.DataType_Text,
		}),
	}
}

func (s *L0WriteBufferSuite) composeTextInsertMsg(segmentID int64, rowCount int) ([]int64, *msgstream.InsertMsg) {
	pks, msg := s.composeInsertMsg(segmentID, rowCount, 128, schemapb.DataType_Int64)
	msg.FieldsData = append(msg.FieldsData, &schemapb.FieldData{
		FieldId:   common.StartOfUserFieldID + 2,
		FieldName: "text",
		Type:      schemapb.DataType_Text,
		Field: &schemapb.FieldData_Scalars{
			Scalars: &schemapb.ScalarField{
				Data: &schemapb.ScalarField_StringData{
					StringData: &schemapb.StringArray{
						Data: lo.RepeatBy(rowCount, func(idx int) string {
							return fmt.Sprintf("text-%d", idx)
						}),
					},
				},
			},
		},
	})
	return pks, msg
}

func (s *L0WriteBufferSuite) composeDeleteMsg(pks []storage.PrimaryKey) *msgstream.DeleteMsg {
	delMsg := &msgstream.DeleteMsg{
		DeleteRequest: &msgpb.DeleteRequest{
			PrimaryKeys: storage.ParsePrimaryKeys2IDs(pks),
			Timestamps:  lo.RepeatBy(len(pks), func(idx int) uint64 { return tsoutil.ComposeTSByTimeWithLogical(time.Now(), int64(idx)+1) }),
		},
	}
	return delMsg
}

func (s *L0WriteBufferSuite) SetupTest() {
	s.syncMgr = syncmgr.NewMockSyncManager(s.T())
	s.metacache = metacache.NewMockMetaCache(s.T())
	s.metacache.EXPECT().GetSchema(mock.Anything).Return(s.collSchema).Maybe()
	s.metacache.EXPECT().Collection().Return(s.collID).Maybe()
	// GetCheckpoint derives a candidate from segments that still owe a sealed
	// flush; with a mock metacache there are none.
	s.metacache.EXPECT().GetSegmentsBy(mock.Anything).Return(nil).Maybe()
	s.allocator = allocator.NewMockGIDAllocator()
	s.allocator.AllocOneF = func() (int64, error) { return int64(tsoutil.ComposeTSByTime(time.Now())), nil }
	s.allocator.AllocF = func(count uint32) (int64, int64, error) {
		start := int64(tsoutil.ComposeTSByTime(time.Now()))
		return start, start + int64(count), nil
	}
}

func (s *L0WriteBufferSuite) newTextMetaCache(schema *schemapb.CollectionSchema) *metacache.MockMetaCache {
	mc := metacache.NewMockMetaCache(s.T())
	mc.EXPECT().GetSchema(mock.Anything).Return(schema).Maybe()
	mc.EXPECT().Collection().Return(s.collID).Maybe()
	mc.EXPECT().GetSegmentsBy(mock.Anything).Return(nil).Maybe()
	return mc
}

func (s *L0WriteBufferSuite) newTextRealMetaCache(schema *schemapb.CollectionSchema) metacache.MetaCache {
	return metacache.NewMetaCache(&datapb.ChannelWatchInfo{
		Schema: schema,
		Vchan: &datapb.VchannelInfo{
			CollectionID: s.collID,
			ChannelName:  s.channelName,
		},
	}, func(*datapb.SegmentInfo) pkoracle.PkStat {
		return pkoracle.NewBloomFilterSet()
	}, metacache.NewBM25StatsFactory)
}

func (s *L0WriteBufferSuite) TestBufferData() {
	s.Run("normal_run", func() {
		paramtable.Get().Save(paramtable.Get().CommonCfg.EnableGrowingSourceFlush.Key, "false")
		defer paramtable.Get().Reset(paramtable.Get().CommonCfg.EnableGrowingSourceFlush.Key)

		wb, err := NewL0WriteBuffer(s.channelName, s.metacache, s.syncMgr, &writeBufferOption{
			idAllocator: s.allocator,
		})
		s.NoError(err)

		pks, msg := s.composeInsertMsg(1000, 10, 128, schemapb.DataType_Int64)
		delMsg := s.composeDeleteMsg(lo.Map(pks, func(id int64, _ int) storage.PrimaryKey { return storage.NewInt64PrimaryKey(id) }))

		s.metacache.EXPECT().GetSegmentByID(int64(1000)).Return(nil, false).Once()
		s.metacache.EXPECT().AddSegment(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
		s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Return()

		metrics.DataNodeFlowGraphBufferDataSize.Reset()
		insertData, err := PrepareInsert(s.collSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.NoError(err)
		err = wb.BufferData(insertData, []*msgstream.DeleteMsg{delMsg}, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100)
		s.NoError(err)

		value, err := metrics.DataNodeFlowGraphBufferDataSize.GetMetricWithLabelValues(paramtable.GetStringNodeID(), fmt.Sprint(s.metacache.Collection()))
		s.NoError(err)
		s.MetricsEqual(value, 5616)

		delMsg = s.composeDeleteMsg(lo.Map(pks, func(id int64, _ int) storage.PrimaryKey { return storage.NewInt64PrimaryKey(id) }))
		err = wb.BufferData([]*InsertData{}, []*msgstream.DeleteMsg{delMsg}, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100)
		s.NoError(err)
		s.MetricsEqual(value, 5856)
	})
}

func (s *L0WriteBufferSuite) TestWriteBufferSyncPayloadBackpressureStartsAfterSubmit() {
	paramtable.Get().Save(paramtable.Get().CommonCfg.EnableGrowingSourceFlush.Key, "false")
	paramtable.Get().Save(paramtable.Get().DataNodeCfg.FlushInsertBufferSize.Key, "1")
	defer paramtable.Get().Reset(paramtable.Get().CommonCfg.EnableGrowingSourceFlush.Key)
	defer paramtable.Get().Reset(paramtable.Get().DataNodeCfg.FlushInsertBufferSize.Key)

	const segmentID = int64(1000)
	segment := metacache.NewSegmentInfo(&datapb.SegmentInfo{
		ID:             segmentID,
		PartitionID:    10,
		CollectionID:   s.collID,
		State:          commonpb.SegmentState_Growing,
		StorageVersion: storage.StorageV2,
	}, pkoracle.NewBloomFilterSet(), nil, metacache.NewEmptySegmentStats())
	s.metacache.EXPECT().GetSegmentByID(segmentID).Return(nil, false).Once()
	s.metacache.EXPECT().GetSegmentByID(segmentID).Return(segment, true).Maybe()
	s.metacache.EXPECT().AddSegment(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return().Once()
	s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Return().Maybe()

	wb, err := NewL0WriteBuffer(s.channelName, s.metacache, s.syncMgr, &writeBufferOption{
		idAllocator:  s.allocator,
		syncPolicies: []SyncPolicy{GetFullBufferPolicy()},
	})
	s.Require().NoError(err)
	l0wb := wb.(*l0WriteBuffer)

	submitted := make(chan *syncmgr.SyncTask, 1)
	s.syncMgr.EXPECT().SyncData(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, task syncmgr.Task, _ ...func(error) error) (*conc.Future[struct{}], error) {
			writeBufferTask := task.(*syncmgr.SyncTask)
			submitted <- writeBufferTask
			return conc.Go(func() (struct{}, error) { return struct{}{}, nil }), nil
		}).Once()

	_, msg := s.composeInsertMsg(segmentID, 1, 128, schemapb.DataType_Int64)
	insertData, err := PrepareInsert(s.collSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
	s.Require().NoError(err)
	metrics.DataNodeFlowGraphBufferDataSize.Reset()

	bufferDone := make(chan error, 1)
	go func() {
		bufferDone <- wb.BufferData(insertData, nil,
			&msgpb.MsgPosition{Timestamp: 100},
			&msgpb.MsgPosition{Timestamp: 200}, 100)
	}()

	var task *syncmgr.SyncTask
	select {
	case task = <-submitted:
	case <-time.After(time.Second):
		s.FailNow("full payload was not submitted before backpressure")
	}
	select {
	case err := <-bufferDone:
		s.FailNow("BufferData returned before the in-flight payload was released", err)
	case <-time.After(20 * time.Millisecond):
	}

	s.Equal(task.PayloadBytes(), l0wb.MemorySize())
	value, err := metrics.DataNodeFlowGraphBufferDataSize.GetMetricWithLabelValues(
		paramtable.GetStringNodeID(), fmt.Sprint(s.metacache.Collection()))
	s.Require().NoError(err)
	s.MetricsEqual(value, float64(task.PayloadBytes()))

	task.Abandon()
	select {
	case err := <-bufferDone:
		s.NoError(err)
	case <-time.After(time.Second):
		s.FailNow("payload release did not remove BufferData backpressure")
	}
	s.Zero(l0wb.MemorySize())
	s.MetricsEqual(value, 0)
}

func (s *L0WriteBufferSuite) TestBufferDataGrowingSourceMode() {
	paramtable.Get().Save(paramtable.Get().CommonCfg.UseLoonFFI.Key, "true")
	paramtable.Get().Save(paramtable.Get().CommonCfg.EnableGrowingSourceFlush.Key, "true")
	defer paramtable.Get().Reset(paramtable.Get().CommonCfg.UseLoonFFI.Key)
	defer paramtable.Get().Reset(paramtable.Get().CommonCfg.EnableGrowingSourceFlush.Key)

	s.Run("no_usable_source_buffers_payload", func() {
		textSchema := s.textSchema()
		metacache := s.newTextMetaCache(textSchema)
		wb, err := NewL0WriteBuffer(s.channelName, metacache, s.syncMgr, &writeBufferOption{
			idAllocator: s.allocator,
		})
		s.NoError(err)

		_, msg := s.composeTextInsertMsg(1000, 10)
		insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.NoError(err)

		// Twice per BufferData on the first insert: decideGrowingFlushSource
		// reads the sticky flushSourceMode, then CreateNewGrowingSegment checks
		// for prior existence. The third read is gone with the row counter the
		// flush range no longer needs.
		metacache.EXPECT().GetSegmentByID(int64(1000)).Return(nil, false).Times(2)
		metacache.EXPECT().AddSegment(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
		metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Return()

		err = wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100)
		s.NoError(err)

		l0wb := wb.(*l0WriteBuffer)
		segBuf := l0wb.buffers[int64(1000)]
		s.NotNil(segBuf)
		s.False(segBuf.insertBuffer.IsEmpty())
		s.EqualValues(100, wb.GetCheckpoint().GetTimestamp())
	})

	s.Run("usable_source_records_progress_and_pins_checkpoint", func() {
		textSchema := s.textSchema()
		mc := s.newTextMetaCache(textSchema)
		var resolvedSegmentID int64
		var resolvedFenceTs uint64
		resolveCalls := 0
		wb, err := NewL0WriteBuffer(s.channelName, mc, s.syncMgr, &writeBufferOption{
			idAllocator:        s.allocator,
			flushRetryInterval: time.Hour,
			growingSourceResolver: func(segmentID int64, endPos *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
				resolveCalls++
				resolvedSegmentID = segmentID
				resolvedFenceTs = endPos.GetTimestamp()
				if resolveCalls == 1 {
					return fakeGrowingFlushSource{}, syncmgr.GrowingSourceUsable
				}
				return nil, syncmgr.GrowingSourceUnavailable
			},
		})
		s.NoError(err)

		_, msg := s.composeTextInsertMsg(1001, 10)
		insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.NoError(err)

		segmentInfo := metacache.NewSegmentInfo(&datapb.SegmentInfo{
			ID:             1001,
			PartitionID:    10,
			CollectionID:   s.collID,
			InsertChannel:  s.channelName,
			StartPosition:  &msgpb.MsgPosition{Timestamp: 100},
			State:          commonpb.SegmentState_Growing,
			StorageVersion: storage.StorageV3,
			SchemaVersion:  100,
		}, pkoracle.NewBloomFilterSet(), nil, nil)

		// CreateNewGrowingSegment sees a new
		// segment; recordGrowingSourceProgress and triggerSync see the segment
		// created by AddSegment.
		mc.EXPECT().GetSegmentByID(int64(1001)).Return(nil, false).Times(2)
		mc.EXPECT().GetSegmentByID(int64(1001)).Return(segmentInfo, true).Times(2)
		mc.EXPECT().AddSegment(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
		mc.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Return()

		err = wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100)
		s.NoError(err)

		l0wb := wb.(*l0WriteBuffer)
		s.Nil(l0wb.buffers[int64(1001)])
		s.Contains(l0wb.growingSourceProgress, int64(1001))
		s.EqualValues(1001, resolvedSegmentID)
		// The fence is the position the flush runs to, not a row count.
		s.NotZero(resolvedFenceTs)
		s.EqualValues(100, wb.GetCheckpoint().GetTimestamp())
	})

	s.Run("writebuffer_source_is_sticky_after_payload_yielded", func() {
		textSchema := s.textSchema()
		metacache := s.newTextRealMetaCache(textSchema)
		sourceState := syncmgr.GrowingSourceUnavailable
		wb, err := NewL0WriteBuffer(s.channelName, metacache, s.syncMgr, &writeBufferOption{
			idAllocator:        s.allocator,
			flushRetryInterval: time.Hour,
			growingSourceResolver: func(segmentID int64, _ *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
				return fakeGrowingFlushSource{}, sourceState
			},
		})
		s.NoError(err)

		_, msg := s.composeTextInsertMsg(1101, 10)
		insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.NoError(err)

		err = wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100)
		s.NoError(err)

		l0wb := wb.(*l0WriteBuffer)
		s.NotNil(l0wb.buffers[int64(1101)])
		s.NotContains(l0wb.growingSourceProgress, int64(1101))

		_, err = l0wb.getSyncTask(context.Background(), 1101)
		s.NoError(err)
		s.Nil(l0wb.buffers[int64(1101)])

		sourceState = syncmgr.GrowingSourceUsable
		_, msg = s.composeTextInsertMsg(1101, 5)
		insertData, err = PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.NoError(err)

		err = wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 300}, &msgpb.MsgPosition{Timestamp: 400}, 100)
		s.NoError(err)

		s.NotNil(l0wb.buffers[int64(1101)])
		s.NotContains(l0wb.growingSourceProgress, int64(1101))
	})

	s.Run("drop_close_waits_for_pending_growing_source", func() {
		textSchema := s.textSchema()
		metaCache := s.newTextRealMetaCache(textSchema)
		metaWriter := syncmgr.NewMockMetaWriter(s.T())
		syncManager := syncmgr.NewMockSyncManager(s.T())
		var sourceState atomic.Int32
		sourceState.Store(int32(syncmgr.GrowingSourcePending))
		var dropStarted atomic.Bool
		pendingObserved := make(chan struct{}, 1)
		wb, err := NewL0WriteBuffer(s.channelName, metaCache, syncManager, &writeBufferOption{
			idAllocator:        s.allocator,
			metaWriter:         metaWriter,
			flushRetryInterval: 10 * time.Millisecond,
			growingSourceResolver: func(segmentID int64, _ *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
				state := syncmgr.GrowingSourceState(sourceState.Load())
				if dropStarted.Load() && state == syncmgr.GrowingSourcePending {
					select {
					case pendingObserved <- struct{}{}:
					default:
					}
				}
				return fakeGrowingFlushSource{}, state
			},
		})
		s.NoError(err)

		_, msg := s.composeTextInsertMsg(1103, 10)
		insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.NoError(err)

		err = wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100)
		s.NoError(err)

		var syncCompleted atomic.Bool
		var dropAfterSync atomic.Bool
		submitted := make(chan *syncmgr.GrowingSourceSyncTask, 1)
		syncManager.EXPECT().SyncData(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask"), mock.Anything).
			RunAndReturn(func(_ context.Context, task syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
				textTask := task.(*syncmgr.GrowingSourceSyncTask)
				submitted <- textTask
				return conc.Go(func() (struct{}, error) {
					if textTask.BatchRows() > 0 {
						metaCache.UpdateSegments(
							metacache.FinishSyncing(textTask.BatchRows()),
							metacache.WithSegmentIDs(textTask.SegmentID()),
						)
					}
					var callbackErr error
					for _, callback := range callbacks {
						callbackErr = callback(callbackErr)
					}
					syncCompleted.Store(true)
					return struct{}{}, callbackErr
				}), nil
			}).Once()
		metaWriter.EXPECT().DropChannel(mock.Anything, s.channelName).
			Run(func(context.Context, string) {
				dropAfterSync.Store(syncCompleted.Load())
			}).
			Return(nil).
			Once()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		closeResult := make(chan any, 1)
		dropStarted.Store(true)
		go func() {
			var panicValue any
			func() {
				defer func() {
					panicValue = recover()
				}()
				wb.Close(ctx, true)
			}()
			closeResult <- panicValue
		}()

		select {
		case <-pendingObserved:
		case <-time.After(time.Second):
			s.FailNow("drop did not observe the pending growing source")
		}
		select {
		case panicValue := <-closeResult:
			s.FailNow("drop returned before the growing source became usable", "panic", panicValue)
		case <-time.After(20 * time.Millisecond):
		}

		sourceState.Store(int32(syncmgr.GrowingSourceUsable))
		select {
		case panicValue := <-closeResult:
			s.Nil(panicValue)
		case <-time.After(time.Second):
			s.FailNow("drop did not finish after the growing source became usable")
		}

		select {
		case task := <-submitted:
			s.True(task.IsDrop())
			s.EqualValues(10, task.BatchRows())
		case <-time.After(time.Second):
			s.FailNow("drop did not submit the final growing-source task")
		}

		l0wb := wb.(*l0WriteBuffer)
		s.NotContains(l0wb.growingSourceProgress, int64(1103))
		s.False(anyGrowingRetryArmed(l0wb.writeBufferBase))
		s.True(dropAfterSync.Load(), "DropChannel must run after the final source sync commits")
		segment, ok := metaCache.GetSegmentByID(1103)
		if ok {
			s.Zero(segment.SyncingRows())
			s.True(metacache.WithNoSyncingTask().Filter(segment))
		}
	})

	s.Run("drop_close_pending_growing_source_honors_caller_timeout", func() {
		textSchema := s.textSchema()
		metaCache := s.newTextRealMetaCache(textSchema)
		metaWriter := syncmgr.NewMockMetaWriter(s.T())
		syncManager := syncmgr.NewMockSyncManager(s.T())
		pendingObserved := make(chan struct{}, 1)
		var dropStarted atomic.Bool
		wb, err := NewL0WriteBuffer(s.channelName, metaCache, syncManager, &writeBufferOption{
			idAllocator:        s.allocator,
			metaWriter:         metaWriter,
			flushRetryInterval: 5 * time.Millisecond,
			growingSourceResolver: func(segmentID int64, _ *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
				if dropStarted.Load() {
					select {
					case pendingObserved <- struct{}{}:
					default:
					}
				}
				return fakeGrowingFlushSource{}, syncmgr.GrowingSourcePending
			},
		})
		s.NoError(err)

		_, msg := s.composeTextInsertMsg(1104, 10)
		insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.NoError(err)
		err = wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100)
		s.NoError(err)

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		closeResult := make(chan any, 1)
		dropStarted.Store(true)
		go func() {
			var panicValue any
			func() {
				defer func() {
					panicValue = recover()
				}()
				wb.Close(ctx, true)
			}()
			closeResult <- panicValue
		}()

		select {
		case <-pendingObserved:
		case <-time.After(time.Second):
			s.FailNow("drop did not observe the pending growing source")
		}

		var panicValue any
		select {
		case panicValue = <-closeResult:
		case <-time.After(time.Second):
			s.FailNow("drop did not honor the caller deadline")
		}
		s.Require().NotNil(panicValue)
		panicErr, ok := panicValue.(error)
		s.Require().True(ok, "drop should propagate the caller context error")
		s.ErrorIs(panicErr, context.DeadlineExceeded)

		l0wb := wb.(*l0WriteBuffer)
		// The progress is deliberately KEPT. It is one of the candidate sources
		// GetCheckpoint pins on, and this drop just declared its rows
		// un-committed: clearing it would let the checkpoint fall back to
		// wb.checkpoint — the latest CONSUMED position — past data WAL replay
		// still has to redo.
		s.Contains(l0wb.growingSourceProgress, int64(1104))
		s.EqualValues(100, wb.GetCheckpoint().GetTimestamp(),
			"checkpoint must stay pinned at the un-committed data, not advance to the consumed position")
		s.False(anyGrowingRetryArmed(l0wb.writeBufferBase))
		s.True(l0wb.closed)
		s.False(l0wb.dropping)
		segment, ok := metaCache.GetSegmentByID(1104)
		if ok {
			s.Zero(segment.SyncingRows())
			s.True(metacache.WithNoSyncingTask().Filter(segment))
		}
		metaWriter.AssertNotCalled(s.T(), "DropChannel", mock.Anything, s.channelName)
		syncManager.AssertNotCalled(s.T(), "SyncData", mock.Anything, mock.Anything, mock.Anything)
	})

	s.Run("pending_source_records_progress_instead_of_falling_back_to_writebuffer", func() {
		textSchema := s.textSchema()
		mc := s.newTextMetaCache(textSchema)
		wb, err := NewL0WriteBuffer(s.channelName, mc, s.syncMgr, &writeBufferOption{
			idAllocator:        s.allocator,
			flushRetryInterval: time.Hour,
			growingSourceResolver: func(segmentID int64, _ *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
				return fakeGrowingFlushSource{}, syncmgr.GrowingSourcePending
			},
		})
		s.NoError(err)

		_, msg := s.composeTextInsertMsg(1002, 10)
		insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.NoError(err)

		segmentInfo := metacache.NewSegmentInfo(&datapb.SegmentInfo{
			ID:             1002,
			PartitionID:    10,
			CollectionID:   s.collID,
			InsertChannel:  s.channelName,
			StartPosition:  &msgpb.MsgPosition{Timestamp: 100},
			State:          commonpb.SegmentState_Growing,
			StorageVersion: storage.StorageV3,
			SchemaVersion:  100,
		}, pkoracle.NewBloomFilterSet(), nil, nil)

		// CreateNewGrowingSegment sees a new
		// segment; recordGrowingSourceProgress and triggerSync see the segment
		// created by AddSegment.
		mc.EXPECT().GetSegmentByID(int64(1002)).Return(nil, false).Times(2)
		mc.EXPECT().GetSegmentByID(int64(1002)).Return(segmentInfo, true).Times(2)
		mc.EXPECT().AddSegment(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
		mc.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Return()

		err = wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100)
		s.NoError(err)

		l0wb := wb.(*l0WriteBuffer)
		s.Nil(l0wb.buffers[int64(1002)])
		s.Contains(l0wb.growingSourceProgress, int64(1002))
		s.EqualValues(100, wb.GetCheckpoint().GetTimestamp())
	})

	s.Run("pending_source_syncs_after_it_becomes_usable_and_acks_progress", func() {
		textSchema := s.textSchema()
		metacache := s.newTextRealMetaCache(textSchema)
		sourceState := syncmgr.GrowingSourcePending
		wb, err := NewL0WriteBuffer(s.channelName, metacache, s.syncMgr, &writeBufferOption{
			idAllocator:        s.allocator,
			flushRetryInterval: time.Hour,
			growingSourceResolver: func(segmentID int64, _ *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
				return fakeGrowingFlushSource{}, sourceState
			},
		})
		s.NoError(err)

		_, msg := s.composeTextInsertMsg(1003, 10)
		insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.NoError(err)

		err = wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100)
		s.NoError(err)

		l0wb := wb.(*l0WriteBuffer)
		s.Nil(l0wb.buffers[int64(1003)])
		s.Contains(l0wb.growingSourceProgress, int64(1003))
		s.EqualValues(100, wb.GetCheckpoint().GetTimestamp())

		sourceState = syncmgr.GrowingSourceUsable
		taskCh := make(chan *syncmgr.GrowingSourceSyncTask, 1)
		s.syncMgr.EXPECT().SyncData(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask"), mock.Anything).
			RunAndReturn(func(ctx context.Context, task syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
				textTask := task.(*syncmgr.GrowingSourceSyncTask)
				taskCh <- textTask
				return conc.Go(func() (struct{}, error) {
					for _, callback := range callbacks {
						if err := callback(nil); err != nil {
							return struct{}{}, err
						}
					}
					return struct{}{}, nil
				}), nil
			}).Once()

		futures := l0wb.syncSegments(context.Background(), []int64{1003})
		s.Require().Len(futures, 1)
		s.NoError(conc.AwaitAll(futures...))
		textTask := <-taskCh
		s.EqualValues(1003, textTask.SegmentID())
		s.NotZero(textTask.FlushThroughTs())
		s.EqualValues(10, textTask.BatchRows())
		s.EqualValues(100, textTask.StartPosition().GetTimestamp())
		s.EqualValues(200, textTask.Checkpoint().GetTimestamp())
		s.Contains(l0wb.growingSourceProgress, int64(1003))
		s.Empty(l0wb.growingSourceProgress[int64(1003)].batches)
		s.EqualValues(200, wb.GetCheckpoint().GetTimestamp())
	})

	s.Run("source_becomes_unavailable_when_building_task_retries_without_fatal", func() {
		textSchema := s.textSchema()
		metacache := s.newTextRealMetaCache(textSchema)
		resolveCalls := 0
		wb, err := NewL0WriteBuffer(s.channelName, metacache, s.syncMgr, &writeBufferOption{
			idAllocator:        s.allocator,
			flushRetryInterval: time.Hour,
			growingSourceResolver: func(segmentID int64, _ *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
				resolveCalls++
				if resolveCalls <= 3 {
					return fakeGrowingFlushSource{}, syncmgr.GrowingSourceUsable
				}
				return nil, syncmgr.GrowingSourceUnavailable
			},
		})
		s.NoError(err)

		_, msg := s.composeTextInsertMsg(1009, 10)
		insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.NoError(err)

		err = wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100)
		s.NoError(err)

		l0wb := wb.(*l0WriteBuffer)
		s.Contains(l0wb.growingSourceProgress, int64(1009))
		s.EqualValues(100, wb.GetCheckpoint().GetTimestamp())
		segment, ok := metacache.GetSegmentByID(1009)
		s.True(ok)
		s.Equal(commonpb.SegmentState_Growing, segment.State())
	})

	s.Run("pending_source_syncs_after_source_becomes_usable", func() {
		textSchema := s.textSchema()
		metacache := s.newTextRealMetaCache(textSchema)
		sourceState := atomic.Int32{}
		sourceState.Store(int32(syncmgr.GrowingSourcePending))
		wb, err := NewL0WriteBuffer(s.channelName, metacache, s.syncMgr, &writeBufferOption{
			idAllocator:        s.allocator,
			flushRetryInterval: time.Hour,
			growingSourceResolver: func(segmentID int64, _ *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
				return fakeGrowingFlushSource{}, syncmgr.GrowingSourceState(sourceState.Load())
			},
		})
		s.NoError(err)

		_, msg := s.composeTextInsertMsg(1007, 10)
		insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.NoError(err)

		err = wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100)
		s.NoError(err)

		taskCh := make(chan *syncmgr.GrowingSourceSyncTask, 1)
		doneCh := make(chan struct{})
		s.syncMgr.EXPECT().SyncData(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask"), mock.Anything).
			RunAndReturn(func(ctx context.Context, task syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
				textTask := task.(*syncmgr.GrowingSourceSyncTask)
				taskCh <- textTask
				return conc.Go(func() (struct{}, error) {
					defer close(doneCh)
					for _, callback := range callbacks {
						if err := callback(nil); err != nil {
							return struct{}{}, err
						}
					}
					return struct{}{}, nil
				}), nil
			}).Once()

		sourceState.Store(int32(syncmgr.GrowingSourceUsable))
		l0wb := wb.(*l0WriteBuffer)
		futures := l0wb.syncSegments(context.Background(), []int64{1007})
		s.Require().Len(futures, 1)
		textTask := <-taskCh
		s.EqualValues(1007, textTask.SegmentID())
		s.NoError(conc.AwaitAll(futures...))
		<-doneCh
		l0wb.mut.RLock()
		defer l0wb.mut.RUnlock()
		s.Contains(l0wb.growingSourceProgress, int64(1007))
		s.Empty(l0wb.growingSourceProgress[int64(1007)].batches)
	})

	s.Run("source_task_run_flushes_source_updates_meta_and_acks_progress", func() {
		textSchema := s.textSchema()
		metacache := s.newTextRealMetaCache(textSchema)
		metaWriter := syncmgr.NewMockMetaWriter(s.T())
		wb, err := NewL0WriteBuffer(s.channelName, metacache, s.syncMgr, &writeBufferOption{
			idAllocator: s.allocator,
			metaWriter:  metaWriter,
			growingSourceResolver: func(segmentID int64, _ *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
				return fakeGrowingFlushSource{}, syncmgr.GrowingSourceUsable
			},
		})
		s.NoError(err)

		_, msg := s.composeTextInsertMsg(1006, 10)
		insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.NoError(err)

		metaWriter.EXPECT().UpdateGrowingSourceSync(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask")).
			Run(func(_ context.Context, task *syncmgr.GrowingSourceSyncTask) {
				s.Equal("manifest", task.ManifestPath())
				s.EqualValues(10, task.BatchRows())
			}).Return(nil).Once()
		done := make(chan struct{})
		s.syncMgr.EXPECT().SyncData(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask"), mock.Anything).
			RunAndReturn(func(ctx context.Context, task syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
				textTask := task.(*syncmgr.GrowingSourceSyncTask)
				textTask.WithChunkManager(storage.NewLocalChunkManager(objectstorage.RootPath(s.T().TempDir())))
				return conc.Go(func() (struct{}, error) {
					defer close(done)
					err := runGrowingTaskWithCallbacks(ctx, textTask, callbacks)
					return struct{}{}, err
				}), nil
			}).Once()

		err = wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100)
		s.NoError(err)

		l0wb := wb.(*l0WriteBuffer)
		futures := l0wb.syncSegments(context.Background(), []int64{1006})
		s.Require().Len(futures, 1)
		s.NoError(conc.AwaitAll(futures...))
		<-done

		s.Contains(l0wb.growingSourceProgress, int64(1006))
		s.Empty(l0wb.growingSourceProgress[int64(1006)].batches)
		segment, ok := metacache.GetSegmentByID(1006)
		s.True(ok)
		s.EqualValues(10, segment.FlushedRows())
		s.Equal("manifest", segment.ManifestPath())
	})

	s.Run("drop_close_reuses_synced_manifest_for_zero_row_growing_source_task", func() {
		textSchema := s.textSchema()
		metacache := s.newTextRealMetaCache(textSchema)
		flushCalls := 0
		source := fakeGrowingFlushSource{
			flushFunc: func(_ context.Context, startTs, endTs uint64, config *syncmgr.GrowingFlushConfig) (*syncmgr.GrowingFlushResult, error) {
				flushCalls++
				// First flush of the segment: the lower fence is unset, and the
				// upper one is the position being flushed to.
				s.EqualValues(0, startTs)
				s.NotZero(endTs)
				return &syncmgr.GrowingFlushResult{
					ManifestPath:           "manifest-0-10",
					NumRows:                10,
					ColumnGroupMemorySizes: fakeColumnGroupMemorySizes(config, 80),
				}, nil
			},
		}
		metaWriter := syncmgr.NewMockMetaWriter(s.T())
		wb, err := NewL0WriteBuffer(s.channelName, metacache, s.syncMgr, &writeBufferOption{
			idAllocator: s.allocator,
			metaWriter:  metaWriter,
			growingSourceResolver: func(segmentID int64, _ *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
				return source, syncmgr.GrowingSourceUsable
			},
		})
		s.NoError(err)

		updateCalls := 0
		metaWriter.EXPECT().UpdateGrowingSourceSync(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask")).
			Run(func(_ context.Context, task *syncmgr.GrowingSourceSyncTask) {
				updateCalls++
				if updateCalls == 1 {
					s.False(task.IsDrop())
					s.EqualValues(10, task.BatchRows())
				} else {
					s.True(task.IsDrop())
					s.EqualValues(0, task.BatchRows())
				}
				s.Equal("manifest-0-10", task.ManifestPath())
			}).Return(nil).Twice()
		metaWriter.EXPECT().DropChannel(mock.Anything, s.channelName).Return(nil).Once()

		done := make(chan struct{})
		s.syncMgr.EXPECT().SyncData(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask"), mock.Anything).
			RunAndReturn(func(ctx context.Context, task syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
				textTask := task.(*syncmgr.GrowingSourceSyncTask)
				textTask.WithChunkManager(storage.NewLocalChunkManager(objectstorage.RootPath(s.T().TempDir())))
				return conc.Go(func() (struct{}, error) {
					err := runGrowingTaskWithCallbacks(ctx, textTask, callbacks)
					if !textTask.IsDrop() {
						close(done)
					}
					return struct{}{}, err
				}), nil
			}).Twice()

		_, msg := s.composeTextInsertMsg(1013, 10)
		insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.NoError(err)
		err = wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100)
		s.NoError(err)
		l0wb := wb.(*l0WriteBuffer)
		futures := l0wb.syncSegments(context.Background(), []int64{1013})
		s.Require().Len(futures, 1)
		s.NoError(conc.AwaitAll(futures...))
		<-done

		s.NotPanics(func() {
			wb.Close(context.Background(), true)
		})
		s.Equal(1, flushCalls)
	})

	s.Run("source_task_error_retries_without_default_error_handler_panic", func() {
		textSchema := s.textSchema()
		metacache := s.newTextRealMetaCache(textSchema)
		flushCalls := 0
		source := fakeGrowingFlushSource{
			flushFunc: func(_ context.Context, _, _ uint64, config *syncmgr.GrowingFlushConfig) (*syncmgr.GrowingFlushResult, error) {
				flushCalls++
				if flushCalls == 1 {
					return nil, fmt.Errorf("mock growing source flush error")
				}
				return &syncmgr.GrowingFlushResult{
					ManifestPath:           "manifest-retry",
					NumRows:                10,
					ColumnGroupMemorySizes: fakeColumnGroupMemorySizes(config, 80),
				}, nil
			},
		}
		var errorHandlerCalls atomic.Int32
		var growingHandlerCalls atomic.Int32
		wb, err := NewL0WriteBuffer(s.channelName, metacache, s.syncMgr, &writeBufferOption{
			idAllocator:        s.allocator,
			flushRetryInterval: time.Hour,
			errorHandler: func(error) {
				errorHandlerCalls.Add(1)
			},
			growingSourceErrorHandler: func(error) {
				growingHandlerCalls.Add(1)
			},
			growingSourceResolver: func(segmentID int64, _ *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
				return source, syncmgr.GrowingSourceUsable
			},
		})
		s.NoError(err)

		done := make(chan *syncmgr.GrowingSourceSyncTask, 2)
		s.syncMgr.EXPECT().SyncData(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask"), mock.Anything).
			RunAndReturn(func(ctx context.Context, task syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
				textTask := task.(*syncmgr.GrowingSourceSyncTask)
				textTask.WithChunkManager(storage.NewLocalChunkManager(objectstorage.RootPath(s.T().TempDir())))
				return conc.Go(func() (struct{}, error) {
					defer func() {
						done <- textTask
					}()
					err := runGrowingTaskWithCallbacks(ctx, textTask, callbacks)
					return struct{}{}, err
				}), nil
			}).Twice()

		_, msg := s.composeTextInsertMsg(1010, 10)
		insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.NoError(err)
		err = wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100)
		s.NoError(err)
		l0wb := wb.(*l0WriteBuffer)
		futures := l0wb.syncSegments(context.Background(), []int64{1010})
		s.Require().Len(futures, 1)
		s.ErrorContains(conc.AwaitAll(futures...), "mock growing source flush error")
		firstTask := <-done

		// A retryable growing-source failure is reported through the
		// non-fatal handler and never escalated: the rows stay in the growing
		// segment and the write buffer re-submits the task.
		s.EqualValues(1, growingHandlerCalls.Load())
		s.EqualValues(0, errorHandlerCalls.Load())
		progress, ok := l0wb.growingSourceProgress[int64(1010)]
		s.True(ok)
		s.EqualValues(1, progress.failureCount)
		s.Contains(progress.lastFailure, "mock growing source flush error")
		s.EqualValues(10, firstTask.BatchRows())
		segment, ok := metacache.GetSegmentByID(1010)
		s.True(ok)
		s.EqualValues(0, segment.FlushedRows())
		s.EqualValues(0, segment.SyncingRows())
		s.EqualValues(10, segment.BufferRows())

		futures = l0wb.syncSegments(context.Background(), []int64{1010})
		s.Require().Len(futures, 1)
		s.NoError(conc.AwaitAll(futures...))
		secondTask := <-done
		// The retry succeeded, so neither handler fires again.
		s.EqualValues(1, growingHandlerCalls.Load())
		s.EqualValues(0, errorHandlerCalls.Load())
		s.EqualValues(10, secondTask.BatchRows())
		segment, ok = metacache.GetSegmentByID(1010)
		s.True(ok)
		s.EqualValues(10, segment.FlushedRows())
		s.EqualValues(0, segment.SyncingRows())
		s.Equal("manifest-retry", segment.ManifestPath())
	})

	s.Run("committed_T1_replay_keeps_later_T2_drop_debt", func() {
		textSchema := s.textSchema()
		mc := s.newTextRealMetaCache(textSchema)
		syncManager := syncmgr.NewMockSyncManager(s.T())
		metaWriter := syncmgr.NewMockMetaWriter(s.T())
		chunkManager := storage.NewLocalChunkManager(objectstorage.RootPath(s.T().TempDir()))

		rowsPerFlush := []int64{10, 5}
		var ranges [][2]uint64
		source := fakeGrowingFlushSource{
			pkRows: func() int64 { return rowsPerFlush[len(ranges)] },
			flushFunc: func(_ context.Context, startTs, endTs uint64, config *syncmgr.GrowingFlushConfig) (*syncmgr.GrowingFlushResult, error) {
				idx := len(ranges)
				ranges = append(ranges, [2]uint64{startTs, endTs})
				return &syncmgr.GrowingFlushResult{
					ManifestPath:           packed.MarshalManifestPath(config.SegmentBasePath, int64(idx+1)),
					NumRows:                rowsPerFlush[idx],
					ColumnGroupMemorySizes: fakeColumnGroupMemorySizes(config, 80),
				}, nil
			},
		}
		wb, err := NewL0WriteBuffer(s.channelName, mc, syncManager, &writeBufferOption{
			idAllocator: s.allocator,
			metaWriter:  metaWriter,
			growingSourceResolver: func(int64, *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
				return source, syncmgr.GrowingSourceUsable
			},
		})
		s.Require().NoError(err)
		l0wb := wb.(*l0WriteBuffer)

		var metaCalls atomic.Int32
		metaWriter.EXPECT().UpdateGrowingSourceSync(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask")).
			RunAndReturn(func(context.Context, *syncmgr.GrowingSourceSyncTask) error {
				if metaCalls.Add(1) == 1 {
					return fmt.Errorf("metadata ack failed")
				}
				return nil
			}).Times(3)

		type submittedTask struct {
			index int
			task  *syncmgr.GrowingSourceSyncTask
		}
		type completedTask struct {
			index int
			err   error
		}
		submitted := make(chan submittedTask, 3)
		completed := make(chan completedTask, 3)
		var submitCalls atomic.Int32
		syncManager.EXPECT().SyncData(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask"), mock.Anything).
			RunAndReturn(func(ctx context.Context, task syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
				index := int(submitCalls.Add(1))
				growingTask := task.(*syncmgr.GrowingSourceSyncTask)
				growingTask.WithChunkManager(chunkManager)
				submitted <- submittedTask{index: index, task: growingTask}
				return conc.Go(func() (struct{}, error) {
					err := runGrowingTaskWithCallbacks(ctx, growingTask, callbacks)
					completed <- completedTask{index: index, err: err}
					return struct{}{}, err
				}), nil
			}).Times(3)

		_, t1 := s.composeTextInsertMsg(1301, 10)
		insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{t1})
		s.Require().NoError(err)
		s.Require().NoError(wb.BufferData(insertData, nil,
			&msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100))
		futures := l0wb.syncSegments(context.Background(), []int64{1301})
		s.Require().Len(futures, 1)
		s.ErrorContains(conc.AwaitAll(futures...), "metadata ack failed")
		first := <-submitted
		s.Equal(1, first.index)
		s.False(first.task.IsFlush())
		s.False(first.task.IsDrop())
		firstCompleted := <-completed
		s.Equal(1, firstCompleted.index)
		s.ErrorContains(firstCompleted.err, "metadata ack failed")

		_, t2 := s.composeTextInsertMsg(1301, 5)
		insertData, err = PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{t2})
		s.Require().NoError(err)
		s.Require().NoError(wb.BufferData(insertData, nil,
			&msgpb.MsgPosition{Timestamp: 300}, &msgpb.MsgPosition{Timestamp: 400}, 300))
		wb.DropPartitions([]int64{0})

		futures = l0wb.syncSegments(context.Background(), []int64{1301})
		s.Require().Len(futures, 1)
		replay := <-submitted
		s.Equal(2, replay.index)
		s.EqualValues(200, replay.task.Checkpoint().GetTimestamp())
		s.EqualValues(10, replay.task.BatchRows())
		s.False(replay.task.IsFlush())
		s.False(replay.task.IsDrop(), "the frozen T1 replay must not absorb a later Drop")
		s.NoError(conc.AwaitAll(futures...))

		drop := <-submitted
		s.Equal(3, drop.index)
		s.EqualValues(400, drop.task.Checkpoint().GetTimestamp())
		s.EqualValues(5, drop.task.BatchRows())
		s.True(drop.task.IsDrop())
		for seen := 0; seen < 2; seen++ {
			completion := <-completed
			s.NoError(completion.err, "task %d must settle successfully", completion.index)
		}

		s.Equal([][2]uint64{{0, 200}, {200, 400}}, ranges)
		_, exists := mc.GetSegmentByID(1301)
		s.False(exists)
		s.NotContains(l0wb.growingSourceProgress, int64(1301))
		s.Nil(l0wb.syncCheckpoint.GetEarliestWithDefault(nil), "both T1 and T2 checkpoint candidates must settle")
	})

	s.Run("replay_built_before_drop_preserves_drop_through_commit", func() {
		textSchema := s.textSchema()
		mc := s.newTextRealMetaCache(textSchema)
		syncManager := syncmgr.NewMockSyncManager(s.T())
		metaWriter := syncmgr.NewMockMetaWriter(s.T())
		chunkManager := storage.NewLocalChunkManager(objectstorage.RootPath(s.T().TempDir()))
		var sourceAvailable atomic.Bool
		sourceAvailable.Store(true)
		source := fakeGrowingFlushSource{
			rows: 10,
			commitFunc: func(uint64) {
				sourceAvailable.Store(false)
			},
			flushFunc: func(_ context.Context, _, _ uint64, config *syncmgr.GrowingFlushConfig) (*syncmgr.GrowingFlushResult, error) {
				return &syncmgr.GrowingFlushResult{
					ManifestPath:           "manifest-final",
					NumRows:                10,
					ColumnGroupMemorySizes: fakeColumnGroupMemorySizes(config, 80),
				}, nil
			},
		}
		wb, err := NewL0WriteBuffer(s.channelName, mc, syncManager, &writeBufferOption{
			idAllocator: s.allocator,
			metaWriter:  metaWriter,
			growingSourceResolver: func(int64, *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
				if !sourceAvailable.Load() {
					return nil, syncmgr.GrowingSourceUnavailable
				}
				return source, syncmgr.GrowingSourceUsable
			},
		})
		s.Require().NoError(err)
		l0wb := wb.(*l0WriteBuffer)

		var metaCalls atomic.Int32
		metaWriter.EXPECT().UpdateGrowingSourceSync(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask")).
			RunAndReturn(func(context.Context, *syncmgr.GrowingSourceSyncTask) error {
				if metaCalls.Add(1) == 1 {
					return fmt.Errorf("metadata ack failed")
				}
				return nil
			}).Times(3)

		type submittedTask struct {
			index int
			task  *syncmgr.GrowingSourceSyncTask
		}
		submitted := make(chan submittedTask, 3)
		completed := make(chan int, 3)
		replayPrepared := make(chan struct{})
		allowReplayCommit := make(chan struct{})
		allowDropCommit := make(chan struct{})
		stateAfterReplayCommit := make(chan commonpb.SegmentState, 1)
		var submitCalls atomic.Int32
		syncManager.EXPECT().SyncData(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask"), mock.Anything).
			RunAndReturn(func(ctx context.Context, task syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
				index := int(submitCalls.Add(1))
				growingTask := task.(*syncmgr.GrowingSourceSyncTask)
				growingTask.WithChunkManager(chunkManager)
				submitted <- submittedTask{index: index, task: growingTask}
				return conc.Go(func() (struct{}, error) {
					var taskErr error
					if index == 2 {
						taskErr = growingTask.Prepare(ctx)
						close(replayPrepared)
						<-allowReplayCommit
						if taskErr == nil {
							taskErr = growingTask.Commit(ctx)
						}
						if segment, ok := mc.GetSegmentByID(1302); ok {
							stateAfterReplayCommit <- segment.State()
						}
						if taskErr != nil {
							growingTask.HandleError(taskErr)
						}
						for _, callback := range callbacks {
							taskErr = callback(taskErr)
						}
					} else {
						if index == 3 {
							<-allowDropCommit
						}
						taskErr = runGrowingTaskWithCallbacks(ctx, growingTask, callbacks)
					}
					completed <- index
					return struct{}{}, taskErr
				}), nil
			}).Times(3)

		_, msg := s.composeTextInsertMsg(1302, 10)
		insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.Require().NoError(err)
		s.Require().NoError(wb.BufferData(insertData, nil,
			&msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100))
		s.Require().NoError(wb.SealSegments(context.Background(), []int64{1302}))
		futures := l0wb.syncSegments(context.Background(), []int64{1302})
		s.Require().Len(futures, 1)
		s.ErrorContains(conc.AwaitAll(futures...), "metadata ack failed")
		first := <-submitted
		s.True(first.task.IsFlush())
		<-completed

		futures = l0wb.syncSegments(context.Background(), []int64{1302})
		s.Require().Len(futures, 1)
		replay := <-submitted
		s.True(replay.task.IsFlush())
		s.False(replay.task.IsDrop())
		<-replayPrepared
		s.Require().NoError(wb.BufferData(nil, nil,
			&msgpb.MsgPosition{Timestamp: 300}, &msgpb.MsgPosition{Timestamp: 400}, 300))
		wb.DropPartitions([]int64{0})
		close(allowReplayCommit)
		s.NoError(conc.AwaitAll(futures...))
		s.Equal(commonpb.SegmentState_Dropped, <-stateAfterReplayCommit,
			"the frozen final replay must not overwrite a later Drop with Flushed")

		drop := <-submitted
		s.Equal(3, drop.index)
		s.True(drop.task.IsDrop())
		s.EqualValues(0, drop.task.BatchRows())
		s.EqualValues(200, wb.GetCheckpoint().GetTimestamp(),
			"metadata-only Drop must keep the drop-message pack behind the durable T1 fence")
		close(allowDropCommit)
		for seen := 0; seen < 2; seen++ {
			<-completed
		}
		_, exists := mc.GetSegmentByID(1302)
		s.False(exists)
		s.NotContains(l0wb.growingSourceProgress, int64(1302))
		s.Nil(l0wb.syncCheckpoint.GetEarliestWithDefault(nil))
	})

	s.Run("drop_between_commit_and_callback_settlement_is_not_removed", func() {
		textSchema := s.textSchema()
		mc := s.newTextRealMetaCache(textSchema)
		syncManager := syncmgr.NewMockSyncManager(s.T())
		metaWriter := syncmgr.NewMockMetaWriter(s.T())
		chunkManager := storage.NewLocalChunkManager(objectstorage.RootPath(s.T().TempDir()))
		var sourceAvailable atomic.Bool
		sourceAvailable.Store(true)
		source := fakeGrowingFlushSource{
			rows: 10,
			commitFunc: func(uint64) {
				sourceAvailable.Store(false)
			},
			flushFunc: func(_ context.Context, _, _ uint64, config *syncmgr.GrowingFlushConfig) (*syncmgr.GrowingFlushResult, error) {
				return &syncmgr.GrowingFlushResult{
					ManifestPath:           "manifest-before-callback",
					NumRows:                10,
					ColumnGroupMemorySizes: fakeColumnGroupMemorySizes(config, 80),
				}, nil
			},
		}
		wb, err := NewL0WriteBuffer(s.channelName, mc, syncManager, &writeBufferOption{
			idAllocator: s.allocator,
			metaWriter:  metaWriter,
			growingSourceResolver: func(int64, *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
				if !sourceAvailable.Load() {
					return nil, syncmgr.GrowingSourceUnavailable
				}
				return source, syncmgr.GrowingSourceUsable
			},
		})
		s.Require().NoError(err)
		l0wb := wb.(*l0WriteBuffer)
		metaWriter.EXPECT().UpdateGrowingSourceSync(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask")).Return(nil).Twice()

		dropSubmitted := make(chan *syncmgr.GrowingSourceSyncTask, 1)
		dropCompleted := make(chan struct{})
		allowDropCommit := make(chan struct{})
		syncManager.EXPECT().SyncData(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask"), mock.Anything).
			RunAndReturn(func(ctx context.Context, task syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
				growingTask := task.(*syncmgr.GrowingSourceSyncTask)
				growingTask.WithChunkManager(chunkManager)
				dropSubmitted <- growingTask
				return conc.Go(func() (struct{}, error) {
					<-allowDropCommit
					err := runGrowingTaskWithCallbacks(ctx, growingTask, callbacks)
					close(dropCompleted)
					return struct{}{}, err
				}), nil
			}).Once()

		_, msg := s.composeTextInsertMsg(1303, 10)
		insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.Require().NoError(err)
		s.Require().NoError(wb.BufferData(insertData, nil,
			&msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100))
		s.Require().NoError(wb.SealSegments(context.Background(), []int64{1303}))

		l0wb.mut.Lock()
		task, err := l0wb.getSyncTask(context.Background(), 1303)
		l0wb.mut.Unlock()
		s.Require().NoError(err)
		finalTask := task.(*syncmgr.GrowingSourceSyncTask)
		finalTask.WithChunkManager(chunkManager)
		s.Require().NoError(finalTask.Prepare(context.Background()))
		s.Require().NoError(finalTask.Commit(context.Background()))
		segment, ok := mc.GetSegmentByID(1303)
		s.Require().True(ok)
		s.Equal(commonpb.SegmentState_Flushed, segment.State())

		// The Drop lands after Commit but before the sync-manager callback settles
		// progress/removes a final-flush segment.
		s.Require().NoError(wb.BufferData(nil, nil,
			&msgpb.MsgPosition{Timestamp: 300}, &msgpb.MsgPosition{Timestamp: 400}, 300))
		wb.DropPartitions([]int64{0})
		s.Require().NoError(l0wb.finishGrowingSourceSync(context.Background(), finalTask, nil))
		dropTask := <-dropSubmitted
		s.True(dropTask.IsDrop())
		s.EqualValues(0, dropTask.BatchRows())
		s.EqualValues(200, wb.GetCheckpoint().GetTimestamp())
		close(allowDropCommit)
		<-dropCompleted

		_, exists := mc.GetSegmentByID(1303)
		s.False(exists)
		s.NotContains(l0wb.growingSourceProgress, int64(1303))
		s.Nil(l0wb.syncCheckpoint.GetEarliestWithDefault(nil))
	})

	s.Run("zero_row_drop_without_source_finalization_proof_requires_source", func() {
		textSchema := s.textSchema()
		mc := s.newTextRealMetaCache(textSchema)
		mc.AddSegment(&datapb.SegmentInfo{
			ID:             1304,
			PartitionID:    0,
			CollectionID:   s.collID,
			InsertChannel:  s.channelName,
			State:          commonpb.SegmentState_Dropped,
			StorageVersion: storage.StorageV3,
		}, func(*datapb.SegmentInfo) pkoracle.PkStat {
			return pkoracle.NewBloomFilterSet()
		}, metacache.NewBM25StatsFactory)

		wb, err := NewL0WriteBuffer(s.channelName, mc, s.syncMgr, &writeBufferOption{
			idAllocator: s.allocator,
			growingSourceResolver: func(int64, *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
				return nil, syncmgr.GrowingSourceUnavailable
			},
		})
		s.Require().NoError(err)
		segment, ok := mc.GetSegmentByID(1304)
		s.Require().True(ok)
		progress := &growingSourceProgress{
			segmentID: 1304,
			owesDrop:  true,
		}

		task, err := wb.(*l0WriteBuffer).getGrowingSourceSyncTask(context.Background(), segment, progress)
		s.ErrorIs(err, errGrowingSourceUnavailable)
		s.Nil(task)
		s.False(progress.syncing)
	})

	s.Run("source_task_layout_mismatch_is_non_retryable", func() {
		textSchema := s.textSchema()
		metacache := s.newTextRealMetaCache(textSchema)
		source := fakeGrowingFlushSource{
			flushFunc: func(context.Context, uint64, uint64, *syncmgr.GrowingFlushConfig) (*syncmgr.GrowingFlushResult, error) {
				return nil, fmt.Errorf("Invalid: Column group size mismatch: existing has 10 groups, but appended has 1 groups: segcore error[segcoreCode=2001]")
			},
		}
		var errorHandlerCalls atomic.Int32
		wb, err := NewL0WriteBuffer(s.channelName, metacache, s.syncMgr, &writeBufferOption{
			idAllocator:        s.allocator,
			flushRetryInterval: time.Hour,
			errorHandler: func(error) {
				errorHandlerCalls.Add(1)
			},
			growingSourceResolver: func(segmentID int64, _ *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
				return source, syncmgr.GrowingSourceUsable
			},
		})
		s.NoError(err)

		done := make(chan struct{})
		s.syncMgr.EXPECT().SyncData(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask"), mock.Anything).
			RunAndReturn(func(ctx context.Context, task syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
				textTask := task.(*syncmgr.GrowingSourceSyncTask)
				textTask.WithChunkManager(storage.NewLocalChunkManager(objectstorage.RootPath(s.T().TempDir())))
				return conc.Go(func() (struct{}, error) {
					defer close(done)
					err := runGrowingTaskWithCallbacks(ctx, textTask, callbacks)
					return struct{}{}, err
				}), nil
			}).Once()

		_, msg := s.composeTextInsertMsg(1014, 10)
		insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.NoError(err)
		err = wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100)
		s.NoError(err)
		l0wb := wb.(*l0WriteBuffer)
		futures := l0wb.syncSegments(context.Background(), []int64{1014})
		s.Require().Len(futures, 1)
		s.ErrorContains(conc.AwaitAll(futures...), "Column group size mismatch")
		<-done

		s.EqualValues(1, errorHandlerCalls.Load())
		progress, ok := l0wb.growingSourceProgress[int64(1014)]
		s.True(ok)
		s.EqualValues(1, progress.failureCount)
		s.Contains(progress.lastFailure, "Column group size mismatch")
		s.True(progress.nonRetryableFailure)
		s.False(anyGrowingRetryArmed(l0wb.writeBufferBase))

		segments := l0wb.getGrowingSourceSegmentsToRetry(time.Now(), 0)
		s.Empty(segments)

		segment, ok := metacache.GetSegmentByID(1014)
		s.True(ok)
		s.EqualValues(0, segment.FlushedRows())
		s.EqualValues(0, segment.SyncingRows())
		s.EqualValues(10, segment.BufferRows())
	})

	s.Run("source_task_row_count_mismatch_retries_without_meta_update", func() {
		textSchema := s.textSchema()
		metacache := s.newTextRealMetaCache(textSchema)
		source := fakeGrowingFlushSource{
			flushFunc: func(_ context.Context, _, _ uint64, config *syncmgr.GrowingFlushConfig) (*syncmgr.GrowingFlushResult, error) {
				return &syncmgr.GrowingFlushResult{
					ManifestPath:           "manifest-mismatch",
					NumRows:                9,
					ColumnGroupMemorySizes: fakeColumnGroupMemorySizes(config, 80),
				}, nil
			},
		}
		metaWriter := syncmgr.NewMockMetaWriter(s.T())
		var errorHandlerCalls atomic.Int32
		var growingHandlerCalls atomic.Int32
		wb, err := NewL0WriteBuffer(s.channelName, metacache, s.syncMgr, &writeBufferOption{
			idAllocator:        s.allocator,
			metaWriter:         metaWriter,
			flushRetryInterval: time.Hour,
			errorHandler: func(error) {
				errorHandlerCalls.Add(1)
			},
			growingSourceErrorHandler: func(error) {
				growingHandlerCalls.Add(1)
			},
			growingSourceResolver: func(segmentID int64, _ *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
				return source, syncmgr.GrowingSourceUsable
			},
		})
		s.NoError(err)

		done := make(chan struct{})
		s.syncMgr.EXPECT().SyncData(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask"), mock.Anything).
			RunAndReturn(func(ctx context.Context, task syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
				textTask := task.(*syncmgr.GrowingSourceSyncTask)
				textTask.WithChunkManager(storage.NewLocalChunkManager(objectstorage.RootPath(s.T().TempDir())))
				return conc.Go(func() (struct{}, error) {
					defer close(done)
					err := runGrowingTaskWithCallbacks(ctx, textTask, callbacks)
					return struct{}{}, err
				}), nil
			}).Once()

		_, msg := s.composeTextInsertMsg(1011, 10)
		insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.NoError(err)
		err = wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100)
		s.NoError(err)
		l0wb := wb.(*l0WriteBuffer)
		futures := l0wb.syncSegments(context.Background(), []int64{1011})
		s.Require().Len(futures, 1)
		s.ErrorContains(conc.AwaitAll(futures...), "row count mismatch")
		<-done

		// A row-count mismatch is a data-integrity violation: the task
		// re-derives it from the same segment state and the same offset range,
		// so retrying reproduces it forever while pinning the checkpoint. It
		// escalates to the fatal handler instead.
		s.EqualValues(1, growingHandlerCalls.Load())
		s.EqualValues(1, errorHandlerCalls.Load())
		progress, ok := l0wb.growingSourceProgress[int64(1011)]
		s.True(ok)
		s.EqualValues(1, progress.failureCount)
		s.Contains(progress.lastFailure, "row count mismatch")
		segment, ok := metacache.GetSegmentByID(1011)
		s.True(ok)
		s.EqualValues(0, segment.FlushedRows())
		s.NotEqual("manifest-mismatch", segment.ManifestPath())
	})

	s.Run("consecutive_flushes_cover_adjacent_position_ranges", func() {
		textSchema := s.textSchema()
		metacache := s.newTextRealMetaCache(textSchema)
		var ranges [][2]uint64
		rowsPerCall := []int64{10, 5}
		source := fakeGrowingFlushSource{
			pkRows: func() int64 { return rowsPerCall[len(ranges)] },
			flushFunc: func(_ context.Context, startTs, endTs uint64, config *syncmgr.GrowingFlushConfig) (*syncmgr.GrowingFlushResult, error) {
				rows := rowsPerCall[len(ranges)]
				ranges = append(ranges, [2]uint64{startTs, endTs})
				return &syncmgr.GrowingFlushResult{
					ManifestPath:           packed.MarshalManifestPath(config.SegmentBasePath, int64(len(ranges))),
					NumRows:                rows,
					ColumnGroupMemorySizes: fakeColumnGroupMemorySizes(config, 80),
				}, nil
			},
		}
		wb, err := NewL0WriteBuffer(s.channelName, metacache, s.syncMgr, &writeBufferOption{
			idAllocator: s.allocator,
			growingSourceResolver: func(segmentID int64, _ *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
				return source, syncmgr.GrowingSourceUsable
			},
		})
		s.NoError(err)

		done := make(chan struct{}, 2)
		s.syncMgr.EXPECT().SyncData(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask"), mock.Anything).
			RunAndReturn(func(ctx context.Context, task syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
				textTask := task.(*syncmgr.GrowingSourceSyncTask)
				textTask.WithChunkManager(storage.NewLocalChunkManager(objectstorage.RootPath(s.T().TempDir())))
				return conc.Go(func() (struct{}, error) {
					err := runGrowingTaskWithCallbacks(ctx, textTask, callbacks)
					done <- struct{}{}
					return struct{}{}, err
				}), nil
			}).Twice()

		_, msg := s.composeTextInsertMsg(1008, 10)
		insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.NoError(err)
		err = wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100)
		s.NoError(err)
		l0wb := wb.(*l0WriteBuffer)
		futures := l0wb.syncSegments(context.Background(), []int64{1008})
		s.Require().Len(futures, 1)
		s.NoError(conc.AwaitAll(futures...))
		<-done

		segment, ok := metacache.GetSegmentByID(1008)
		s.True(ok)
		s.EqualValues(10, segment.FlushedRows())

		_, msg = s.composeTextInsertMsg(1008, 5)
		insertData, err = PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.NoError(err)
		err = wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 300}, &msgpb.MsgPosition{Timestamp: 400}, 100)
		s.NoError(err)
		futures = l0wb.syncSegments(context.Background(), []int64{1008})
		s.Require().Len(futures, 1)
		s.NoError(conc.AwaitAll(futures...))
		<-done

		segment, ok = metacache.GetSegmentByID(1008)
		s.True(ok)
		s.EqualValues(15, segment.FlushedRows())
		// Adjacent and non-overlapping: the second flush starts exactly where
		// the first stopped, so no row is written twice and none is skipped.
		// The fences are the packs' end positions (200, then 400).
		s.Equal([][2]uint64{{0, 200}, {200, 400}}, ranges)
	})

	s.Run("inflight_source_sync_gets_finalized_when_segment_is_flushed", func() {
		textSchema := s.textSchema()
		metacache := s.newTextRealMetaCache(textSchema)
		source := fakeGrowingFlushSource{
			flushFunc: func(_ context.Context, startTs, endTs uint64, config *syncmgr.GrowingFlushConfig) (*syncmgr.GrowingFlushResult, error) {
				return &syncmgr.GrowingFlushResult{
					ManifestPath:           fmt.Sprintf("manifest-%d-%d", startTs, endTs),
					NumRows:                10,
					ColumnGroupMemorySizes: fakeColumnGroupMemorySizes(config, 80),
				}, nil
			},
		}
		metaWriter := syncmgr.NewMockMetaWriter(s.T())
		wb, err := NewL0WriteBuffer(s.channelName, metacache, s.syncMgr, &writeBufferOption{
			idAllocator: s.allocator,
			metaWriter:  metaWriter,
			growingSourceResolver: func(segmentID int64, _ *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
				return source, syncmgr.GrowingSourceUsable
			},
		})
		s.NoError(err)

		metaWriter.EXPECT().UpdateGrowingSourceSync(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask")).
			Run(func(_ context.Context, task *syncmgr.GrowingSourceSyncTask) {
				if task.IsFlush() {
					s.EqualValues(0, task.BatchRows())
					s.Equal("manifest-0-200", task.ManifestPath())
					return
				}
				s.EqualValues(10, task.BatchRows())
				s.Equal("manifest-0-200", task.ManifestPath())
			}).Return(nil).Twice()

		firstMayRun := make(chan struct{})
		secondTaskCh := make(chan *syncmgr.GrowingSourceSyncTask, 1)
		callCount := 0
		s.syncMgr.EXPECT().SyncData(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask"), mock.Anything).
			RunAndReturn(func(ctx context.Context, task syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
				callCount++
				textTask := task.(*syncmgr.GrowingSourceSyncTask)
				textTask.WithChunkManager(storage.NewLocalChunkManager(objectstorage.RootPath(s.T().TempDir())))
				if callCount == 1 {
					return conc.Go(func() (struct{}, error) {
						<-firstMayRun
						err := runGrowingTaskWithCallbacks(ctx, textTask, callbacks)
						return struct{}{}, err
					}), nil
				}
				secondTaskCh <- textTask
				return conc.Go(func() (struct{}, error) {
					err := runGrowingTaskWithCallbacks(ctx, textTask, callbacks)
					return struct{}{}, err
				}), nil
			}).Twice()

		_, msg := s.composeTextInsertMsg(1012, 10)
		insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.NoError(err)
		err = wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100)
		s.NoError(err)

		l0wb := wb.(*l0WriteBuffer)
		futures := l0wb.syncSegments(context.Background(), []int64{1012})
		s.Require().Len(futures, 1)
		err = wb.SealSegments(context.Background(), []int64{1012})
		s.NoError(err)
		l0wb.EvictBuffer()

		close(firstMayRun)
		s.NoError(conc.AwaitAll(futures...))
		secondTask := <-secondTaskCh
		s.True(secondTask.IsFlush())
		s.EqualValues(1012, secondTask.SegmentID())
		// A re-flush of an already-drained segment carries no rows but still
		// names the position it flushes through.
		s.NotZero(secondTask.FlushThroughTs())
		s.EqualValues(0, secondTask.BatchRows())

		s.Eventually(func() bool {
			_, ok := metacache.GetSegmentByID(1012)
			return !ok
		}, time.Second, 10*time.Millisecond)
		s.NotContains(l0wb.growingSourceProgress, int64(1012))
	})

	s.Run("sealed_pending_source_waits_without_fatal_and_retries_when_usable", func() {
		textSchema := s.textSchema()
		metacache := s.newTextRealMetaCache(textSchema)
		sourceState := syncmgr.GrowingSourcePending
		wb, err := NewL0WriteBuffer(s.channelName, metacache, s.syncMgr, &writeBufferOption{
			idAllocator:        s.allocator,
			flushRetryInterval: time.Hour,
			growingSourceResolver: func(segmentID int64, _ *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
				return fakeGrowingFlushSource{}, sourceState
			},
		})
		s.NoError(err)

		_, msg := s.composeTextInsertMsg(1005, 10)
		insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.NoError(err)

		err = wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100)
		s.NoError(err)
		err = wb.SealSegments(context.Background(), []int64{1005})
		s.NoError(err)

		l0wb := wb.(*l0WriteBuffer)
		l0wb.EvictBuffer()
		segment, ok := metacache.GetSegmentByID(1005)
		s.True(ok)
		// Task construction claims a sealed segment before resolving its
		// growing source. A pending source keeps that one-way claim in Flushing
		// so the retry resumes the same fixed flush.
		s.Equal(commonpb.SegmentState_Flushing, segment.State())
		s.Contains(l0wb.growingSourceProgress, int64(1005))

		sourceState = syncmgr.GrowingSourceUsable
		taskCh := make(chan *syncmgr.GrowingSourceSyncTask, 2)
		s.syncMgr.EXPECT().SyncData(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask"), mock.Anything).
			RunAndReturn(func(ctx context.Context, task syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
				textTask := task.(*syncmgr.GrowingSourceSyncTask)
				taskCh <- textTask
				return conc.Go(func() (struct{}, error) {
					for _, callback := range callbacks {
						if err := callback(nil); err != nil {
							return struct{}{}, err
						}
					}
					return struct{}{}, nil
				}), nil
			}).Once()

		l0wb.driveGrowingSourceRetries(context.Background(), time.Now(), 0)
		textTask := <-taskCh
		s.EqualValues(1005, textTask.SegmentID())
		s.True(textTask.IsFlush())
		s.EqualValues(10, textTask.BatchRows())

		s.Eventually(func() bool {
			_, ok := metacache.GetSegmentByID(1005)
			return !ok
		}, time.Second, 10*time.Millisecond)
		s.NotContains(l0wb.growingSourceProgress, int64(1005))
	})

	s.Run("dropped_text_progress_builds_drop_source_task", func() {
		textSchema := s.textSchema()
		metacache := s.newTextRealMetaCache(textSchema)
		resolveCalls := 0
		sourceState := syncmgr.GrowingSourcePending
		wb, err := NewL0WriteBuffer(s.channelName, metacache, s.syncMgr, &writeBufferOption{
			idAllocator:        s.allocator,
			flushRetryInterval: time.Hour,
			growingSourceResolver: func(segmentID int64, _ *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
				resolveCalls++
				if resolveCalls == 1 {
					return fakeGrowingFlushSource{}, syncmgr.GrowingSourceUsable
				}
				return fakeGrowingFlushSource{}, sourceState
			},
		})
		s.NoError(err)

		_, msg := s.composeTextInsertMsg(1004, 10)
		insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.NoError(err)

		err = wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100)
		s.NoError(err)

		l0wb := wb.(*l0WriteBuffer)
		s.Contains(l0wb.growingSourceProgress, int64(1004))
		wb.DropPartitions([]int64{0})

		sourceState = syncmgr.GrowingSourceUsable
		taskCh := make(chan *syncmgr.GrowingSourceSyncTask, 1)
		s.syncMgr.EXPECT().SyncData(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask"), mock.Anything).
			RunAndReturn(func(ctx context.Context, task syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
				textTask := task.(*syncmgr.GrowingSourceSyncTask)
				taskCh <- textTask
				return conc.Go(func() (struct{}, error) {
					for _, callback := range callbacks {
						if err := callback(nil); err != nil {
							return struct{}{}, err
						}
					}
					return struct{}{}, nil
				}), nil
			}).Once()

		futures := l0wb.syncSegments(context.Background(), []int64{1004})
		s.Require().Len(futures, 1)
		s.NoError(conc.AwaitAll(futures...))

		textTask := <-taskCh
		s.True(textTask.IsDrop())
		s.EqualValues(1004, textTask.SegmentID())
	})
}

// While a growing-source flush task (T1) is in flight, a non-empty batch (T2)
// buffered mid-flight must survive T1's ack: the trim is bounded by T1's frozen
// checkpoint, so only the batches it covered are removed, and the channel
// checkpoint stays pinned at T2's start position.
func (s *L0WriteBufferSuite) TestConcurrentBufferDuringGrowingFlushKeepsMidFlightBatch() {
	paramtable.Get().Save(paramtable.Get().CommonCfg.UseLoonFFI.Key, "true")
	paramtable.Get().Save(paramtable.Get().CommonCfg.EnableGrowingSourceFlush.Key, "true")
	defer paramtable.Get().Reset(paramtable.Get().CommonCfg.UseLoonFFI.Key)
	defer paramtable.Get().Reset(paramtable.Get().CommonCfg.EnableGrowingSourceFlush.Key)

	segmentID := int64(1140)
	textSchema := s.textSchema()
	metaCache := s.newTextRealMetaCache(textSchema)
	syncManager := syncmgr.NewMockSyncManager(s.T())
	wb, err := NewL0WriteBuffer(s.channelName, metaCache, syncManager, &writeBufferOption{
		idAllocator:        s.allocator,
		flushRetryInterval: time.Hour,
		growingSourceResolver: func(int64, *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
			return fakeGrowingFlushSource{}, syncmgr.GrowingSourceUsable
		},
	})
	s.Require().NoError(err)

	_, msg := s.composeTextInsertMsg(segmentID, 10)
	insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
	s.Require().NoError(err)
	s.Require().NoError(wb.BufferData(insertData, nil,
		&msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100))

	taskInFlight := make(chan *syncmgr.GrowingSourceSyncTask, 1)
	releaseTask := make(chan struct{})
	syncManager.EXPECT().SyncData(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask"), mock.Anything).
		RunAndReturn(func(_ context.Context, task syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
			growingTask := task.(*syncmgr.GrowingSourceSyncTask)
			taskInFlight <- growingTask
			return conc.Go(func() (struct{}, error) {
				// Hold the task in flight until the test has buffered T2.
				<-releaseTask
				metaCache.UpdateSegments(
					metacache.FinishSyncing(growingTask.BatchRows()),
					metacache.WithSegmentIDs(growingTask.SegmentID()),
				)
				var callbackErr error
				for _, callback := range callbacks {
					callbackErr = callback(callbackErr)
				}
				return struct{}{}, callbackErr
			}), nil
		}).Once()

	l0wb := wb.(*l0WriteBuffer)
	futuresCh := make(chan []*conc.Future[struct{}], 1)
	go func() {
		futuresCh <- l0wb.syncSegments(context.Background(), []int64{segmentID})
	}()
	var task *syncmgr.GrowingSourceSyncTask
	select {
	case task = <-taskInFlight:
	case <-time.After(5 * time.Second):
		s.FailNow("growing-source sync task was not submitted")
	}
	s.Require().EqualValues(200, task.Checkpoint().GetTimestamp(),
		"T1 must be fenced at the last position recorded when it was built")

	// T2 arrives while T1 is still in flight.
	_, msg2 := s.composeTextInsertMsg(segmentID, 5)
	insertData2, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg2})
	s.Require().NoError(err)
	s.Require().NoError(wb.BufferData(insertData2, nil,
		&msgpb.MsgPosition{Timestamp: 300}, &msgpb.MsgPosition{Timestamp: 400}, 100))

	close(releaseTask)
	var futures []*conc.Future[struct{}]
	select {
	case futures = <-futuresCh:
	case <-time.After(5 * time.Second):
		s.FailNow("syncSegments did not return")
	}
	s.Require().Len(futures, 1)
	_, err = futures[0].Await()
	s.Require().NoError(err)

	l0wb.mut.RLock()
	progress := l0wb.growingSourceProgress[segmentID]
	s.Require().NotNil(progress)
	batches := append([]growingSourceProgressBatch{}, progress.batches...)
	lastFlushedTs := progress.lastFlushedTs
	l0wb.mut.RUnlock()

	s.Require().Len(batches, 1, "the mid-flight T2 batch must survive T1's ack trim")
	s.EqualValues(300, batches[0].startPosition.GetTimestamp())
	s.EqualValues(400, batches[0].endPosition.GetTimestamp())
	s.EqualValues(5, batches[0].rowNum)
	s.EqualValues(200, lastFlushedTs, "the ack must advance only through T1's frozen checkpoint")
	s.EqualValues(300, wb.GetCheckpoint().GetTimestamp(),
		"the channel checkpoint must stay pinned at T2's start position")
	_, exists := metaCache.GetSegmentByID(segmentID)
	s.True(exists, "a periodic (non-flush) T1 must not remove the segment while T2 is pending")
}

func (s *L0WriteBufferSuite) TestGetGrowingFlushProgress() {
	paramtable.Get().Save(paramtable.Get().CommonCfg.UseLoonFFI.Key, "true")
	paramtable.Get().Save(paramtable.Get().CommonCfg.EnableGrowingSourceFlush.Key, "true")
	defer paramtable.Get().Reset(paramtable.Get().CommonCfg.UseLoonFFI.Key)
	defer paramtable.Get().Reset(paramtable.Get().CommonCfg.EnableGrowingSourceFlush.Key)

	s.Run("synced_growing_source_segment_is_still_handoff_candidate_until_final_flush", func() {
		handoffProvider := fakeGrowingSourceProvider{}
		registration := syncmgr.DefaultGrowingSourceRegistry().Register(s.channelName, handoffProvider)
		defer syncmgr.DefaultGrowingSourceRegistry().Unregister(registration)

		textSchema := s.textSchema()
		mc := s.newTextRealMetaCache(textSchema)
		metaWriter := syncmgr.NewMockMetaWriter(s.T())
		wb, err := NewL0WriteBuffer(s.channelName, mc, s.syncMgr, &writeBufferOption{
			idAllocator: s.allocator,
			metaWriter:  metaWriter,
			growingSourceResolver: func(segmentID int64, _ *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
				return fakeGrowingFlushSource{}, syncmgr.GrowingSourceUsable
			},
		})
		s.NoError(err)

		metaWriter.EXPECT().UpdateGrowingSourceSync(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask")).Return(nil).Once()
		done := make(chan struct{})
		s.syncMgr.EXPECT().SyncData(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask"), mock.Anything).
			RunAndReturn(func(ctx context.Context, task syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
				textTask := task.(*syncmgr.GrowingSourceSyncTask)
				textTask.WithChunkManager(storage.NewLocalChunkManager(objectstorage.RootPath(s.T().TempDir())))
				return conc.Go(func() (struct{}, error) {
					defer close(done)
					err := runGrowingTaskWithCallbacks(ctx, textTask, callbacks)
					return struct{}{}, err
				}), nil
			}).Once()

		_, msg := s.composeTextInsertMsg(1200, 10)
		insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.NoError(err)
		err = wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100)
		s.NoError(err)
		l0wb := wb.(*l0WriteBuffer)
		futures := l0wb.syncSegments(context.Background(), []int64{1200})
		s.Require().Len(futures, 1)
		s.NoError(conc.AwaitAll(futures...))
		<-done

		s.Contains(l0wb.growingSourceProgress, int64(1200))
		s.Empty(l0wb.growingSourceProgress[int64(1200)].batches)

		progress, err := wb.GetGrowingFlushProgress(context.Background(), []int64{1200})
		s.NoError(err)
		s.Len(progress, 1)
		s.EqualValues(1200, progress[0].SegmentID)
		s.True(progress[0].NeedReleaseHandoff)
		s.EqualValues(200, progress[0].FlushThroughTs)
		s.Equal(metacache.FlushSourceGrowing, progress[0].SourceMode)
	})

	s.Run("dropped_growing_source_segment_is_still_handoff_candidate_until_progress_done", func() {
		handoffProvider := fakeGrowingSourceProvider{}
		registration := syncmgr.DefaultGrowingSourceRegistry().Register(s.channelName, handoffProvider)
		defer syncmgr.DefaultGrowingSourceRegistry().Unregister(registration)

		textSchema := s.textSchema()
		mc := s.newTextRealMetaCache(textSchema)
		metaWriter := syncmgr.NewMockMetaWriter(s.T())
		wb, err := NewL0WriteBuffer(s.channelName, mc, s.syncMgr, &writeBufferOption{
			idAllocator: s.allocator,
			metaWriter:  metaWriter,
			growingSourceResolver: func(segmentID int64, _ *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
				return fakeGrowingFlushSource{}, syncmgr.GrowingSourceUsable
			},
		})
		s.NoError(err)

		metaWriter.EXPECT().UpdateGrowingSourceSync(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask")).Return(nil).Once()
		done := make(chan struct{})
		s.syncMgr.EXPECT().SyncData(mock.Anything, mock.AnythingOfType("*syncmgr.GrowingSourceSyncTask"), mock.Anything).
			RunAndReturn(func(ctx context.Context, task syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
				textTask := task.(*syncmgr.GrowingSourceSyncTask)
				textTask.WithChunkManager(storage.NewLocalChunkManager(objectstorage.RootPath(s.T().TempDir())))
				return conc.Go(func() (struct{}, error) {
					defer close(done)
					err := runGrowingTaskWithCallbacks(ctx, textTask, callbacks)
					return struct{}{}, err
				}), nil
			}).Once()

		_, msg := s.composeTextInsertMsg(1203, 10)
		insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.NoError(err)
		err = wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100)
		s.NoError(err)
		l0wb := wb.(*l0WriteBuffer)
		futures := l0wb.syncSegments(context.Background(), []int64{1203})
		s.Require().Len(futures, 1)
		s.NoError(conc.AwaitAll(futures...))
		<-done

		s.Contains(l0wb.growingSourceProgress, int64(1203))
		s.Empty(l0wb.growingSourceProgress[int64(1203)].batches)

		wb.DropPartitions([]int64{0})

		progress, err := wb.GetGrowingFlushProgress(context.Background(), []int64{1203})
		s.NoError(err)
		s.Len(progress, 1)
		s.EqualValues(1203, progress[0].SegmentID)
		s.True(progress[0].NeedReleaseHandoff)
		s.EqualValues(200, progress[0].FlushThroughTs)
		s.Equal(metacache.FlushSourceGrowing, progress[0].SourceMode)
	})

	s.Run("writebuffer_backed_segment_has_no_growing_progress", func() {
		handoffProvider := fakeGrowingSourceProvider{}
		registration := syncmgr.DefaultGrowingSourceRegistry().Register(s.channelName, handoffProvider)
		defer syncmgr.DefaultGrowingSourceRegistry().Unregister(registration)

		textSchema := s.textSchema()
		mc := s.newTextRealMetaCache(textSchema)
		wb, err := NewL0WriteBuffer(s.channelName, mc, s.syncMgr, &writeBufferOption{
			idAllocator:        s.allocator,
			flushRetryInterval: time.Hour,
			growingSourceResolver: func(segmentID int64, _ *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
				return nil, syncmgr.GrowingSourceUnavailable
			},
		})
		s.NoError(err)

		_, msg := s.composeTextInsertMsg(1202, 10)
		insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
		s.NoError(err)
		err = wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100)
		s.NoError(err)

		progress, err := wb.GetGrowingFlushProgress(context.Background(), []int64{1202})
		s.NoError(err)
		s.Len(progress, 1)
		s.EqualValues(1202, progress[0].SegmentID)
		s.False(progress[0].NeedReleaseHandoff)
		s.Zero(progress[0].FlushThroughTs)
		s.Equal(metacache.FlushSourceWriteBuffer, progress[0].SourceMode)
	})

	s.Run("unknown_segment_has_no_release_handoff", func() {
		handoffProvider := fakeGrowingSourceProvider{}
		registration := syncmgr.DefaultGrowingSourceRegistry().Register(s.channelName, handoffProvider)
		defer syncmgr.DefaultGrowingSourceRegistry().Unregister(registration)

		textSchema := s.textSchema()
		mc := s.newTextRealMetaCache(textSchema)
		wb, err := NewL0WriteBuffer(s.channelName, mc, s.syncMgr, &writeBufferOption{
			idAllocator:        s.allocator,
			flushRetryInterval: time.Hour,
		})
		s.NoError(err)
		wb.SetFlushTimestamp(200)

		progress, err := wb.GetGrowingFlushProgress(context.Background(), []int64{1204})
		s.NoError(err)
		s.Len(progress, 1)
		s.EqualValues(1204, progress[0].SegmentID)
		s.False(progress[0].NeedReleaseHandoff)
		s.Zero(progress[0].FlushThroughTs)
		s.Equal(metacache.FlushSourceUnknown, progress[0].SourceMode)
	})

	s.Run("requested_segments_are_merged_with_tracked_growing_progress", func() {
		handoffProvider := fakeGrowingSourceProvider{}
		registration := syncmgr.DefaultGrowingSourceRegistry().Register(s.channelName, handoffProvider)
		defer syncmgr.DefaultGrowingSourceRegistry().Unregister(registration)

		textSchema := s.textSchema()
		mc := s.newTextRealMetaCache(textSchema)
		wb, err := NewL0WriteBuffer(s.channelName, mc, s.syncMgr, &writeBufferOption{
			idAllocator: s.allocator,
			growingSourceResolver: func(segmentID int64, _ *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
				return fakeGrowingFlushSource{}, syncmgr.GrowingSourceUsable
			},
		})
		s.NoError(err)

		for _, segmentID := range []int64{1205, 1206} {
			_, msg := s.composeTextInsertMsg(segmentID, 10)
			insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
			s.NoError(err)
			err = wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100)
			s.NoError(err)
		}

		progress, err := wb.GetGrowingFlushProgress(context.Background(), []int64{1204})
		s.NoError(err)
		progressBySegment := lo.SliceToMap(progress, func(progress GrowingFlushSegmentProgress) (int64, GrowingFlushSegmentProgress) {
			return progress.SegmentID, progress
		})
		s.Contains(progressBySegment, int64(1204))
		s.False(progressBySegment[1204].NeedReleaseHandoff)
		for _, segmentID := range []int64{1205, 1206} {
			s.True(progressBySegment[segmentID].NeedReleaseHandoff)
			s.EqualValues(200, progressBySegment[segmentID].FlushThroughTs)
			s.Equal(metacache.FlushSourceGrowing, progressBySegment[segmentID].SourceMode)
		}
	})
}

func (s *L0WriteBufferSuite) TestGrowingSourceAdmissionFence() {
	paramtable.Get().Save(paramtable.Get().CommonCfg.UseLoonFFI.Key, "true")
	paramtable.Get().Save(paramtable.Get().CommonCfg.EnableGrowingSourceFlush.Key, "true")
	defer paramtable.Get().Reset(paramtable.Get().CommonCfg.UseLoonFFI.Key)
	defer paramtable.Get().Reset(paramtable.Get().CommonCfg.EnableGrowingSourceFlush.Key)

	handoffProvider := fakeGrowingSourceProvider{}
	registration := syncmgr.DefaultGrowingSourceRegistry().Register(s.channelName, handoffProvider)
	defer syncmgr.DefaultGrowingSourceRegistry().Unregister(registration)

	textSchema := s.textSchema()
	mc := s.newTextRealMetaCache(textSchema)
	wb, err := NewL0WriteBuffer(s.channelName, mc, s.syncMgr, &writeBufferOption{
		idAllocator:        s.allocator,
		flushRetryInterval: time.Hour,
		growingSourceResolver: func(segmentID int64, _ *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
			return fakeGrowingFlushSource{}, syncmgr.GrowingSourceUsable
		},
	})
	s.NoError(err)
	l0wb := wb.(*l0WriteBuffer)

	// Segment admitted before the release fence uses the growing source.
	_, msg := s.composeTextInsertMsg(1300, 10)
	insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
	s.NoError(err)
	s.NoError(wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100))
	s.Contains(l0wb.growingSourceProgress, int64(1300))

	// Release prepare fences admission before it appends its ManualFlush.
	wb.FenceGrowingSourceAdmission()

	// A segment first seen after the fence must fall back to the write buffer:
	// its growing segment will be dropped with the unsubscribe, so admitting it
	// would strand its only data copy.
	_, msg = s.composeTextInsertMsg(1301, 10)
	insertData, err = PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
	s.NoError(err)
	s.NoError(wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 210}, &msgpb.MsgPosition{Timestamp: 220}, 100))
	s.NotContains(l0wb.growingSourceProgress, int64(1301))
	s.True(l0wb.hasWriteBufferInsertPayload(1301))

	// The pre-fence segment keeps its sticky growing decision.
	_, msg = s.composeTextInsertMsg(1300, 10)
	insertData, err = PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
	s.NoError(err)
	s.NoError(wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 230}, &msgpb.MsgPosition{Timestamp: 240}, 100))
	s.False(l0wb.hasWriteBufferInsertPayload(1300))

	// A NEW provider registration (fresh local subscription) reopens admission.
	newRegistration := syncmgr.DefaultGrowingSourceRegistry().Register(s.channelName, handoffProvider)
	defer syncmgr.DefaultGrowingSourceRegistry().Unregister(newRegistration)
	_, msg = s.composeTextInsertMsg(1302, 10)
	insertData, err = PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
	s.NoError(err)
	s.NoError(wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 250}, &msgpb.MsgPosition{Timestamp: 260}, 100))
	s.Contains(l0wb.growingSourceProgress, int64(1302))
}

// Regression: a segment that arrives AFTER the release fence must not be able
// to owe a growing-source flush, because the ManualFlush appended after the
// fence never sealed it — it would stay FlushSourceGrowing and un-Flushed, and
// the drain would block on it until the release deadline.
// Growing-source and write-buffer tasks share one segment-level gate. The
// dispatcher serializes their COMMITS, but it cannot stop a second task from
// being BUILT against counters the first has not committed yet — the two would
// then claim overlapping ranges of the same segment. getSyncTask must refuse to
// build while a sync is in flight, and it must record the debt it saw so the
// re-drive after the in-flight task settles does not lose it.
func (s *L0WriteBufferSuite) TestGetSyncTaskRefusesWhileSegmentIsSyncing() {
	paramtable.Get().Save(paramtable.Get().CommonCfg.UseLoonFFI.Key, "true")
	paramtable.Get().Save(paramtable.Get().CommonCfg.EnableGrowingSourceFlush.Key, "true")
	defer paramtable.Get().Reset(paramtable.Get().CommonCfg.UseLoonFFI.Key)
	defer paramtable.Get().Reset(paramtable.Get().CommonCfg.EnableGrowingSourceFlush.Key)

	handoffProvider := fakeGrowingSourceProvider{}
	registration := syncmgr.DefaultGrowingSourceRegistry().Register(s.channelName, handoffProvider)
	defer syncmgr.DefaultGrowingSourceRegistry().Unregister(registration)

	textSchema := s.textSchema()
	mc := s.newTextRealMetaCache(textSchema)
	wb, err := NewL0WriteBuffer(s.channelName, mc, s.syncMgr, &writeBufferOption{
		idAllocator:        s.allocator,
		flushRetryInterval: time.Hour,
		growingSourceResolver: func(segmentID int64, _ *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
			return fakeGrowingFlushSource{}, syncmgr.GrowingSourceUsable
		},
	})
	s.NoError(err)
	l0wb := wb.(*l0WriteBuffer)

	_, msg := s.composeTextInsertMsg(1340, 10)
	insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
	s.NoError(err)
	s.NoError(wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100))
	s.Require().Contains(l0wb.growingSourceProgress, int64(1340))

	// A task for 1340 is in flight, and the segment gets sealed meanwhile.
	l0wb.mut.Lock()
	l0wb.growingSourceProgress[1340].syncing = true
	l0wb.mut.Unlock()
	s.NoError(wb.SealSegments(context.Background(), []int64{1340}))

	l0wb.mut.Lock()
	task, err := l0wb.getSyncTask(context.Background(), 1340)
	progress := l0wb.growingSourceProgress[1340]
	l0wb.mut.Unlock()

	// The discriminating assertion: no second task may be built against the
	// in-flight one's counters. Without the guard a second GrowingSourceSyncTask
	// comes back here carrying the same batchRows as the task already running.
	s.NoError(err)
	s.Nil(task)
	// The debt survives to the re-drive. (sealSegments records it too, so this
	// is documentation of the invariant, not proof of the guard.)
	s.True(progress.owesFlush)

	// Once the in-flight task settles, the same call does produce a task.
	l0wb.mut.Lock()
	progress.syncing = false
	task, err = l0wb.getSyncTask(context.Background(), 1340)
	l0wb.mut.Unlock()
	s.NoError(err)
	s.NotNil(task)
}

func (s *L0WriteBufferSuite) TestPostFenceSegmentCannotBlockDrain() {
	paramtable.Get().Save(paramtable.Get().CommonCfg.UseLoonFFI.Key, "true")
	paramtable.Get().Save(paramtable.Get().CommonCfg.EnableGrowingSourceFlush.Key, "true")
	defer paramtable.Get().Reset(paramtable.Get().CommonCfg.UseLoonFFI.Key)
	defer paramtable.Get().Reset(paramtable.Get().CommonCfg.EnableGrowingSourceFlush.Key)

	handoffProvider := fakeGrowingSourceProvider{}
	registration := syncmgr.DefaultGrowingSourceRegistry().Register(s.channelName, handoffProvider)
	defer syncmgr.DefaultGrowingSourceRegistry().Unregister(registration)

	textSchema := s.textSchema()
	mc := s.newTextRealMetaCache(textSchema)
	wb, err := NewL0WriteBuffer(s.channelName, mc, s.syncMgr, &writeBufferOption{
		idAllocator:        s.allocator,
		flushRetryInterval: time.Hour,
		growingSourceResolver: func(segmentID int64, _ *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
			return fakeGrowingFlushSource{}, syncmgr.GrowingSourceUsable
		},
	})
	s.NoError(err)
	l0wb := wb.(*l0WriteBuffer)

	// Release prepare fences admission first, then appends its ManualFlush.
	wb.FenceGrowingSourceAdmission()

	// Data that reaches the buffer after the fence — exactly the window the old
	// ordering left open between the append and the fence.
	_, msg := s.composeTextInsertMsg(1330, 10)
	insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
	s.NoError(err)
	s.NoError(wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 210}, &msgpb.MsgPosition{Timestamp: 220}, 100))

	s.NotContains(l0wb.growingSourceProgress, int64(1330))
	s.True(l0wb.hasWriteBufferInsertPayload(1330))

	// Nothing owes a growing-source flush, so the release drains immediately
	// instead of blocking on an unsealed growing segment.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s.NoError(l0wb.WaitGrowingFlushDrained(ctx, nil))
}

func (s *L0WriteBufferSuite) TestWaitGrowingFlushDrainedScansAllSegments() {
	paramtable.Get().Save(paramtable.Get().CommonCfg.UseLoonFFI.Key, "true")
	paramtable.Get().Save(paramtable.Get().CommonCfg.EnableGrowingSourceFlush.Key, "true")
	defer paramtable.Get().Reset(paramtable.Get().CommonCfg.UseLoonFFI.Key)
	defer paramtable.Get().Reset(paramtable.Get().CommonCfg.EnableGrowingSourceFlush.Key)

	textSchema := s.textSchema()
	mc := s.newTextRealMetaCache(textSchema)
	wb, err := NewL0WriteBuffer(s.channelName, mc, s.syncMgr, &writeBufferOption{
		idAllocator:        s.allocator,
		flushRetryInterval: time.Hour,
		growingSourceResolver: func(segmentID int64, _ *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
			return fakeGrowingFlushSource{}, syncmgr.GrowingSourceUsable
		},
	})
	s.NoError(err)
	l0wb := wb.(*l0WriteBuffer)

	_, msg := s.composeTextInsertMsg(1310, 10)
	insertData, err := PrepareInsert(textSchema, s.pkSchema, []*msgstream.InsertMsg{msg})
	s.NoError(err)
	s.NoError(wb.BufferData(insertData, nil, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 100))
	s.Contains(l0wb.growingSourceProgress, int64(1310))

	// The wait must cover segments the caller did not list: an empty list still
	// blocks on 1310's pending growing-source flush.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = l0wb.WaitGrowingFlushDrained(ctx, nil)
	s.Error(err)
	s.ErrorContains(err, "1310")

	// Once nothing owes a flush the wait returns immediately.
	l0wb.mut.Lock()
	delete(l0wb.growingSourceProgress, 1310)
	l0wb.mut.Unlock()
	s.NoError(l0wb.WaitGrowingFlushDrained(context.Background(), nil))
}

func (s *L0WriteBufferSuite) TestGrowingSourceRegistryResolveMultipleProviders() {
	registry := syncmgr.NewGrowingSourceRegistry()
	channel := "by-dev-rootcoord-dml_1v0"

	pendingReg := registry.Register(channel, fakeGrowingSourceProvider{
		source: fakeGrowingFlushSource{tsafe: 8},
		state:  syncmgr.GrowingSourcePending,
	})
	pendingReg2 := registry.Register(channel, fakeGrowingSourceProvider{
		source: fakeGrowingFlushSource{tsafe: 9},
		state:  syncmgr.GrowingSourcePending,
	})
	registry.Register(channel, fakeGrowingSourceProvider{state: syncmgr.GrowingSourceUnavailable})

	resolved, state := registry.Resolve(channel, 1000, &msgpb.MsgPosition{Timestamp: 200})
	s.Equal(syncmgr.GrowingSourcePending, state)
	s.Nil(resolved)

	usableReg := registry.Register(channel, fakeGrowingSourceProvider{
		source: fakeGrowingFlushSource{tsafe: 10},
		state:  syncmgr.GrowingSourceUsable,
	})
	usableReg2 := registry.Register(channel, fakeGrowingSourceProvider{
		source: fakeGrowingFlushSource{tsafe: 12},
		state:  syncmgr.GrowingSourceUsable,
	})
	resolved, state = registry.Resolve(channel, 1000, &msgpb.MsgPosition{Timestamp: 200})
	s.Equal(syncmgr.GrowingSourceUsable, state)
	s.NotNil(resolved)
	s.EqualValues(10, resolved.TSafe())

	registry.Unregister(pendingReg)
	registry.Unregister(pendingReg2)
	resolved, state = registry.Resolve(channel, 1000, &msgpb.MsgPosition{Timestamp: 200})
	s.Equal(syncmgr.GrowingSourceUsable, state)
	s.NotNil(resolved)
	s.EqualValues(10, resolved.TSafe())

	registry.Unregister(usableReg2)
	resolved, state = registry.Resolve(channel, 1000, &msgpb.MsgPosition{Timestamp: 200})
	s.Equal(syncmgr.GrowingSourceUsable, state)
	s.NotNil(resolved)
	s.EqualValues(10, resolved.TSafe())

	registry.Unregister(usableReg)
	resolved, state = registry.Resolve(channel, 1000, &msgpb.MsgPosition{Timestamp: 200})
	s.Equal(syncmgr.GrowingSourceUnavailable, state)
	s.Nil(resolved)
}

func (s *L0WriteBufferSuite) TestGrowingSourceRegistryResolveNilPendingProvider() {
	registry := syncmgr.NewGrowingSourceRegistry()
	channel := "by-dev-rootcoord-dml_1v0"

	registry.Register(channel, fakeGrowingSourceProvider{state: syncmgr.GrowingSourcePending})
	registry.Register(channel, fakeGrowingSourceProvider{state: syncmgr.GrowingSourceUnavailable})

	resolved, state := registry.Resolve(channel, 1000, &msgpb.MsgPosition{Timestamp: 200})
	s.Equal(syncmgr.GrowingSourcePending, state)
	s.Nil(resolved)
}

func (s *L0WriteBufferSuite) TestGrowingSourceRegistryResolveReleasesUnselectedSources() {
	registry := syncmgr.NewGrowingSourceRegistry()
	channel := "by-dev-rootcoord-dml_1v0"
	released := 0

	registry.Register(channel, fakeGrowingSourceProvider{
		source: fakeGrowingFlushSource{tsafe: 10, releaseFunc: func() { released++ }},
		state:  syncmgr.GrowingSourceUsable,
	})
	registry.Register(channel, fakeGrowingSourceProvider{
		source: fakeGrowingFlushSource{tsafe: 12, releaseFunc: func() { released++ }},
		state:  syncmgr.GrowingSourceUsable,
	})
	registry.Register(channel, fakeGrowingSourceProvider{
		source: fakeGrowingFlushSource{tsafe: 11, releaseFunc: func() { released++ }},
		state:  syncmgr.GrowingSourcePending,
	})

	resolved, state := registry.Resolve(channel, 1000, &msgpb.MsgPosition{Timestamp: 200})
	s.Equal(syncmgr.GrowingSourceUsable, state)
	s.NotNil(resolved)
	s.EqualValues(10, resolved.TSafe())
	s.Equal(0, released)
}

func TestL0WriteBuffer(t *testing.T) {
	suite.Run(t, new(L0WriteBufferSuite))
}
