package writebuffer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/suite"

	"github.com/milvus-io/milvus-proto/go-api/v3/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v3/msgpb"
	"github.com/milvus-io/milvus-proto/go-api/v3/schemapb"
	"github.com/milvus-io/milvus/internal/flushcommon/metacache"
	"github.com/milvus-io/milvus/internal/flushcommon/metacache/pkoracle"
	"github.com/milvus-io/milvus/internal/flushcommon/syncmgr"
	"github.com/milvus-io/milvus/pkg/v3/common"
	"github.com/milvus-io/milvus/pkg/v3/proto/datapb"
	"github.com/milvus-io/milvus/pkg/v3/util/merr"
	"github.com/milvus-io/milvus/pkg/v3/util/paramtable"
)

// SealCheckpointPinSuite covers the [seal -> sync task construction] window: a
// segment whose write buffer was already drained by an earlier periodic flush is
// sealed by a ManualFlush fence, and nothing else pins the channel checkpoint
// until the terminal metadata-only task exists.
type SealCheckpointPinSuite struct {
	suite.Suite

	collID      int64
	channelName string
	collSchema  *schemapb.CollectionSchema
	metacache   metacache.MetaCache
	syncMgr     *syncmgr.MockSyncManager
	wb          *writeBufferBase
}

func (s *SealCheckpointPinSuite) SetupSuite() {
	paramtable.Get().Init(paramtable.NewBaseTable())
	s.collID = 100
	s.channelName = "by-dev-rootcoord-dml_0v0"
	s.collSchema = &schemapb.CollectionSchema{
		Name: "seal_pin_collection",
		Fields: []*schemapb.FieldSchema{
			{FieldID: 100, DataType: schemapb.DataType_Int64, IsPrimaryKey: true, Name: "pk"},
			{FieldID: 101, DataType: schemapb.DataType_FloatVector, TypeParams: []*commonpb.KeyValuePair{
				{Key: common.DimKey, Value: "128"},
			}},
		},
	}
}

func (s *SealCheckpointPinSuite) SetupTest() {
	s.metacache = metacache.NewMetaCache(&datapb.ChannelWatchInfo{
		Schema: s.collSchema,
		Vchan: &datapb.VchannelInfo{
			CollectionID: s.collID,
			ChannelName:  s.channelName,
		},
	}, func(*datapb.SegmentInfo) pkoracle.PkStat {
		return pkoracle.NewBloomFilterSet()
	}, metacache.NewBM25StatsFactory)
	s.syncMgr = syncmgr.NewMockSyncManager(s.T())

	var err error
	s.wb, err = newWriteBufferBase(s.channelName, s.metacache, s.syncMgr, &writeBufferOption{
		metaWriter: syncmgr.NewMockMetaWriter(s.T()),
	})
	s.Require().NoError(err)
	s.wb.allowGrowingSourceFlush = false
}

// addGrowingSegment registers a drained growing segment: it has rows in binlogs
// already (an earlier periodic flush wrote them) and nothing left in the write
// buffer.
func (s *SealCheckpointPinSuite) addGrowingSegment(segmentID int64) {
	s.metacache.AddSegment(&datapb.SegmentInfo{
		ID:            segmentID,
		PartitionID:   10,
		CollectionID:  s.collID,
		InsertChannel: s.channelName,
		State:         commonpb.SegmentState_Growing,
	}, func(*datapb.SegmentInfo) pkoracle.PkStat {
		return pkoracle.NewBloomFilterSet()
	}, metacache.NewBM25StatsFactory)
}

func (s *SealCheckpointPinSuite) setConsumedCheckpoint(ts uint64) {
	s.wb.mut.Lock()
	defer s.wb.mut.Unlock()
	s.wb.checkpoint = &msgpb.MsgPosition{ChannelName: s.channelName, Timestamp: ts}
}

// TestSealPinsCheckpointBeforeTaskExists is the reproduction: the fence sealed
// the segment, but no sync task was built yet. Nothing may let the channel
// checkpoint advance past the fence position in this window.
func (s *SealCheckpointPinSuite) TestSealPinsCheckpointBeforeTaskExists() {
	const segmentID = int64(2001)
	s.addGrowingSegment(segmentID)
	s.setConsumedCheckpoint(100)

	s.Require().NoError(s.wb.SealSegments(context.Background(), []int64{segmentID}))

	// Timeticks keep flowing; no sync round has run.
	s.setConsumedCheckpoint(500)

	s.EqualValues(100, s.wb.GetCheckpoint().GetTimestamp(),
		"checkpoint must not advance past the fence that sealed the segment")
}

// TestPinHoldsAcrossTaskLifetime covers the in-flight terminal task and a
// retryable failure of it: neither may release the fence pin.
func (s *SealCheckpointPinSuite) TestPinHoldsAcrossTaskLifetime() {
	const segmentID = int64(2002)
	s.addGrowingSegment(segmentID)
	s.setConsumedCheckpoint(100)
	s.Require().NoError(s.wb.SealSegments(context.Background(), []int64{segmentID}))
	s.setConsumedCheckpoint(500)

	s.wb.mut.Lock()
	task, err := s.wb.getSyncTask(context.Background(), segmentID)
	s.Require().NoError(err)
	syncTask := task.(*syncmgr.SyncTask)
	entry := s.wb.writeBufferSyncEntryLocked(syncTask)
	s.Require().NotNil(entry)
	s.wb.mut.Unlock()

	s.True(syncTask.IsFlush())
	s.EqualValues(100, s.wb.GetCheckpoint().GetTimestamp(), "in-flight terminal task keeps the fence pinned")

	// Retryable failure: the flush still owes a commit, so the pin stays.
	err = s.wb.finishWriteBufferSync(context.Background(), entry, syncTask, merr.WrapErrIoFailedReason("transient s3 failure"))
	s.Error(err)
	s.setConsumedCheckpoint(900)
	s.EqualValues(100, s.wb.GetCheckpoint().GetTimestamp(), "a retryable failure keeps the fence pinned")

	segment, ok := s.metacache.GetSegmentByID(segmentID)
	s.Require().True(ok)
	s.Equal(commonpb.SegmentState_Flushing, segment.State())
	s.EqualValues(100, segment.PendingFlushCheckpoint().GetTimestamp())
}

// TestPinReleasedOnFlushCommit: the flush commit removes the segment from the
// metacache, so the derived candidate disappears with it.
func (s *SealCheckpointPinSuite) TestPinReleasedOnFlushCommit() {
	const segmentID = int64(2003)
	s.addGrowingSegment(segmentID)
	s.setConsumedCheckpoint(100)
	s.Require().NoError(s.wb.SealSegments(context.Background(), []int64{segmentID}))

	s.wb.mut.Lock()
	task, err := s.wb.getSyncTask(context.Background(), segmentID)
	s.Require().NoError(err)
	syncTask := task.(*syncmgr.SyncTask)
	entry := s.wb.writeBufferSyncEntryLocked(syncTask)
	s.Require().NotNil(entry)
	s.wb.mut.Unlock()

	s.setConsumedCheckpoint(500)
	s.Require().NoError(s.wb.finishWriteBufferSync(context.Background(), entry, syncTask, nil))

	_, ok := s.metacache.GetSegmentByID(segmentID)
	s.False(ok, "a committed flush removes the segment")
	s.EqualValues(500, s.wb.GetCheckpoint().GetTimestamp(), "the checkpoint advances once the flush is durable")
}

// TestPinReleasedOnDrop: a dropped segment owes no flush any more, so the
// derived candidate stops matching without anyone unregistering it.
func (s *SealCheckpointPinSuite) TestPinReleasedOnDrop() {
	const segmentID = int64(2004)
	s.addGrowingSegment(segmentID)
	s.setConsumedCheckpoint(100)
	s.Require().NoError(s.wb.SealSegments(context.Background(), []int64{segmentID}))
	s.setConsumedCheckpoint(500)
	s.EqualValues(100, s.wb.GetCheckpoint().GetTimestamp())

	s.wb.mut.Lock()
	s.wb.dropPartitions([]int64{10})
	s.wb.mut.Unlock()

	segment, ok := s.metacache.GetSegmentByID(segmentID)
	s.Require().True(ok)
	s.Equal(commonpb.SegmentState_Dropped, segment.State())
	s.EqualValues(500, s.wb.GetCheckpoint().GetTimestamp(), "a dropped segment owes no flush and must not pin")
}

// TestReSealDoesNotMovePinLater: the earliest un-committed fence is the one
// recovery has to replay from.
func (s *SealCheckpointPinSuite) TestReSealDoesNotMovePinLater() {
	const segmentID = int64(2005)
	s.addGrowingSegment(segmentID)
	s.setConsumedCheckpoint(100)
	s.Require().NoError(s.wb.SealSegments(context.Background(), []int64{segmentID}))

	s.setConsumedCheckpoint(700)
	s.Require().NoError(s.wb.SealSegments(context.Background(), []int64{segmentID}))

	segment, ok := s.metacache.GetSegmentByID(segmentID)
	s.Require().True(ok)
	s.EqualValues(100, segment.PendingFlushCheckpoint().GetTimestamp())
	s.EqualValues(100, s.wb.GetCheckpoint().GetTimestamp(), "a re-seal must not push the pin forward")
}

// TestSealAllSegmentsPins: FlushAll / AlterWAL seal every growing segment and
// have the same crash window as a targeted seal.
func (s *SealCheckpointPinSuite) TestSealAllSegmentsPins() {
	const segmentID = int64(2006)
	s.addGrowingSegment(segmentID)
	s.setConsumedCheckpoint(100)

	s.wb.SealAllSegments(context.Background())

	s.setConsumedCheckpoint(500)
	s.EqualValues(100, s.wb.GetCheckpoint().GetTimestamp())
}

// TestNoStalePinAfterSequenceOfOps is the leak invariant: after seal/flush,
// seal/drop and a close, no checkpoint candidate may reference a segment that is
// no longer in the metacache, and the checkpoint must not be stuck.
func (s *SealCheckpointPinSuite) TestNoStalePinAfterSequenceOfOps() {
	ctx := context.Background()

	runFlush := func(segmentID int64, sealTs, commitTs uint64) {
		s.addGrowingSegment(segmentID)
		s.setConsumedCheckpoint(sealTs)
		s.Require().NoError(s.wb.SealSegments(ctx, []int64{segmentID}))
		s.wb.mut.Lock()
		task, err := s.wb.getSyncTask(ctx, segmentID)
		s.Require().NoError(err)
		syncTask := task.(*syncmgr.SyncTask)
		entry := s.wb.writeBufferSyncEntryLocked(syncTask)
		s.Require().NotNil(entry)
		s.wb.mut.Unlock()
		s.setConsumedCheckpoint(commitTs)
		s.Require().NoError(s.wb.finishWriteBufferSync(ctx, entry, syncTask, nil))
	}

	// seal + flush success
	runFlush(3001, 100, 200)
	// seal + drop
	s.addGrowingSegment(3002)
	s.setConsumedCheckpoint(300)
	s.Require().NoError(s.wb.SealSegments(ctx, []int64{3002}))
	s.wb.mut.Lock()
	s.wb.dropPartitions([]int64{10})
	s.metacache.RemoveSegments(metacache.WithSegmentIDs(3002))
	s.wb.mut.Unlock()
	// seal + flush success again
	runFlush(3003, 400, 500)

	s.setConsumedCheckpoint(900)

	// No candidate may outlive its segment.
	for _, segment := range s.metacache.GetSegmentsBy() {
		s.Fail("no segment should remain", "segment %d still in metacache", segment.SegmentID())
	}
	s.Nil(s.wb.syncCheckpoint.GetEarliestWithDefault(nil), "no sync entry pin may outlive its task")
	s.EqualValues(900, s.wb.GetCheckpoint().GetTimestamp(), "the checkpoint must not be stuck behind a vanished segment")

	s.wb.Close(ctx, false)
	s.EqualValues(900, s.wb.GetCheckpoint().GetTimestamp())
}

func TestSealCheckpointPin(t *testing.T) {
	suite.Run(t, new(SealCheckpointPinSuite))
}
