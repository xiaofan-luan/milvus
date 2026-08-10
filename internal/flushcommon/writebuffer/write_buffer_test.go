package writebuffer

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"

	"github.com/milvus-io/milvus-proto/go-api/v3/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v3/msgpb"
	"github.com/milvus-io/milvus-proto/go-api/v3/schemapb"
	"github.com/milvus-io/milvus/internal/allocator"
	"github.com/milvus-io/milvus/internal/flushcommon/metacache"
	"github.com/milvus-io/milvus/internal/flushcommon/syncmgr"
	"github.com/milvus-io/milvus/internal/storage"
	"github.com/milvus-io/milvus/internal/util/function"
	"github.com/milvus-io/milvus/internal/util/streamingutil"
	"github.com/milvus-io/milvus/pkg/v3/common"
	"github.com/milvus-io/milvus/pkg/v3/metrics"
	"github.com/milvus-io/milvus/pkg/v3/mq/msgstream"
	"github.com/milvus-io/milvus/pkg/v3/proto/datapb"
	"github.com/milvus-io/milvus/pkg/v3/util/conc"
	"github.com/milvus-io/milvus/pkg/v3/util/merr"
	"github.com/milvus-io/milvus/pkg/v3/util/paramtable"
	"github.com/milvus-io/milvus/pkg/v3/util/tsoutil"
)

type WriteBufferSuite struct {
	suite.Suite
	collID      int64
	channelName string
	collSchema  *schemapb.CollectionSchema
	wb          *writeBufferBase
	syncMgr     *syncmgr.MockSyncManager
	metacache   *metacache.MockMetaCache
}

type materializeTestInsertMessage struct {
	body *msgpb.InsertRequest
}

type testSyncTaskAdmission struct {
	mu                sync.Mutex
	held              bool
	inFlight          bool
	releaseRequested  bool
	submitCalls       atomic.Int32
	physicalReleases  atomic.Int32
	onPhysicalRelease func()
	onSubmit          func(context.Context, syncmgr.Task, ...func(error) error) (*conc.Future[struct{}], error)
}

func newTestSyncTaskAdmission(
	onSubmit func(context.Context, syncmgr.Task, ...func(error) error) (*conc.Future[struct{}], error),
) *testSyncTaskAdmission {
	return &testSyncTaskAdmission{held: true, onSubmit: onSubmit}
}

func (a *testSyncTaskAdmission) Submit(ctx context.Context, task syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
	a.mu.Lock()
	if !a.held || a.inFlight {
		a.mu.Unlock()
		return nil, merr.WrapErrServiceInternalMsg("test admission unavailable")
	}
	a.inFlight = true
	a.mu.Unlock()
	a.submitCalls.Add(1)
	callbacks = append(callbacks, func(err error) error {
		a.finishAttempt()
		return err
	})
	future, err := a.onSubmit(ctx, task, callbacks...)
	if err != nil {
		a.finishAttempt()
	}
	return future, err
}

func (a *testSyncTaskAdmission) Release() {
	a.mu.Lock()
	if !a.held || a.releaseRequested {
		a.mu.Unlock()
		return
	}
	if a.inFlight {
		a.releaseRequested = true
		a.mu.Unlock()
		return
	}
	a.held = false
	a.mu.Unlock()
	a.physicalRelease()
}

func (a *testSyncTaskAdmission) finishAttempt() {
	a.mu.Lock()
	a.inFlight = false
	shouldRelease := a.held && a.releaseRequested
	if shouldRelease {
		a.held = false
	}
	a.mu.Unlock()
	if shouldRelease {
		a.physicalRelease()
	}
}

func (a *testSyncTaskAdmission) physicalRelease() {
	a.physicalReleases.Add(1)
	if a.onPhysicalRelease != nil {
		a.onPhysicalRelease()
	}
}

type reservableTestSyncManager struct {
	syncmgr.SyncManager
	reserveStarted chan struct{}
	allowReserve   <-chan struct{}
	admission      *testSyncTaskAdmission
	reserveOnce    sync.Once
	reserveCalls   atomic.Int32
}

func (m *reservableTestSyncManager) ReserveSyncTask(ctx context.Context) (syncmgr.SyncTaskAdmission, error) {
	m.reserveCalls.Add(1)
	m.reserveOnce.Do(func() { close(m.reserveStarted) })
	if m.allowReserve != nil {
		select {
		case <-m.allowReserve:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return m.admission, nil
}

type boundedReservableTestSyncManager struct {
	syncmgr.SyncManager
	slots          chan struct{}
	reserveStarted chan int
	reserveCalls   atomic.Int32
	mu             sync.Mutex
	admissions     []*testSyncTaskAdmission
	onSubmit       func(context.Context, syncmgr.Task, ...func(error) error) (*conc.Future[struct{}], error)
}

func (m *boundedReservableTestSyncManager) ReserveSyncTask(ctx context.Context) (syncmgr.SyncTaskAdmission, error) {
	call := int(m.reserveCalls.Add(1))
	select {
	case m.reserveStarted <- call:
	default:
	}
	select {
	case m.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	admission := newTestSyncTaskAdmission(m.onSubmit)
	admission.onPhysicalRelease = func() { <-m.slots }
	m.mu.Lock()
	m.admissions = append(m.admissions, admission)
	m.mu.Unlock()
	return admission, nil
}

func (m *materializeTestInsertMessage) MustBody() *msgpb.InsertRequest {
	return m.body
}

func (m *materializeTestInsertMessage) OverwriteBody(body *msgpb.InsertRequest) {
	m.body = body
}

func (s *WriteBufferSuite) SetupSuite() {
	paramtable.Get().Init(paramtable.NewBaseTable())
	s.collID = 100
	s.collSchema = &schemapb.CollectionSchema{
		Name: "wb_base_collection",
		Fields: []*schemapb.FieldSchema{
			{FieldID: 100, DataType: schemapb.DataType_Int64, IsPrimaryKey: true, Name: "pk"},
			{FieldID: 101, DataType: schemapb.DataType_FloatVector, TypeParams: []*commonpb.KeyValuePair{
				{Key: common.DimKey, Value: "128"},
			}},
		},
	}
	s.channelName = "by-dev-rootcoord-dml_0v0"
}

func (s *WriteBufferSuite) SetupTest() {
	s.syncMgr = syncmgr.NewMockSyncManager(s.T())
	s.metacache = metacache.NewMockMetaCache(s.T())
	s.metacache.EXPECT().GetSchema(mock.Anything).Return(s.collSchema).Maybe()
	s.metacache.EXPECT().Collection().Return(s.collID).Maybe()
	// GetCheckpoint derives a candidate from segments that still owe a sealed
	// flush; with a mock metacache there are none.
	s.metacache.EXPECT().GetSegmentsBy(mock.Anything).Return(nil).Maybe()
	var err error
	s.wb, err = newWriteBufferBase(s.channelName, s.metacache, s.syncMgr, &writeBufferOption{})
	s.Require().NoError(err)
}

func (s *WriteBufferSuite) TestHasSegment() {
	segmentID := int64(1001)

	s.wb.allowGrowingSourceFlush = false
	s.False(s.wb.HasSegment(segmentID))

	s.wb.getOrCreateBuffer(segmentID, 0)

	s.True(s.wb.HasSegment(segmentID))
}

func (s *WriteBufferSuite) TestWriteBufferSyncSyncWindowBlocksSixthTask() {
	segmentID := int64(1101)
	segment := metacache.NewSegmentInfo(&datapb.SegmentInfo{
		ID:    segmentID,
		State: commonpb.SegmentState_Growing,
	}, nil, nil, metacache.NewEmptySegmentStats())
	s.metacache.EXPECT().GetSegmentByID(segmentID).Return(segment, true).Once()
	s.wb.mut.Lock()
	for i := 0; i < segmentReorderWindow; i++ {
		s.wb.registerWriteBufferSyncLocked(newTestSyncEntry(segmentID, uint64(200+i), entrySubmitted))
	}
	s.wb.buffers[segmentID] = &segmentBuffer{
		segmentID:    segmentID,
		insertBuffer: &InsertBuffer{},
		deltaBuffer:  &DeltaBuffer{},
	}
	tasks := s.wb.getSyncTasksLocked(context.Background(), []int64{segmentID})
	_, buffered := s.wb.buffers[segmentID]
	s.wb.mut.Unlock()

	s.Empty(tasks)
	s.True(s.wb.writeBufferSyncQueues[segmentID].intent.owes)
	s.True(buffered, "the sixth batch must stay buffered until the reorder window advances")
}

func (s *WriteBufferSuite) TestWriteBufferSyncPendingAdmissionPreservesTaskOrder() {
	segmentID := int64(1118)
	segment := metacache.NewSegmentInfo(&datapb.SegmentInfo{
		ID:             segmentID,
		State:          commonpb.SegmentState_Growing,
		StorageVersion: storage.StorageV2,
	}, nil, nil, metacache.NewEmptySegmentStats())
	s.metacache.EXPECT().GetSegmentByID(segmentID).Return(segment, true)

	state := newTestSyncEntry(segmentID, 200)
	buffer, err := newSegmentBuffer(segmentID, s.collSchema)
	s.Require().NoError(err)
	s.wb.mut.Lock()
	s.wb.registerWriteBufferSyncLocked(state)
	s.wb.buffers[segmentID] = buffer
	tasks := s.wb.getSyncTasksLocked(context.Background(), []int64{segmentID})
	_, buffered := s.wb.buffers[segmentID]
	queuePending := s.wb.writeBufferSyncQueues[segmentID].intent.owes
	s.wb.mut.Unlock()

	s.Empty(tasks)
	s.True(queuePending)
	s.True(buffered, "task7 must stay buffered until task6 finishes dispatcher admission")

	// Once the pending entry finishes dispatcher admission, the deferred batch
	// may yield — and its task must take its place BEHIND the pending one, so
	// the per-segment replay order is preserved.
	s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Return().Once()
	s.wb.mut.Lock()
	state.submitted = true
	tasks = s.wb.getSyncTasksLocked(context.Background(), []int64{segmentID})
	entries := append([]*writeBufferSyncEntry{}, s.wb.writeBufferSyncQueues[segmentID].entries...)
	s.wb.mut.Unlock()

	s.Require().Len(tasks, 1)
	s.Require().Len(entries, 2)
	s.Same(state, entries[0], "the pending task must keep the head of the queue")
	s.Same(tasks[0], entries[1].task, "the new task must be queued behind the pending one")
}

func (s *WriteBufferSuite) TestSyncSegmentsReservesBeforeYieldWithoutHoldingMutex() {
	segmentID := int64(1120)
	segment := growingSegment(segmentID, storage.StorageV2)
	s.metacache.EXPECT().GetSegmentByID(segmentID).Return(segment, true).Maybe()
	s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Return().Maybe()

	buffer, err := newSegmentBuffer(segmentID, s.collSchema)
	s.Require().NoError(err)
	s.wb.buffers[segmentID] = buffer
	s.wb.checkpoint = &msgpb.MsgPosition{Timestamp: 200}

	allowReserve := make(chan struct{})
	admission := newTestSyncTaskAdmission(
		func(_ context.Context, task syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
			// Prepare normally releases payload ownership before callbacks run.
			// This test only needs the admission handoff, so settle it directly.
			task.(*syncmgr.SyncTask).Abandon()
			var callbackErr error
			for _, callback := range callbacks {
				callbackErr = callback(callbackErr)
			}
			return completedSyncFuture(callbackErr), nil
		})
	reservable := &reservableTestSyncManager{
		SyncManager:    s.syncMgr,
		reserveStarted: make(chan struct{}),
		allowReserve:   allowReserve,
		admission:      admission,
	}
	s.wb.syncMgr = reservable

	done := make(chan []*conc.Future[struct{}], 1)
	go func() {
		done <- s.wb.syncSegments(context.Background(), []int64{segmentID})
	}()
	select {
	case <-reservable.reserveStarted:
	case <-time.After(time.Second):
		s.FailNow("syncSegments did not reserve admission")
	}

	type bufferProbe struct {
		buffered bool
		queued   bool
	}
	lockProbe := make(chan bufferProbe, 1)
	go func() {
		s.wb.mut.Lock()
		defer s.wb.mut.Unlock()
		_, buffered := s.wb.buffers[segmentID]
		_, queued := s.wb.writeBufferSyncQueues[segmentID]
		lockProbe <- bufferProbe{buffered: buffered, queued: queued}
	}()
	select {
	case probe := <-lockProbe:
		s.True(probe.buffered, "payload must not be yielded while admission is blocked")
		s.False(probe.queued)
	case <-time.After(time.Second):
		s.FailNow("ReserveSyncTask blocked while holding the write-buffer mutex")
	}
	select {
	case <-done:
		s.FailNow("syncSegments returned before reservation was granted")
	default:
	}

	close(allowReserve)
	var futures []*conc.Future[struct{}]
	select {
	case futures = <-done:
	case <-time.After(time.Second):
		s.FailNow("syncSegments did not continue after reservation was granted")
	}
	s.Require().Len(futures, 1)
	_, err = futures[0].Await()
	s.NoError(err)
	s.EqualValues(1, admission.submitCalls.Load())
	s.EqualValues(1, admission.physicalReleases.Load(), "terminal completion must release the payload lease")
	s.NotContains(s.wb.buffers, segmentID)
}

func (s *WriteBufferSuite) TestSyncSegmentsRecheckReleasesClosedReservation() {
	segmentID := int64(1121)
	buffer, err := newSegmentBuffer(segmentID, s.collSchema)
	s.Require().NoError(err)
	s.wb.buffers[segmentID] = buffer

	allowReserve := make(chan struct{})
	admission := newTestSyncTaskAdmission(
		func(context.Context, syncmgr.Task, ...func(error) error) (*conc.Future[struct{}], error) {
			s.FailNow("closed recheck must not submit a task")
			return nil, nil
		})
	reservable := &reservableTestSyncManager{
		SyncManager:    s.syncMgr,
		reserveStarted: make(chan struct{}),
		allowReserve:   allowReserve,
		admission:      admission,
	}
	s.wb.syncMgr = reservable

	done := make(chan []*conc.Future[struct{}], 1)
	go func() {
		done <- s.wb.syncSegments(context.Background(), []int64{segmentID})
	}()
	select {
	case <-reservable.reserveStarted:
	case <-time.After(time.Second):
		s.FailNow("syncSegments did not reserve admission")
	}
	s.wb.mut.Lock()
	s.wb.closed = true
	s.wb.mut.Unlock()
	close(allowReserve)

	select {
	case futures := <-done:
		s.Empty(futures)
	case <-time.After(time.Second):
		s.FailNow("closed lifecycle recheck did not return")
	}
	s.Zero(admission.submitCalls.Load())
	s.EqualValues(1, admission.physicalReleases.Load())
	s.Contains(s.wb.buffers, segmentID, "closed recheck must not yield payload")
}

func (s *WriteBufferSuite) TestSyncSegmentsNoTaskReleasesReservation() {
	segmentID := int64(1122)
	s.metacache.EXPECT().GetSegmentByID(segmentID).Return(nil, false).Once()
	admission := newTestSyncTaskAdmission(
		func(context.Context, syncmgr.Task, ...func(error) error) (*conc.Future[struct{}], error) {
			s.FailNow("no-task path must not submit")
			return nil, nil
		})
	reservable := &reservableTestSyncManager{
		SyncManager:    s.syncMgr,
		reserveStarted: make(chan struct{}),
		admission:      admission,
	}
	s.wb.syncMgr = reservable

	futures := s.wb.syncSegments(context.Background(), []int64{segmentID})
	s.Empty(futures)
	s.Zero(admission.submitCalls.Load())
	s.EqualValues(1, admission.physicalReleases.Load())
}

// The LATE-reservation scenario (a slot granted just as the bound expired,
// with nobody left to release it) no longer exists: reserveSyncTask calls
// ReserveSyncTask directly, and the sync manager's acquire releases the slot
// itself when a successful acquire races cancellation — an error return never
// holds a slot. What must survive is the timedOut contract: the bound firing
// returns (nil, true, nil) so the caller keeps its payload buffered.
func (s *WriteBufferSuite) TestAdmissionTimeoutKeepsPayloadBuffered() {
	params := paramtable.Get()
	s.Require().NoError(params.Save(params.DataNodeCfg.GracefulStopTimeout.Key, "0"))
	s.T().Cleanup(func() { params.Reset(params.DataNodeCfg.GracefulStopTimeout.Key) })

	// No free slot, ever: the reserve blocks until the bound cancels it.
	manager := &boundedReservableTestSyncManager{
		SyncManager:    s.syncMgr,
		slots:          make(chan struct{}),
		reserveStarted: make(chan int, 1),
	}
	s.wb.syncMgr = manager

	reserved, timedOut, err := s.wb.reserveSyncTask(context.Background())
	s.NoError(err)
	s.True(timedOut)
	s.Nil(reserved)
}

// A caller whose OWN context is done gets the error back, never timedOut: the
// caller's cancellation must not be misread as keep-payload-buffered.
func (s *WriteBufferSuite) TestAdmissionCallerCancelReturnsError() {
	manager := &boundedReservableTestSyncManager{
		SyncManager:    s.syncMgr,
		slots:          make(chan struct{}),
		reserveStarted: make(chan int, 1),
	}
	s.wb.syncMgr = manager

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reserved, timedOut, err := s.wb.reserveSyncTask(ctx)
	s.ErrorIs(err, context.Canceled)
	s.False(timedOut)
	s.Nil(reserved)
}

func (s *WriteBufferSuite) TestRetryableParkedTaskRetainsAdmissionAndBlocksNewYield() {
	firstSegmentID := int64(1123)
	secondSegmentID := int64(1124)
	segments := map[int64]*metacache.SegmentInfo{
		firstSegmentID:  growingSegment(firstSegmentID, storage.StorageV2),
		secondSegmentID: growingSegment(secondSegmentID, storage.StorageV2),
	}
	for segmentID, segment := range segments {
		s.metacache.EXPECT().GetSegmentByID(segmentID).Return(segment, true).Maybe()
		buffer, err := newSegmentBuffer(segmentID, s.collSchema)
		s.Require().NoError(err)
		s.wb.buffers[segmentID] = buffer
	}
	s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Return().Maybe()
	s.wb.checkpoint = &msgpb.MsgPosition{Timestamp: 200}
	s.wb.flushRetryInterval = time.Millisecond

	retryErr := merr.WrapErrServiceInternalMsg("transient storage failure")
	s.Require().Equal(syncmgr.SyncRetry, syncmgr.ClassifySyncError(context.Background(), retryErr))
	var firstAttempts atomic.Int32
	manager := &boundedReservableTestSyncManager{
		SyncManager:    s.syncMgr,
		slots:          make(chan struct{}, 1),
		reserveStarted: make(chan int, 4),
	}
	manager.onSubmit = func(_ context.Context, task syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
		var callbackErr error
		if task.SegmentID() == firstSegmentID && firstAttempts.Add(1) == 1 {
			callbackErr = retryErr
		} else {
			task.(*syncmgr.SyncTask).Abandon()
		}
		for _, callback := range callbacks {
			callbackErr = callback(callbackErr)
		}
		return completedSyncFuture(callbackErr), nil
	}
	s.wb.syncMgr = manager

	first := s.wb.syncSegments(context.Background(), []int64{firstSegmentID})
	s.Require().Len(first, 1)
	select {
	case call := <-manager.reserveStarted:
		s.Equal(1, call)
	default:
		s.FailNow("first segment did not reserve admission")
	}
	_, err := first[0].Await()
	s.ErrorIs(err, retryErr)
	firstEntry := s.wb.writeBufferSyncQueues[firstSegmentID].entries[0]
	s.True(firstEntry.failed)
	s.False(firstEntry.submitted)
	manager.mu.Lock()
	s.Require().Len(manager.admissions, 1)
	firstAdmission := manager.admissions[0]
	manager.mu.Unlock()
	s.Zero(firstAdmission.physicalReleases.Load(), "retry debt must retain its payload lease")

	secondDone := make(chan []*conc.Future[struct{}], 1)
	go func() {
		secondDone <- s.wb.syncSegments(context.Background(), []int64{secondSegmentID})
	}()
	select {
	case call := <-manager.reserveStarted:
		s.Equal(2, call)
	case <-time.After(time.Second):
		s.FailNow("second segment did not attempt admission")
	}
	select {
	case <-secondDone:
		s.FailNow("parked retry debt did not hold the node-wide payload budget")
	case <-time.After(30 * time.Millisecond):
	}
	s.wb.mut.RLock()
	_, secondBuffered := s.wb.buffers[secondSegmentID]
	_, secondQueued := s.wb.writeBufferSyncQueues[secondSegmentID]
	s.wb.mut.RUnlock()
	s.True(secondBuffered, "blocked admission must prevent second payload yield")
	s.False(secondQueued)

	// The admission wait no longer pumps retries itself; the bufferManager
	// retry ticker is what re-drives the parked first segment while the second
	// waits for its slot. Simulate its ticks.
	stopBackstop := make(chan struct{})
	defer close(stopBackstop)
	go func() {
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.wb.driveSyncRetries()
			case <-stopBackstop:
				return
			}
		}
	}()

	var second []*conc.Future[struct{}]
	select {
	case second = <-secondDone:
	case <-time.After(2 * time.Second):
		s.FailNow("admission wait did not drive the parked retry")
	}
	s.EqualValues(2, firstAttempts.Load())
	s.EqualValues(1, firstAdmission.physicalReleases.Load())
	s.Require().Len(second, 1)
	_, err = second[0].Await()
	s.NoError(err)
	s.NotContains(s.wb.buffers, secondSegmentID)
}

func (s *WriteBufferSuite) TestRetainedRetryReusesAdmissionUntilTerminalSuccess() {
	segmentID := int64(1125)
	segment := growingSegment(segmentID, storage.StorageV2)
	s.metacache.EXPECT().GetSegmentByID(segmentID).Return(segment, true).Maybe()
	s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Return().Maybe()
	buffer, err := newSegmentBuffer(segmentID, s.collSchema)
	s.Require().NoError(err)
	s.wb.buffers[segmentID] = buffer
	s.wb.checkpoint = &msgpb.MsgPosition{Timestamp: 200}

	retryErr := merr.WrapErrServiceInternalMsg("transient storage failure")
	var attempts atomic.Int32
	manager := &boundedReservableTestSyncManager{
		SyncManager:    s.syncMgr,
		slots:          make(chan struct{}, 1),
		reserveStarted: make(chan int, 2),
	}
	manager.onSubmit = func(_ context.Context, task syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
		var callbackErr error
		if attempts.Add(1) == 1 {
			callbackErr = retryErr
		} else {
			task.(*syncmgr.SyncTask).Abandon()
		}
		for _, callback := range callbacks {
			callbackErr = callback(callbackErr)
		}
		return completedSyncFuture(callbackErr), nil
	}
	s.wb.syncMgr = manager

	first := s.wb.syncSegments(context.Background(), []int64{segmentID})
	s.Require().Len(first, 1)
	_, err = first[0].Await()
	s.ErrorIs(err, retryErr)
	s.wb.mut.RLock()
	entry := s.wb.writeBufferSyncQueues[segmentID].entries[0]
	s.wb.mut.RUnlock()
	s.True(entry.failed)

	s.wb.resubmitWriteBufferSync(context.Background(), entry)
	s.Eventually(func() bool {
		s.wb.mut.RLock()
		defer s.wb.mut.RUnlock()
		_, exists := s.wb.writeBufferSyncQueues[segmentID]
		return !exists
	}, time.Second, time.Millisecond)
	s.EqualValues(1, manager.reserveCalls.Load(), "retained retry must not reserve a second payload slot")
	s.EqualValues(2, attempts.Load())
	manager.mu.Lock()
	s.Require().Len(manager.admissions, 1)
	admission := manager.admissions[0]
	manager.mu.Unlock()
	s.EqualValues(2, admission.submitCalls.Load())
	s.EqualValues(1, admission.physicalReleases.Load())
}

func (s *WriteBufferSuite) TestBeginDropSnapshotsEveryReorderEntry() {
	segmentID := int64(1119)
	states := make([]*writeBufferSyncEntry, 0, 2)
	s.wb.mut.Lock()
	for i := range 2 {
		state := newTestSyncEntry(segmentID, uint64(200+i), entrySubmitted)
		states = append(states, state)
		s.wb.registerWriteBufferSyncLocked(state)
	}
	s.wb.dropping = true
	waiters := s.wb.allWriteBufferSyncEntriesLocked()
	s.wb.mut.Unlock()

	s.ElementsMatch(states, waiters)
}

func (s *WriteBufferSuite) TestWriteBufferSyncPayloadCountsTowardFlushCapacity() {
	paramtable.Get().Save(paramtable.Get().DataNodeCfg.FlushInsertBufferSize.Key, "100")
	defer paramtable.Get().Reset(paramtable.Get().DataNodeCfg.FlushInsertBufferSize.Key)

	segmentID := int64(1102)
	segment := metacache.NewSegmentInfo(&datapb.SegmentInfo{
		ID:    segmentID,
		State: commonpb.SegmentState_Growing,
	}, nil, nil, metacache.NewEmptySegmentStats())
	s.metacache.EXPECT().GetSegmentByID(segmentID).Return(segment, true).Once()
	payloadReleased := make(chan struct{})
	releasedBytes := make(chan int64, 1)
	task := syncmgr.NewSyncTask().
		WithSyncPack(new(syncmgr.SyncPack).WithSegmentID(segmentID)).
		WithPayloadAccounting(60, 0, func(released int64) {
			releasedBytes <- released
			close(payloadReleased)
		})
	state := &writeBufferSyncEntry{
		task:            task,
		payloadReleased: payloadReleased,
		done:            make(chan struct{}),
		submitted:       true,
	}
	s.wb.mut.Lock()
	s.wb.registerWriteBufferSyncLocked(state)
	s.wb.buffers[segmentID] = &segmentBuffer{
		segmentID: segmentID,
		insertBuffer: &InsertBuffer{BufferBase: BufferBase{
			size: 40,
		}},
		deltaBuffer: &DeltaBuffer{},
	}
	s.wb.mut.Unlock()

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- s.wb.waitFlushCapacity()
	}()
	select {
	case err := <-waitDone:
		s.FailNow("flush capacity did not apply backpressure", err)
	case <-time.After(20 * time.Millisecond):
	}

	s.EqualValues(100, s.wb.MemorySize())
	s.EqualValues(40, s.wb.EvictableMemorySize())
	task.Abandon()
	s.EqualValues(60, <-releasedBytes)

	select {
	case err := <-waitDone:
		s.NoError(err)
	case <-time.After(time.Second):
		s.FailNow("payload release did not unblock flush capacity")
	}
	s.EqualValues(40, s.wb.MemorySize())
}

func (s *WriteBufferSuite) TestBM25StatsAreNotCountedTowardSyncPayload() {
	segmentID := int64(1108)
	segment := growingSegment(segmentID, storage.StorageV2)
	s.metacache.EXPECT().GetSegmentByID(segmentID).Return(segment, true).Once()
	s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Return().Once()

	stats := storage.NewBM25Stats()
	stats.Append(map[uint32]float32{1: 1, 2: 1, 3: 1})
	statsBuffer := newStatsBuffer()
	statsBuffer.Buffer(map[int64]*storage.BM25Stats{101: stats})
	s.wb.buffers[segmentID] = &segmentBuffer{
		segmentID: segmentID,
		insertBuffer: &InsertBuffer{
			collSchema:  s.collSchema,
			statsBuffer: statsBuffer,
		},
		deltaBuffer: NewDeltaBuffer(),
	}
	s.wb.checkpoint = &msgpb.MsgPosition{Timestamp: 200}

	// BM25 stats survive the payload release — SyncPack.ReleaseData keeps them
	// for Commit — so they are deliberately outside the payload budget. The
	// buffer reports nothing for them...
	s.Zero(s.wb.MemorySize())

	s.wb.mut.Lock()
	task, err := s.wb.getSyncTask(context.Background(), segmentID)
	s.wb.mut.Unlock()
	s.Require().NoError(err)
	writeBufferTask := task.(*syncmgr.SyncTask)

	// ...and neither does the task, which still carries them to the flush.
	s.Zero(writeBufferTask.InsertPayloadBytes())
	s.Zero(writeBufferTask.PayloadBytes())
	s.Zero(s.wb.MemorySize())
}

func (s *WriteBufferSuite) TestGrowingObserverPanicHappensAfterLifecycleCleanup() {
	segmentID := int64(1109)
	segment := metacache.NewSegmentInfo(&datapb.SegmentInfo{ID: segmentID}, nil, nil, nil)
	s.metacache.EXPECT().GetSegmentByID(segmentID).Return(segment, true).Maybe()
	s.wb.growingSourceProgress[segmentID] = &growingSourceProgress{
		segmentID: segmentID,
		syncing:   true,
	}
	s.wb.taskObserverCallback = func(syncmgr.Task, error) {
		panic("observer failure")
	}
	task := syncmgr.NewGrowingSourceSyncTask().
		WithSegmentID(segmentID).
		WithChannelName(s.channelName).
		WithCheckpoint(&msgpb.MsgPosition{Timestamp: 200})
	s.syncMgr.EXPECT().SyncData(mock.Anything, task, mock.Anything).RunAndReturn(
		func(_ context.Context, _ syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
			func() {
				defer func() { _ = recover() }()
				_ = callbacks[0](nil)
			}()
			return conc.Go(func() (struct{}, error) { return struct{}{}, nil }), nil
		}).Once()

	_ = submitSyncTasks(s.wb, context.Background(), []syncmgr.Task{task})
	s.False(s.wb.growingSourceProgress[segmentID].syncing)
}

// submitSyncTasks wraps submitSyncTaskSubmissions for tests that only need the
// plain task form; production always builds submissions directly.
func submitSyncTasks(wb *writeBufferBase, ctx context.Context, syncTasks []syncmgr.Task) []*conc.Future[struct{}] {
	submissions := make([]syncTaskSubmission, 0, len(syncTasks))
	for _, task := range syncTasks {
		submissions = append(submissions, syncTaskSubmission{task: task})
	}
	return wb.submitSyncTaskSubmissions(ctx, submissions)
}

func (s *WriteBufferSuite) TestDropRetriesGrowingSourceFailure() {
	segmentID := int64(1110)
	segment := growingSegment(segmentID, storage.StorageV3)
	s.metacache.EXPECT().GetSegmentByID(segmentID).Return(segment, true).Maybe()
	s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Return().Maybe()

	s.wb.checkpoint = &msgpb.MsgPosition{Timestamp: 200}
	s.wb.flushRetryInterval = time.Millisecond
	s.wb.growingSourceResolver = func(int64, *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
		return fakeGrowingFlushSource{}, syncmgr.GrowingSourceUsable
	}
	s.wb.growingSourceProgress[segmentID] = &growingSourceProgress{
		segmentID: segmentID,
	}

	transientErr := errors.New("transient growing-source sync failure")
	var attempts atomic.Int32
	s.syncMgr.EXPECT().SyncData(mock.Anything, mock.MatchedBy(func(task syncmgr.Task) bool {
		return task.SegmentID() == segmentID && task.IsDrop()
	}), mock.Anything).RunAndReturn(
		func(_ context.Context, _ syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
			attempt := attempts.Add(1)
			var callbackErr error
			if attempt == 1 {
				callbackErr = transientErr
			}
			for _, callback := range callbacks {
				callbackErr = callback(callbackErr)
			}
			return conc.Go(func() (struct{}, error) { return struct{}{}, callbackErr }), nil
		}).Twice()
	metaWriter := syncmgr.NewMockMetaWriter(s.T())
	metaWriter.EXPECT().DropChannel(mock.Anything, s.channelName).Return(nil).Once()
	s.wb.metaWriter = metaWriter

	s.NotPanics(func() { s.wb.Close(context.Background(), true) })
	s.EqualValues(2, attempts.Load())
	s.NotContains(s.wb.growingSourceProgress, segmentID)
}

func (s *WriteBufferSuite) TestCloseWaitsForWriteBufferSyncAndGrowingCleanup() {
	segmentID := int64(1107)
	segment := metacache.NewSegmentInfo(&datapb.SegmentInfo{ID: segmentID}, nil, nil, metacache.NewEmptySegmentStats())
	metacache.UpdateBufferedRows(10)(segment)
	metacache.StartSyncing(10)(segment)
	s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Run(
		func(action metacache.SegmentAction, _ ...metacache.SegmentFilter) {
			action(segment)
		}).Return().Once()

	state := newTestSyncEntry(segmentID, 200, entryBatchRows(10), entrySubmitted)
	task := state.task
	s.wb.registerWriteBufferSyncLocked(state)
	s.wb.growingSourceProgress[2207] = &growingSourceProgress{segmentID: 2207, syncing: true}

	closeDone := make(chan struct{})
	go func() {
		defer close(closeDone)
		s.wb.Close(context.Background(), false)
	}()

	select {
	case <-closeDone:
		s.FailNow("Close returned before the write-buffer callback cleaned its state")
	case <-time.After(20 * time.Millisecond):
	}

	s.ErrorIs(s.wb.finishWriteBufferSync(context.Background(), state, task, context.Canceled), context.Canceled)
	select {
	case <-closeDone:
		s.FailNow("Close returned before growing-source cleanup")
	case <-time.After(20 * time.Millisecond):
	}

	s.wb.mut.Lock()
	s.wb.failGrowingSyncLocked(s.wb.growingSourceProgress[2207], context.Canceled)
	s.wb.mut.Unlock()
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		s.FailNow("Close did not finish after all sync ownership was cleaned")
	}
	s.Zero(segment.BufferRows(), "discard must not claim the payload was restored to the buffer")
	s.Zero(segment.SyncingRows())
	s.True(metacache.WithNoSyncingTask().Filter(segment))
	s.Zero(s.wb.MemorySize())
}

func (s *WriteBufferSuite) TestWriteBufferSyncFailureKeepsCheckpointForReplay() {
	segmentID := int64(1120)
	start := &msgpb.MsgPosition{Timestamp: 200}
	state := newTestSyncEntry(segmentID, 300, entryStartPosition(start.GetTimestamp()), entrySubmitted)
	task := state.task
	s.wb.registerWriteBufferSyncLocked(state)
	s.wb.syncCheckpoint.Add(segmentID, start, "syncing task")
	s.wb.errHandler = func(error) {}
	s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Return().Once()

	// A terminal error is what ends a segment stream; a retryable one keeps the
	// task queued instead. See TestFinishWriteBufferSyncClassification.
	taskErr := merr.WrapErrDataIntegrityMsg("segment stream aborted")
	s.ErrorIs(s.wb.finishWriteBufferSync(context.Background(), state, task, taskErr), taskErr)
	candidate := s.wb.syncCheckpoint.GetEarliestWithDefault(nil)
	s.Require().NotNil(candidate)
	s.Equal(segmentID, candidate.segmentID)
	s.Equal(start.GetTimestamp(), candidate.position.GetTimestamp())
	s.NotContains(s.wb.writeBufferSyncQueues, segmentID)
}

func (s *WriteBufferSuite) TestWriteBufferSyncDoneWaitsForObserverCompletion() {
	segmentID := int64(1114)
	state := newTestSyncEntry(segmentID, 200, entrySubmitted)
	task := state.task
	s.wb.registerWriteBufferSyncLocked(state)
	s.metacache.EXPECT().GetSegmentByID(segmentID).Return(nil, false).Once()

	observerEntered := make(chan struct{})
	releaseObserver := make(chan struct{})
	s.wb.taskObserverCallback = func(syncmgr.Task, error) {
		close(observerEntered)
		<-releaseObserver
	}
	finishDone := make(chan error, 1)
	go func() {
		finishDone <- s.wb.finishWriteBufferSync(context.Background(), state, task, nil)
	}()

	select {
	case <-observerEntered:
	case <-time.After(time.Second):
		s.FailNow("write-buffer observer was not invoked")
	}
	select {
	case <-state.done:
		s.FailNow("write-buffer done was published before observer completion")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseObserver)
	select {
	case err := <-finishDone:
		s.NoError(err)
	case <-time.After(time.Second):
		s.FailNow("write-buffer completion did not finish after observer returned")
	}
	<-state.done
	s.NoError(state.terminalErr)
}

func (s *WriteBufferSuite) TestWriteBufferSyncObserverPanicDoesNotSkipFatalHandler() {
	state := newTestSyncEntry(1115, 200, entrySubmitted)
	task := state.task
	fatalErr := merr.WrapErrDataIntegrityMsg("fatal write-buffer sync")
	fatalCalled := false
	s.wb.registerWriteBufferSyncLocked(state)
	s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Return().Once()
	s.wb.taskObserverCallback = func(syncmgr.Task, error) {
		panic("observer panic")
	}
	s.wb.errHandler = func(err error) {
		fatalCalled = true
		s.ErrorIs(err, fatalErr)
	}

	s.PanicsWithValue("observer panic", func() {
		_ = s.wb.finishWriteBufferSync(context.Background(), state, task, fatalErr)
	})
	s.True(fatalCalled)
	<-state.done
	s.ErrorIs(state.terminalErr, fatalErr)
}

func (s *WriteBufferSuite) TestWriteBufferSyncWaitersObserveLaterTerminalErrorAndSettleAll() {
	segmentID := int64(1116)
	terminalErr := errors.New("terminal sync failure")
	retrying := newTestSyncEntry(segmentID, 200, entrySubmitted)
	task := retrying.task
	terminal := &writeBufferSyncEntry{done: make(chan struct{})}
	terminal.complete(terminalErr)
	s.wb.registerWriteBufferSyncLocked(retrying)
	s.wb.dropping = true
	s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Return().Once()

	dropCtx, dropCancel := context.WithCancel(context.Background())
	defer dropCancel()
	cancelObserved := make(chan struct{})
	settleRelease := make(chan struct{})
	go func() {
		<-dropCtx.Done()
		close(cancelObserved)
		<-settleRelease
		_ = s.wb.finishWriteBufferSync(dropCtx, retrying, task, context.Canceled)
	}()

	result := make(chan error, 1)
	go func() {
		result <- s.wb.waitSyncsSettled(dropCtx, dropCancel, []*writeBufferSyncEntry{retrying, terminal})
	}()

	select {
	case <-cancelObserved:
	case <-time.After(time.Second):
		s.FailNow("later terminal waiter did not cancel the Drop context")
	}
	select {
	case <-result:
		s.FailNow("wait returned before the retrying owner settled")
	default:
	}
	close(settleRelease)
	select {
	case err := <-result:
		s.ErrorIs(err, terminalErr)
	case <-time.After(time.Second):
		s.FailNow("wait did not return after every owner settled")
	}
	s.NotContains(s.wb.writeBufferSyncQueues, segmentID)
}

func (s *WriteBufferSuite) TestBeginDropSnapshotsWriteBufferSyncTerminalErrorAtomically() {
	segmentID := int64(1117)
	state := newTestSyncEntry(segmentID, 200, entrySubmitted)
	task := state.task
	s.wb.registerWriteBufferSyncLocked(state)
	s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Return().Once()

	dropCtx, dropCancel := context.WithCancel(context.Background())
	defer dropCancel()
	s.wb.mut.Lock()
	s.wb.dropping = true
	waiters := s.wb.allWriteBufferSyncEntriesLocked()
	s.Equal([]*writeBufferSyncEntry{state}, waiters)

	callbackStarted := make(chan struct{})
	callbackDone := make(chan struct{})
	go func() {
		defer close(callbackDone)
		close(callbackStarted)
		_ = s.wb.finishWriteBufferSync(dropCtx, state, task, context.Canceled)
	}()
	<-callbackStarted
	s.wb.mut.Unlock()

	s.ErrorIs(s.wb.waitSyncsSettled(dropCtx, dropCancel, waiters), context.Canceled)
	<-callbackDone
	s.NotContains(s.wb.writeBufferSyncQueues, segmentID)
}

func (s *WriteBufferSuite) TestGrowingAndWriteBufferSyncShareSegmentAdmissionGate() {
	segmentID := int64(1108)
	s.wb.growingSourceProgress[segmentID] = &growingSourceProgress{
		segmentID: segmentID,
		syncing:   true,
	}
	s.wb.buffers[segmentID] = &segmentBuffer{
		segmentID:    segmentID,
		insertBuffer: &InsertBuffer{},
		deltaBuffer:  &DeltaBuffer{},
	}
	s.metacache.EXPECT().GetSegmentByID(segmentID).Return(
		metacache.NewSegmentInfo(&datapb.SegmentInfo{ID: segmentID}, nil, nil, nil), true).Maybe()

	futures := s.wb.syncSegments(context.Background(), []int64{segmentID})
	s.Empty(futures)
	s.Contains(s.wb.buffers, segmentID)
	s.NotContains(s.wb.writeBufferSyncQueues, segmentID)
}

func (s *WriteBufferSuite) TestDropDrainsOutstandingBeforeBufferedTail() {
	segmentID := int64(1105)
	segment := growingSegment(segmentID, storage.StorageV2)
	s.metacache.EXPECT().GetSegmentByID(segmentID).Return(segment, true).Maybe()
	s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Run(
		func(action metacache.SegmentAction, _ ...metacache.SegmentFilter) {
			action(segment)
		}).Return().Maybe()

	metaWriter := syncmgr.NewMockMetaWriter(s.T())
	metaWriter.EXPECT().DropChannel(mock.Anything, s.channelName).Return(nil).Once()
	s.wb.metaWriter = metaWriter
	s.wb.checkpoint = &msgpb.MsgPosition{Timestamp: 300}

	firstState := newTestSyncEntry(segmentID, 200, entryBatchRows(10), entrySubmitted)
	firstTask := firstState.task
	s.wb.registerWriteBufferSyncLocked(firstState)
	tail, err := newSegmentBuffer(segmentID, s.collSchema)
	s.Require().NoError(err)
	s.wb.buffers[segmentID] = tail

	var submitted []syncmgr.Task
	s.syncMgr.EXPECT().SyncData(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, task syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
			submitted = append(submitted, task)
			var callbackErr error
			for _, callback := range callbacks {
				callbackErr = callback(callbackErr)
			}
			return conc.Go(func() (struct{}, error) { return struct{}{}, callbackErr }), nil
		}).Once()

	closeDone := make(chan struct{})
	go func() {
		defer close(closeDone)
		s.wb.Close(context.Background(), true)
	}()
	s.Eventually(func() bool {
		s.wb.mut.RLock()
		defer s.wb.mut.RUnlock()
		return s.wb.dropping
	}, time.Second, time.Millisecond)
	select {
	case <-closeDone:
		s.FailNow("drop overtook the outstanding task")
	default:
	}
	s.NoError(s.wb.finishWriteBufferSync(context.Background(), firstState, firstTask, nil))
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		s.FailNow("drop did not finish after the outstanding task")
	}
	s.Require().Len(submitted, 1)
	s.True(submitted[0].IsDrop(), "the buffered tail must become the final drop task")
}

func (s *WriteBufferSuite) TestFlushSourceModeNotifier() {
	segmentID := int64(1001)
	var notifiedSegmentID int64
	var notifiedMode metacache.FlushSourceMode
	s.wb.flushSourceModeNotifier = func(segmentID int64, mode metacache.FlushSourceMode) {
		notifiedSegmentID = segmentID
		notifiedMode = mode
	}

	s.Run("write_buffer_mode", func() {
		segment := metacache.NewSegmentInfo(&datapb.SegmentInfo{ID: segmentID}, nil, nil, nil)
		metacache.SetFlushSourceMode(metacache.FlushSourceWriteBuffer)(segment)
		s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Run(func(action metacache.SegmentAction, _ ...metacache.SegmentFilter) {
			action(segment)
		}).Return().Once()
		s.metacache.EXPECT().GetSegmentByID(segmentID).Return(segment, true).Once()

		s.wb.allowGrowingSourceFlush = true
		s.wb.getOrCreateBuffer(segmentID, 0)
		s.Equal(segmentID, notifiedSegmentID)
		s.Equal(metacache.FlushSourceWriteBuffer, notifiedMode)
	})

	s.Run("growing_mode", func() {
		segmentID := int64(1002)
		segment := metacache.NewSegmentInfo(&datapb.SegmentInfo{ID: segmentID, StorageVersion: storage.StorageV3}, nil, nil, nil)
		notifiedSegmentID = 0
		notifiedMode = metacache.FlushSourceUnknown
		s.metacache.EXPECT().GetSegmentByID(segmentID).Return(segment, true).Once()
		s.metacache.EXPECT().GetSegmentByID(segmentID).Return(segment, true).Once()
		s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Run(func(action metacache.SegmentAction, _ ...metacache.SegmentFilter) {
			action(segment)
		}).Return().Once()
		s.metacache.EXPECT().GetSegmentByID(segmentID).Return(segment, true).Once()

		err := s.wb.recordGrowingSourceProgress(&InsertData{
			segmentID:   segmentID,
			partitionID: 10,
			rowNum:      3,
		}, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 1)
		s.NoError(err)
		s.Equal(segmentID, notifiedSegmentID)
		s.Equal(metacache.FlushSourceGrowing, notifiedMode)
	})
}

func (s *WriteBufferSuite) TestCreateNewGrowingSegmentStorageVersion() {
	param := paramtable.Get()
	param.Save(param.CommonCfg.UseLoonFFI.Key, "false")
	defer param.Reset(param.CommonCfg.UseLoonFFI.Key)
	param.Save(param.CommonCfg.EnableGrowingSourceFlush.Key, "false")
	defer param.Reset(param.CommonCfg.EnableGrowingSourceFlush.Key)

	s.Run("non_text_uses_v2_when_ffi_disabled", func() {
		s.wb.allowGrowingSourceFlush = false
		s.False(s.wb.AllowGrowingSourceFlush())
		s.metacache.EXPECT().GetSegmentByID(int64(2001)).Return(nil, false).Once()
		s.metacache.EXPECT().AddSegment(mock.MatchedBy(func(info *datapb.SegmentInfo) bool {
			return info.GetStorageVersion() == storage.StorageV2 &&
				info.GetManifestPath() == "" &&
				info.GetSchemaVersion() == 11
		}), mock.Anything, mock.Anything, mock.Anything).Return().Once()

		err := s.wb.CreateNewGrowingSegment(CreateGrowingSegmentInfo{
			PartitionID:    10,
			SegmentID:      2001,
			SchemaVersion:  11,
			StorageVersion: storage.StorageV2,
		})
		s.NoError(err)
	})

	s.Run("growing_source_does_not_force_v3_manifest_when_ffi_disabled", func() {
		s.wb.allowGrowingSourceFlush = true
		s.metacache.EXPECT().GetSegmentByID(int64(2002)).Return(nil, false).Once()
		s.metacache.EXPECT().AddSegment(mock.MatchedBy(func(info *datapb.SegmentInfo) bool {
			return info.GetStorageVersion() == storage.StorageV2 &&
				info.GetManifestPath() == "" &&
				info.GetSchemaVersion() == 12
		}), mock.Anything, mock.Anything, mock.Anything).Return().Once()

		err := s.wb.CreateNewGrowingSegment(CreateGrowingSegmentInfo{
			PartitionID:    10,
			SegmentID:      2002,
			SchemaVersion:  12,
			StorageVersion: storage.StorageV2,
		})
		s.NoError(err)
	})

	s.Run("text_schema_uses_v3_manifest_without_enabling_growing_source_when_ffi_disabled", func() {
		textSchema := &schemapb.CollectionSchema{
			Name: "wb_text_collection",
			Fields: []*schemapb.FieldSchema{
				{FieldID: 100, DataType: schemapb.DataType_Int64, IsPrimaryKey: true, Name: "pk"},
				{FieldID: 101, DataType: schemapb.DataType_FloatVector, TypeParams: []*commonpb.KeyValuePair{
					{Key: common.DimKey, Value: "128"},
				}},
				{FieldID: 102, DataType: schemapb.DataType_Text, Name: "text"},
			},
		}
		mc := metacache.NewMockMetaCache(s.T())
		mc.EXPECT().GetSchema(mock.Anything).Return(textSchema).Maybe()
		mc.EXPECT().Collection().Return(s.collID).Maybe()

		wb, err := newWriteBufferBase(s.channelName, mc, s.syncMgr, &writeBufferOption{})
		s.Require().NoError(err)
		s.False(wb.AllowGrowingSourceFlush())

		mc.EXPECT().GetSegmentByID(int64(2003)).Return(nil, false).Once()
		mc.EXPECT().AddSegment(mock.MatchedBy(func(info *datapb.SegmentInfo) bool {
			return info.GetStorageVersion() == storage.StorageV3 &&
				info.GetManifestPath() != "" &&
				info.GetSchemaVersion() == 13
		}), mock.Anything, mock.Anything, mock.Anything).Return().Once()

		err = wb.CreateNewGrowingSegment(CreateGrowingSegmentInfo{
			PartitionID:   10,
			SegmentID:     2003,
			SchemaVersion: 13,
		})
		s.NoError(err)
	})

	s.Run("text_schema_uses_v3_manifest_with_writer_buffer_when_ffi_enabled", func() {
		param.Save(param.CommonCfg.UseLoonFFI.Key, "true")
		defer param.Save(param.CommonCfg.UseLoonFFI.Key, "false")

		textSchema := &schemapb.CollectionSchema{
			Name: "wb_text_collection",
			Fields: []*schemapb.FieldSchema{
				{FieldID: 100, DataType: schemapb.DataType_Int64, IsPrimaryKey: true, Name: "pk"},
				{FieldID: 101, DataType: schemapb.DataType_FloatVector, TypeParams: []*commonpb.KeyValuePair{
					{Key: common.DimKey, Value: "128"},
				}},
				{FieldID: 102, DataType: schemapb.DataType_Text, Name: "text"},
			},
		}
		mc := metacache.NewMockMetaCache(s.T())
		mc.EXPECT().GetSchema(mock.Anything).Return(textSchema).Maybe()
		mc.EXPECT().Collection().Return(s.collID).Maybe()

		wb, err := newWriteBufferBase(s.channelName, mc, s.syncMgr, &writeBufferOption{})
		s.Require().NoError(err)
		s.False(wb.AllowGrowingSourceFlush())

		mc.EXPECT().GetSegmentByID(int64(2004)).Return(nil, false).Once()
		mc.EXPECT().AddSegment(mock.MatchedBy(func(info *datapb.SegmentInfo) bool {
			return info.GetStorageVersion() == storage.StorageV3 &&
				info.GetManifestPath() != "" &&
				info.GetSchemaVersion() == 14
		}), mock.Anything, mock.Anything, mock.Anything).Return().Once()

		err = wb.CreateNewGrowingSegment(CreateGrowingSegmentInfo{
			PartitionID:    10,
			SegmentID:      2004,
			SchemaVersion:  14,
			StorageVersion: storage.StorageV3,
		})
		s.NoError(err)
	})

	s.Run("streaming_requires_create_segment_storage_version", func() {
		s.T().Setenv(streamingutil.MilvusStreamingServiceEnabled, "1")

		s.metacache.EXPECT().GetSegmentByID(int64(2005)).Return(nil, false).Once()

		err := s.wb.CreateNewGrowingSegment(CreateGrowingSegmentInfo{
			PartitionID:   10,
			SegmentID:     2005,
			SchemaVersion: 15,
		})
		s.ErrorIs(err, merr.ErrServiceInternal)
	})
}

func (s *WriteBufferSuite) TestFlushSegments() {
	segmentID := int64(1001)

	s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything, mock.Anything).Return()
	s.metacache.EXPECT().GetSegmentByID(mock.Anything, mock.Anything, mock.Anything).Return(nil, true)
	wb, err := NewWriteBuffer(s.channelName, s.metacache, s.syncMgr, WithIDAllocator(allocator.NewMockAllocator(s.T())))
	s.NoError(err)

	err = wb.SealSegments(context.Background(), []int64{segmentID})
	s.NoError(err)
}

func (s *WriteBufferSuite) TestSealSegmentsMissingSegment() {
	segmentID := int64(1001)

	s.Run("non_text_returns_error", func() {
		s.wb.allowGrowingSourceFlush = false
		s.metacache.EXPECT().GetSegmentByID(segmentID).Return(nil, false).Once()

		err := s.wb.SealSegments(context.Background(), []int64{segmentID})
		s.ErrorIs(err, merr.ErrSegmentNotFound)
	})

	s.Run("text_skips_missing_segment", func() {
		s.wb.allowGrowingSourceFlush = true
		defer func() {
			s.wb.allowGrowingSourceFlush = false
		}()
		s.metacache.EXPECT().GetSegmentByID(segmentID).Return(nil, false).Once()

		err := s.wb.SealSegments(context.Background(), []int64{segmentID})
		s.NoError(err)
	})
}

func (s *WriteBufferSuite) TestSealAllSegments() {
	s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything, mock.Anything).Return()
	wb, err := NewWriteBuffer(s.channelName, s.metacache, s.syncMgr, WithIDAllocator(allocator.NewMockAllocator(s.T())))
	s.NoError(err)
	wb.SealAllSegments(context.Background())
}

func (s *WriteBufferSuite) TestGetCheckpoint() {
	s.Run("use_consume_cp", func() {
		s.wb.checkpoint = &msgpb.MsgPosition{
			Timestamp: 1000,
		}

		checkpoint := s.wb.GetCheckpoint()
		s.EqualValues(1000, checkpoint.GetTimestamp())
	})

	s.Run("use_syncing_segment_cp", func() {
		s.wb.checkpoint = &msgpb.MsgPosition{
			Timestamp: 1000,
		}

		s.wb.syncCheckpoint.Add(1, &msgpb.MsgPosition{Timestamp: 500}, "syncing segments")
		defer s.wb.syncCheckpoint.Remove(1, 500)

		checkpoint := s.wb.GetCheckpoint()
		s.EqualValues(500, checkpoint.GetTimestamp())
	})

	s.Run("use_segment_buffer_min", func() {
		s.wb.checkpoint = &msgpb.MsgPosition{
			Timestamp: 1000,
		}

		s.wb.syncCheckpoint.Add(1, &msgpb.MsgPosition{Timestamp: 500}, "syncing segments")
		defer s.wb.syncCheckpoint.Remove(1, 500)

		buf1, err := newSegmentBuffer(2, s.collSchema)
		s.Require().NoError(err)
		buf1.insertBuffer.startPos = &msgpb.MsgPosition{
			Timestamp: 440,
		}
		buf1.deltaBuffer.startPos = &msgpb.MsgPosition{
			Timestamp: 400,
		}
		buf2, err := newSegmentBuffer(3, s.collSchema)
		s.Require().NoError(err)
		buf2.insertBuffer.startPos = &msgpb.MsgPosition{
			Timestamp: 550,
		}
		buf2.deltaBuffer.startPos = &msgpb.MsgPosition{
			Timestamp: 600,
		}

		s.wb.mut.Lock()
		s.wb.buffers[2] = buf1
		s.wb.buffers[3] = buf2
		s.wb.mut.Unlock()

		defer func() {
			s.wb.mut.Lock()
			defer s.wb.mut.Unlock()
			s.wb.buffers = make(map[int64]*segmentBuffer)
		}()

		checkpoint := s.wb.GetCheckpoint()
		s.EqualValues(400, checkpoint.GetTimestamp())
	})

	s.Run("sync_mgr_smaller", func() {
		s.wb.checkpoint = &msgpb.MsgPosition{
			Timestamp: 1000,
		}

		s.wb.syncCheckpoint.Add(1, &msgpb.MsgPosition{Timestamp: 300}, "syncing segments")
		defer s.wb.syncCheckpoint.Remove(1, 300)

		buf1, err := newSegmentBuffer(2, s.collSchema)
		s.Require().NoError(err)
		buf1.insertBuffer.startPos = &msgpb.MsgPosition{
			Timestamp: 440,
		}
		buf1.deltaBuffer.startPos = &msgpb.MsgPosition{
			Timestamp: 400,
		}
		buf2, err := newSegmentBuffer(3, s.collSchema)
		s.Require().NoError(err)
		buf2.insertBuffer.startPos = &msgpb.MsgPosition{
			Timestamp: 550,
		}
		buf2.deltaBuffer.startPos = &msgpb.MsgPosition{
			Timestamp: 600,
		}

		s.wb.mut.Lock()
		s.wb.buffers[2] = buf1
		s.wb.buffers[3] = buf2
		s.wb.mut.Unlock()

		defer func() {
			s.wb.mut.Lock()
			defer s.wb.mut.Unlock()
			s.wb.buffers = make(map[int64]*segmentBuffer)
		}()

		checkpoint := s.wb.GetCheckpoint()
		s.EqualValues(300, checkpoint.GetTimestamp())
	})

	s.Run("segment_buffer_smaller", func() {
		s.wb.checkpoint = &msgpb.MsgPosition{
			Timestamp: 1000,
		}

		s.wb.syncCheckpoint.Add(1, &msgpb.MsgPosition{Timestamp: 800}, "syncing segments")
		defer s.wb.syncCheckpoint.Remove(1, 800)

		buf1, err := newSegmentBuffer(2, s.collSchema)
		s.Require().NoError(err)
		buf1.insertBuffer.startPos = &msgpb.MsgPosition{
			Timestamp: 440,
		}
		buf1.deltaBuffer.startPos = &msgpb.MsgPosition{
			Timestamp: 400,
		}
		buf2, err := newSegmentBuffer(3, s.collSchema)
		s.Require().NoError(err)
		buf2.insertBuffer.startPos = &msgpb.MsgPosition{
			Timestamp: 550,
		}
		buf2.deltaBuffer.startPos = &msgpb.MsgPosition{
			Timestamp: 600,
		}

		s.wb.mut.Lock()
		s.wb.buffers[2] = buf1
		s.wb.buffers[3] = buf2
		s.wb.mut.Unlock()

		defer func() {
			s.wb.mut.Lock()
			defer s.wb.mut.Unlock()
			s.wb.buffers = make(map[int64]*segmentBuffer)
		}()

		checkpoint := s.wb.GetCheckpoint()
		s.EqualValues(400, checkpoint.GetTimestamp())
	})
}

func (s *WriteBufferSuite) TestEvictBuffer() {
	wb, err := newWriteBufferBase(s.channelName, s.metacache, s.syncMgr, &writeBufferOption{})
	s.Require().NoError(err)

	s.Run("no_checkpoint", func() {
		wb.mut.Lock()
		wb.buffers[100] = &segmentBuffer{}
		wb.mut.Unlock()
		defer func() {
			wb.mut.Lock()
			defer wb.mut.Unlock()
			wb.buffers = make(map[int64]*segmentBuffer)
		}()

		wb.EvictBuffer(GetOldestBufferPolicy(1))
	})

	s.Run("trigger_sync", func() {
		buf1, err := newSegmentBuffer(2, s.collSchema)
		s.Require().NoError(err)
		buf1.insertBuffer.startPos = &msgpb.MsgPosition{
			Timestamp: 440,
		}
		buf1.deltaBuffer.startPos = &msgpb.MsgPosition{
			Timestamp: 400,
		}
		buf2, err := newSegmentBuffer(3, s.collSchema)
		s.Require().NoError(err)
		buf2.insertBuffer.startPos = &msgpb.MsgPosition{
			Timestamp: 550,
		}
		buf2.deltaBuffer.startPos = &msgpb.MsgPosition{
			Timestamp: 600,
		}

		wb.mut.Lock()
		wb.buffers[2] = buf1
		wb.buffers[3] = buf2
		wb.checkpoint = &msgpb.MsgPosition{Timestamp: 100}
		wb.mut.Unlock()

		segment := metacache.NewSegmentInfo(&datapb.SegmentInfo{
			ID: 2,
		}, nil, nil, metacache.NewEmptySegmentStats())
		s.metacache.EXPECT().GetSegmentByID(int64(2)).Return(segment, true)
		s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Return()
		s.syncMgr.EXPECT().SyncData(mock.Anything, mock.MatchedBy(func(task syncmgr.Task) bool {
			return task != nil && task.SegmentID() == 2
		}), mock.Anything).Return(conc.Go[struct{}](func() (struct{}, error) {
			return struct{}{}, nil
		}), nil).Once()
		defer func() {
			s.wb.mut.Lock()
			defer s.wb.mut.Unlock()
			s.wb.buffers = make(map[int64]*segmentBuffer)
		}()
		wb.EvictBuffer(GetOldestBufferPolicy(1))
	})

	s.Run("sync_submit_outside_lock", func() {
		buf, err := newSegmentBuffer(4, s.collSchema)
		s.Require().NoError(err)
		buf.insertBuffer.startPos = &msgpb.MsgPosition{Timestamp: 440}
		buf.deltaBuffer.startPos = &msgpb.MsgPosition{Timestamp: 400}

		wb.mut.Lock()
		wb.buffers[4] = buf
		wb.checkpoint = &msgpb.MsgPosition{Timestamp: 1000}
		wb.mut.Unlock()

		segment := metacache.NewSegmentInfo(&datapb.SegmentInfo{
			ID: 4,
		}, nil, nil, nil)
		s.metacache.EXPECT().GetSegmentByID(int64(4)).Return(segment, true)
		s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Return()

		submitEntered := make(chan struct{})
		releaseSubmit := make(chan struct{})
		s.syncMgr.EXPECT().SyncData(mock.Anything, mock.MatchedBy(func(task syncmgr.Task) bool {
			return task != nil && task.SegmentID() == 4
		}), mock.Anything).RunAndReturn(
			func(context.Context, syncmgr.Task, ...func(error) error) (*conc.Future[struct{}], error) {
				close(submitEntered)
				<-releaseSubmit
				return conc.Go[struct{}](func() (struct{}, error) {
					return struct{}{}, nil
				}), nil
			},
		).Once()

		evictDone := make(chan struct{})
		go func() {
			defer close(evictDone)
			wb.EvictBuffer(GetOldestBufferPolicy(1))
		}()

		select {
		case <-submitEntered:
		case <-time.After(3 * time.Second):
			s.FailNow("SyncData should be called before checking the write buffer lock")
		}

		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			_ = wb.MemorySize()
		}()

		select {
		case <-readDone:
		case <-time.After(3 * time.Second):
			s.FailNow("write buffer read lock should not be blocked by SyncData submit")
		}

		close(releaseSubmit)
		select {
		case <-evictDone:
		case <-time.After(3 * time.Second):
			s.FailNow("EvictBuffer should finish after SyncData submit is released")
		}
	})

	s.Run("drop_close_sync_submit_outside_lock", func() {
		syncMgr := syncmgr.NewMockSyncManager(s.T())
		metaCache := metacache.NewMockMetaCache(s.T())
		metaWriter := syncmgr.NewMockMetaWriter(s.T())
		metaCache.EXPECT().GetSchema(mock.Anything).Return(s.collSchema).Maybe()
		metaCache.EXPECT().Collection().Return(s.collID).Maybe()

		closeWB, err := newWriteBufferBase(s.channelName, metaCache, syncMgr, &writeBufferOption{
			metaWriter: metaWriter,
		})
		s.Require().NoError(err)

		buf, err := newSegmentBuffer(5, s.collSchema)
		s.Require().NoError(err)
		buf.insertBuffer.startPos = &msgpb.MsgPosition{Timestamp: 540}
		buf.deltaBuffer.startPos = &msgpb.MsgPosition{Timestamp: 500}

		closeWB.mut.Lock()
		closeWB.buffers[5] = buf
		closeWB.checkpoint = &msgpb.MsgPosition{Timestamp: 1000}
		closeWB.mut.Unlock()

		segment := metacache.NewSegmentInfo(&datapb.SegmentInfo{
			ID: 5,
		}, nil, nil, nil)
		metaCache.EXPECT().GetSegmentByID(int64(5)).Return(segment, true)
		metaCache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Return()

		submitEntered := make(chan struct{})
		releaseSubmit := make(chan struct{})
		syncMgr.EXPECT().SyncData(mock.Anything, mock.MatchedBy(func(task syncmgr.Task) bool {
			return task != nil && task.SegmentID() == 5 && task.IsDrop()
		}), mock.Anything).RunAndReturn(
			func(_ context.Context, _ syncmgr.Task, callbacks ...func(error) error) (*conc.Future[struct{}], error) {
				close(submitEntered)
				<-releaseSubmit
				var callbackErr error
				for _, callback := range callbacks {
					callbackErr = callback(callbackErr)
				}
				return conc.Go[struct{}](func() (struct{}, error) {
					return struct{}{}, callbackErr
				}), nil
			},
		).Once()
		metaWriter.EXPECT().DropChannel(mock.Anything, s.channelName).Return(nil).Once()

		closeDone := make(chan struct{})
		go func() {
			defer close(closeDone)
			closeWB.Close(context.Background(), true)
		}()

		select {
		case <-submitEntered:
		case <-time.After(3 * time.Second):
			s.FailNow("SyncData should be called before checking the write buffer lock")
		}

		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			_ = closeWB.MemorySize()
		}()

		select {
		case <-readDone:
		case <-time.After(3 * time.Second):
			s.FailNow("write buffer read lock should not be blocked by Close SyncData submit")
		}

		close(releaseSubmit)
		select {
		case <-closeDone:
		case <-time.After(3 * time.Second):
			s.FailNow("Close should finish after SyncData submit is released")
		}
	})

	s.Run("await_outside_lock", func() {
		mockAllocator := allocator.NewMockAllocator(s.T())
		var nextSegmentID atomic.Int64
		nextSegmentID.Store(1000)
		secondBufferProgress := make(chan struct{})
		var allocCallCount atomic.Int64
		var progressNotified atomic.Bool
		mockAllocator.EXPECT().AllocOne().RunAndReturn(func() (int64, error) {
			if allocCallCount.Add(1) == 2 && progressNotified.CompareAndSwap(false, true) {
				close(secondBufferProgress)
			}
			return nextSegmentID.Add(1), nil
		}).Times(2)

		l0wb, err := NewL0WriteBuffer(s.channelName, s.metacache, s.syncMgr, &writeBufferOption{
			idAllocator:  mockAllocator,
			syncPolicies: []SyncPolicy{},
		})
		s.Require().NoError(err)

		s.metacache.EXPECT().AddSegment(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return().Maybe()
		l0Segment := metacache.NewSegmentInfo(&datapb.SegmentInfo{
			ID:          1001,
			PartitionID: 10,
			Level:       datapb.SegmentLevel_L0,
		}, nil, nil, metacache.NewEmptySegmentStats())
		s.metacache.EXPECT().GetSegmentByID(mock.Anything).Return(l0Segment, true).Maybe()
		s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Return().Maybe()

		syncStarted := make(chan struct{})
		releaseSync := make(chan struct{})
		var releaseSyncClosed atomic.Bool
		closeReleaseSync := func() {
			if releaseSyncClosed.CompareAndSwap(false, true) {
				close(releaseSync)
			}
		}
		defer closeReleaseSync()
		var syncStartedOnce atomic.Bool
		s.syncMgr.EXPECT().SyncData(mock.Anything, mock.MatchedBy(func(task syncmgr.Task) bool {
			return task != nil && task.SegmentID() == 1001
		}), mock.Anything).RunAndReturn(
			func(context.Context, syncmgr.Task, ...func(error) error) (*conc.Future[struct{}], error) {
				if syncStartedOnce.CompareAndSwap(false, true) {
					close(syncStarted)
				}
				return conc.Go[struct{}](func() (struct{}, error) {
					<-releaseSync
					return struct{}{}, nil
				}), nil
			},
		).Once()

		firstDeleteMsgs := []*msgstream.DeleteMsg{
			{
				DeleteRequest: &msgpb.DeleteRequest{
					PartitionID: 10,
					PrimaryKeys: &schemapb.IDs{IdField: &schemapb.IDs_IntId{IntId: &schemapb.LongArray{Data: []int64{10}}}},
					Timestamps:  []uint64{100},
				},
			},
		}
		err = l0wb.BufferData(nil, firstDeleteMsgs, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200}, 0)
		s.Require().NoError(err)

		evictDone := make(chan struct{})
		go func() {
			defer close(evictDone)
			l0wb.EvictBuffer(GetOldestBufferPolicy(1))
		}()

		select {
		case <-syncStarted:
		case <-time.After(3 * time.Second):
			s.FailNow("SyncData should start before buffering second batch")
		}

		secondDeleteMsgs := []*msgstream.DeleteMsg{
			{
				DeleteRequest: &msgpb.DeleteRequest{
					PartitionID: 12,
					PrimaryKeys: &schemapb.IDs{IdField: &schemapb.IDs_IntId{IntId: &schemapb.LongArray{Data: []int64{12}}}},
					Timestamps:  []uint64{300},
				},
			},
		}

		bufferDataDone := make(chan error, 1)
		go func() {
			bufferDataDone <- l0wb.BufferData(nil, secondDeleteMsgs, &msgpb.MsgPosition{Timestamp: 300}, &msgpb.MsgPosition{Timestamp: 350}, 0)
		}()

		select {
		case <-secondBufferProgress:
			// BufferData made progress under lock before sync release.
		case <-time.After(3 * time.Second):
			s.FailNow("BufferData should make progress before sync future is released")
		}

		closeReleaseSync()
		select {
		case err := <-bufferDataDone:
			s.NoError(err)
		case <-time.After(3 * time.Second):
			s.FailNow("BufferData should finish after sync is released")
		}
		select {
		case <-evictDone:
		case <-time.After(3 * time.Second):
			s.FailNow("EvictBuffer should finish after sync is released")
		}
	})
}

func (s *WriteBufferSuite) TestGrowingSourceProgressSelectedByPolicy() {
	paramtable.Get().Save(paramtable.Get().DataNodeCfg.SyncPeriod.Key, "1")
	defer paramtable.Get().Reset(paramtable.Get().DataNodeCfg.SyncPeriod.Key)
	paramtable.Get().Save(paramtable.Get().DataNodeCfg.FlushInsertBufferSize.Key, "100")
	defer paramtable.Get().Reset(paramtable.Get().DataNodeCfg.FlushInsertBufferSize.Key)
	originalEstSize := s.wb.estSizePerRecord
	s.wb.estSizePerRecord = 10
	defer func() {
		s.wb.estSizePerRecord = originalEstSize
	}()

	now := time.Now()
	recentTs := tsoutil.ComposeTSByTime(now.Add(500 * time.Millisecond))
	staleTs := tsoutil.ComposeTSByTime(now.Add(2 * time.Second))
	startTs := tsoutil.ComposeTSByTime(now)

	s.Run("pending_flush", func() {
		selected := s.wb.growingSourceProgressSelectedByPolicy(recentTs, 1001, &growingSourceProgress{
			segmentID: 1001,
			owesFlush: true,
		})
		s.True(selected)
	})

	s.Run("non_retryable_failure", func() {
		selected := s.wb.growingSourceProgressSelectedByPolicy(recentTs, 1007, &growingSourceProgress{
			segmentID:           1007,
			owesFlush:           true,
			nonRetryableFailure: true,
		})
		s.False(selected)
	})

	s.Run("sealed_segment", func() {
		segment := metacache.NewSegmentInfo(&datapb.SegmentInfo{
			ID:    1002,
			State: commonpb.SegmentState_Sealed,
		}, nil, nil, nil)
		s.metacache.EXPECT().GetSegmentByID(int64(1002)).Return(segment, true).Once()

		selected := s.wb.growingSourceProgressSelectedByPolicy(recentTs, 1002, &growingSourceProgress{
			segmentID: 1002,
		})
		s.True(selected)
	})

	s.Run("recent_progress", func() {
		s.metacache.EXPECT().GetSegmentByID(int64(1003)).Return(nil, false).Once()

		selected := s.wb.growingSourceProgressSelectedByPolicy(recentTs, 1003, &growingSourceProgress{
			segmentID: 1003,
			batches: []growingSourceProgressBatch{
				{startPosition: &msgpb.MsgPosition{Timestamp: startTs}},
			},
		})
		s.False(selected)
	})

	s.Run("below_row_threshold", func() {
		segment := metacache.NewSegmentInfo(&datapb.SegmentInfo{
			ID: 1004,
		}, nil, nil, nil)
		s.metacache.EXPECT().GetSegmentByID(int64(1004)).Return(segment, true).Once()

		selected := s.wb.growingSourceProgressSelectedByPolicy(recentTs, 1004, &growingSourceProgress{
			segmentID:     1004,
			lastFlushedTs: 9,
			batches: []growingSourceProgressBatch{
				{startPosition: &msgpb.MsgPosition{Timestamp: startTs}},
			},
		})
		s.False(selected)
	})

	s.Run("row_threshold", func() {
		segment := metacache.NewSegmentInfo(&datapb.SegmentInfo{
			ID: 1005,
		}, nil, nil, nil)
		s.metacache.EXPECT().GetSegmentByID(int64(1005)).Return(segment, true).Once()

		// The row count now comes from the recorded packs, not from a cumulative
		// offset: pendingRows() sums what has been recorded and not yet flushed.
		selected := s.wb.growingSourceProgressSelectedByPolicy(recentTs, 1005, &growingSourceProgress{
			segmentID: 1005,
			batches: []growingSourceProgressBatch{
				{endPosition: &msgpb.MsgPosition{Timestamp: startTs}, rowNum: 10},
			},
		})
		s.True(selected)
	})

	s.Run("stale_progress", func() {
		s.metacache.EXPECT().GetSegmentByID(int64(1006)).Return(nil, false).Once()

		selected := s.wb.growingSourceProgressSelectedByPolicy(staleTs, 1006, &growingSourceProgress{
			segmentID: 1006,
			batches: []growingSourceProgressBatch{
				{startPosition: &msgpb.MsgPosition{Timestamp: startTs}},
			},
		})
		s.True(selected)
	})
}

func (s *WriteBufferSuite) TestGrowingSourceLayoutMismatch() {
	s.Equal(syncmgr.SyncTerminal, syncmgr.ClassifySyncError(context.Background(), errors.New("flush growing source data: Invalid: Column count mismatch at index 0: existing has 21 columns, but appended has 44 columns: segcore error[segcoreCode=2001]")))
	s.Equal(syncmgr.SyncTerminal, syncmgr.ClassifySyncError(context.Background(), errors.New("flush growing source data: Invalid: Column group size mismatch: existing has 10 groups, but appended has 1 groups: segcore error[segcoreCode=2001]")))
	s.Equal(syncmgr.SyncRetry, syncmgr.ClassifySyncError(context.Background(), errors.New("flush growing source data: mock transient error")))
	s.Equal(syncmgr.SyncRetry, syncmgr.ClassifySyncError(context.Background(), nil))
}

func (s *WriteBufferSuite) TestGrowingSourceProgressSyncableSkipsNonRetryableFailure() {
	syncable := s.wb.growingSourceProgressSyncable(1001, &growingSourceProgress{
		segmentID:           1001,
		nonRetryableFailure: true,
		batches: []growingSourceProgressBatch{
			{endPosition: &msgpb.MsgPosition{Timestamp: 100}},
		},
	})
	s.False(syncable)
}

func (s *WriteBufferSuite) TestGrowingSourceProgressRetrySkipsNonRetryableFailure() {
	s.wb.growingSourceProgress[1001] = &growingSourceProgress{
		segmentID:           1001,
		nonRetryableFailure: true,
		batches: []growingSourceProgressBatch{
			{endPosition: &msgpb.MsgPosition{Timestamp: 100}},
		},
	}

	segments := s.wb.getGrowingSourceSegmentsToRetry(time.Now(), 0)
	s.Empty(segments)
}

func (s *WriteBufferSuite) TestGrowingSourceFailureMetricAggregatesByChannel() {
	metrics.DataNodeGrowingSourceSyncFailureCount.Reset()
	s.T().Cleanup(metrics.DataNodeGrowingSourceSyncFailureCount.Reset)
	s.wb.growingSourceProgress = map[int64]*growingSourceProgress{
		1001: {segmentID: 1001, failureCount: 5},
		1002: {segmentID: 1002, failureCount: 2},
	}

	s.wb.updateGrowingSourceSyncFailureMetricLocked()
	gauge := metrics.DataNodeGrowingSourceSyncFailureCount.WithLabelValues(
		paramtable.GetStringNodeID(), fmt.Sprint(s.collID), s.channelName)
	s.Equal(float64(5), promtestutil.ToFloat64(gauge))

	s.wb.resetGrowingSourceSyncFailureMetric(1001)
	s.Equal(float64(2), promtestutil.ToFloat64(gauge))

	s.wb.resetGrowingSourceSyncFailureMetric(1002)
	s.Equal(0, promtestutil.CollectAndCount(metrics.DataNodeGrowingSourceSyncFailureCount))
}

func (s *WriteBufferSuite) TestGrowingSourceCancellationIsNotFailure() {
	metrics.DataNodeGrowingSourceSyncFailureCount.Reset()
	s.T().Cleanup(metrics.DataNodeGrowingSourceSyncFailureCount.Reset)

	const segmentID = int64(1003)
	progress := &growingSourceProgress{
		segmentID:   segmentID,
		syncing:     true,
		claimedRows: 0,
	}
	s.wb.growingSourceProgress[segmentID] = progress
	s.metacache.EXPECT().GetSegmentByID(segmentID).Return(nil, false).Once()
	task := syncmgr.NewGrowingSourceSyncTask().WithSegmentID(segmentID).WithBatchRows(0)

	err := s.wb.finishGrowingSourceSync(context.Background(), task, context.Canceled)
	s.ErrorIs(err, context.Canceled)
	s.False(progress.syncing)
	s.Zero(progress.failureCount)
	s.Empty(progress.lastFailure)
	s.Equal(0, promtestutil.CollectAndCount(metrics.DataNodeGrowingSourceSyncFailureCount))
}

func (s *WriteBufferSuite) TestGrowingSourceFailureMetricSettledOnClose() {
	metrics.DataNodeGrowingSourceSyncFailureCount.Reset()
	s.T().Cleanup(metrics.DataNodeGrowingSourceSyncFailureCount.Reset)
	s.wb.growingSourceProgress = map[int64]*growingSourceProgress{
		1001: {segmentID: 1001, failureCount: 5},
	}

	s.wb.updateGrowingSourceSyncFailureMetricLocked()
	s.Equal(1, promtestutil.CollectAndCount(metrics.DataNodeGrowingSourceSyncFailureCount))
	s.wb.settleGrowingSourceFailureMetricLocked()
	s.wb.growingSourceProgress[1001].failureCount = 6
	s.wb.updateGrowingSourceSyncFailureMetricLocked()
	s.Equal(0, promtestutil.CollectAndCount(metrics.DataNodeGrowingSourceSyncFailureCount),
		"a late callback must not recreate a closed channel's series")
}

func (s *WriteBufferSuite) TestFlowGraphBufferMetricSharedCollectionLifecycle() {
	metrics.DataNodeFlowGraphBufferDataSize.Reset()
	s.T().Cleanup(metrics.DataNodeFlowGraphBufferDataSize.Reset)

	wb1 := &writeBufferBase{collectionID: s.collID}
	wb2 := &writeBufferBase{collectionID: s.collID}
	wb1.addBufferMetric(10)
	wb2.addBufferMetric(20)

	gauge := metrics.DataNodeFlowGraphBufferDataSize.WithLabelValues(
		paramtable.GetStringNodeID(), fmt.Sprint(s.collID))
	s.Equal(float64(30), promtestutil.ToFloat64(gauge))

	wb1.settleBufferMetric()
	s.Equal(float64(20), promtestutil.ToFloat64(gauge))
	s.Equal(1, promtestutil.CollectAndCount(metrics.DataNodeFlowGraphBufferDataSize))

	wb2.settleBufferMetric()
	s.Equal(0, promtestutil.CollectAndCount(metrics.DataNodeFlowGraphBufferDataSize))

	// A task completing after its channel was settled must not recreate the
	// deleted collector with a negative value.
	wb2.addBufferMetric(-5)
	s.Equal(0, promtestutil.CollectAndCount(metrics.DataNodeFlowGraphBufferDataSize))
}

func (s *WriteBufferSuite) TestDropPartitions() {
	wb, err := newWriteBufferBase(s.channelName, s.metacache, s.syncMgr, &writeBufferOption{})
	s.Require().NoError(err)

	segIDs := []int64{1, 2, 3}
	s.metacache.EXPECT().GetSegmentIDsBy(mock.Anything).Return(segIDs).Once()
	s.metacache.EXPECT().UpdateSegments(mock.AnythingOfType("metacache.SegmentAction"), metacache.WithSegmentIDs(segIDs...)).Return().Once()

	wb.dropPartitions([]int64{100, 101})
}

func TestWriteBufferBase(t *testing.T) {
	suite.Run(t, new(WriteBufferSuite))
}

// TestPrepareInsertWithMissingFields tests the missing field filling functionality
func TestPrepareInsertWithMissingFields(t *testing.T) {
	// Create a collection schema with multiple fields including nullable and default value fields
	collSchema := &schemapb.CollectionSchema{
		Name: "test_collection",
		Fields: []*schemapb.FieldSchema{
			{FieldID: common.RowIDField, DataType: schemapb.DataType_Int64, Name: "RowID"},
			{FieldID: common.TimeStampField, DataType: schemapb.DataType_Int64, Name: "Timestamp"},
			{
				FieldID:      100,
				Name:         "pk",
				DataType:     schemapb.DataType_Int64,
				IsPrimaryKey: true,
				AutoID:       true,
			},
			{
				FieldID:  101,
				Name:     "int32_field",
				DataType: schemapb.DataType_Int32,
				Nullable: false,
				DefaultValue: &schemapb.ValueField{
					Data: &schemapb.ValueField_IntData{
						IntData: 42,
					},
				},
			},
			{
				FieldID:  102,
				Name:     "nullable_float_field",
				DataType: schemapb.DataType_Float,
				Nullable: true,
			},
			{
				FieldID:  103,
				Name:     "default_string_field",
				DataType: schemapb.DataType_String,
				Nullable: false,
				DefaultValue: &schemapb.ValueField{
					Data: &schemapb.ValueField_StringData{
						StringData: "default_string",
					},
				},
			},
		},
	}

	pkField := &schemapb.FieldSchema{
		FieldID:      100,
		Name:         "pk",
		DataType:     schemapb.DataType_Int64,
		IsPrimaryKey: true,
		AutoID:       true,
	}

	t.Run("test_missing_fields_with_default_values", func(t *testing.T) {
		// Create insert message with only some fields
		insertMsg := &msgstream.InsertMsg{
			BaseMsg: msgstream.BaseMsg{
				BeginTimestamp: 1000,
				EndTimestamp:   1002,
			},
			InsertRequest: &msgpb.InsertRequest{
				SegmentID:    1,
				PartitionID:  1,
				CollectionID: 1,
				FieldsData: []*schemapb.FieldData{
					{
						FieldId:   common.RowIDField,
						FieldName: "RowID",
						Type:      schemapb.DataType_Int64,
						Field: &schemapb.FieldData_Scalars{
							Scalars: &schemapb.ScalarField{
								Data: &schemapb.ScalarField_LongData{
									LongData: &schemapb.LongArray{
										Data: []int64{1, 2, 3},
									},
								},
							},
						},
					},
					{
						FieldId:   common.TimeStampField,
						FieldName: "Timestamp",
						Type:      schemapb.DataType_Int64,
						Field: &schemapb.FieldData_Scalars{
							Scalars: &schemapb.ScalarField{
								Data: &schemapb.ScalarField_LongData{
									LongData: &schemapb.LongArray{
										Data: []int64{1000, 1001, 1002},
									},
								},
							},
						},
					},
					{
						FieldId:   100, // Primary key field
						FieldName: "pk",
						Type:      schemapb.DataType_Int64,
						Field: &schemapb.FieldData_Scalars{
							Scalars: &schemapb.ScalarField{
								Data: &schemapb.ScalarField_LongData{
									LongData: &schemapb.LongArray{
										Data: []int64{1, 2, 3},
									},
								},
							},
						},
					},
					{
						FieldId:   101, // field1 with default value
						FieldName: "int32_field",
						Type:      schemapb.DataType_Int32,
						Field: &schemapb.FieldData_Scalars{
							Scalars: &schemapb.ScalarField{
								Data: &schemapb.ScalarField_IntData{
									IntData: &schemapb.IntArray{
										Data: []int32{1, 2, 3},
									},
								},
							},
						},
					},
				},
				// Missing nullable_float_field (nullable)
				// Missing default_string_field (with default value)
				NumRows:    3,
				Version:    msgpb.InsertDataVersion_ColumnBased,
				Timestamps: []uint64{1, 1, 1},
			},
		}

		insertMsgs := []*msgstream.InsertMsg{insertMsg}
		result, err := PrepareInsert(collSchema, pkField, insertMsgs)

		assert.NoError(t, err)
		assert.Len(t, result, 1)
		assert.Equal(t, int64(3), result[0].rowNum)

		// Check that all fields are present in the result
		insertData := result[0].data[0]
		for i := 0; i < 3; i++ {
			assert.Nil(t, insertData.Data[102].GetRow(i))
			assert.Equal(t, "default_string", insertData.Data[103].GetRow(i))
		}
	})
}

func TestPrepareInsertMaterializesLegacyBM25Output(t *testing.T) {
	collSchema := &schemapb.CollectionSchema{
		Name: "bm25_collection",
		Fields: []*schemapb.FieldSchema{
			{FieldID: common.RowIDField, Name: common.RowIDFieldName, DataType: schemapb.DataType_Int64},
			{FieldID: common.TimeStampField, Name: common.TimeStampFieldName, DataType: schemapb.DataType_Int64},
			{
				FieldID:      100,
				Name:         "pk",
				DataType:     schemapb.DataType_Int64,
				IsPrimaryKey: true,
			},
			{
				FieldID:  101,
				Name:     "text",
				DataType: schemapb.DataType_VarChar,
				TypeParams: []*commonpb.KeyValuePair{
					{Key: "max_length", Value: "1024"},
				},
			},
			{
				FieldID:          102,
				Name:             "sparse",
				DataType:         schemapb.DataType_SparseFloatVector,
				IsFunctionOutput: true,
			},
		},
		Functions: []*schemapb.FunctionSchema{
			{
				Name:           "bm25",
				Type:           schemapb.FunctionType_BM25,
				InputFieldIds:  []int64{101},
				OutputFieldIds: []int64{102},
			},
			{
				Name:          "rerank",
				Type:          schemapb.FunctionType_Rerank,
				InputFieldIds: []int64{101},
			},
		},
	}
	pkField := collSchema.GetFields()[2]
	insertMsg := &msgstream.InsertMsg{
		BaseMsg: msgstream.BaseMsg{
			BeginTimestamp: 1000,
			EndTimestamp:   1002,
		},
		InsertRequest: &msgpb.InsertRequest{
			SegmentID:    1,
			PartitionID:  1,
			CollectionID: 1,
			NumRows:      3,
			Version:      msgpb.InsertDataVersion_ColumnBased,
			RowIDs:       []int64{10, 11, 12},
			Timestamps:   []uint64{1000, 1001, 1002},
			FieldsData: []*schemapb.FieldData{
				{
					FieldId: 100,
					Type:    schemapb.DataType_Int64,
					Field: &schemapb.FieldData_Scalars{
						Scalars: &schemapb.ScalarField{
							Data: &schemapb.ScalarField_LongData{
								LongData: &schemapb.LongArray{Data: []int64{1, 2, 3}},
							},
						},
					},
				},
				{
					FieldId: 101,
					Type:    schemapb.DataType_VarChar,
					Field: &schemapb.FieldData_Scalars{
						Scalars: &schemapb.ScalarField{
							Data: &schemapb.ScalarField_StringData{
								StringData: &schemapb.StringArray{Data: []string{"hello world", "milvus bm25", "legacy message"}},
							},
						},
					},
				},
			},
		},
	}

	assert.NoError(t, function.GetManager().Alloc(1, "v1", collSchema))
	_, err := function.GetManager().Materialize(context.Background(), 1, "v1", collSchema.GetVersion(), &materializeTestInsertMessage{body: insertMsg.InsertRequest})
	assert.NoError(t, err)
	defer function.GetManager().Release(1, "v1")

	result, err := PrepareInsert(collSchema, pkField, []*msgstream.InsertMsg{insertMsg})

	assert.NoError(t, err)
	assert.Len(t, result, 1)
	assert.Contains(t, result[0].bm25Stats, int64(102))
	assert.Equal(t, int64(3), result[0].bm25Stats[102].NumRow())
	assert.NotNil(t, insertMsg.GetFieldsData()[2])
	assert.Equal(t, int64(102), insertMsg.GetFieldsData()[2].GetFieldId())
}

// finishWriteBufferSync settles one attempt; what that settlement means is the
// dropping × error-classification matrix below. One harness queues the entry,
// settles it, and asserts the task error passes through unchanged; each case
// keeps its original test's name and assertions.
func (s *WriteBufferSuite) TestFinishWriteBufferSyncClassification() {
	type classificationCase struct {
		name      string
		dropping  bool
		taskErr   error
		retryable bool // require the error classifies as SyncRetry first
		setup     func() (entry *writeBufferSyncEntry, verify func(fatalCalled bool))
	}

	terminalErr := merr.WrapErrDataIntegrityMsg("row count mismatch")
	cases := []classificationCase{
		{
			// A retryable failure must change nothing the retry depends on: the
			// task keeps its payload, its place in the queue and its metacache
			// syncing counters, and nobody is told the flush is finished.
			name:      "RetryableFailureKeepsTaskQueued",
			taskErr:   merr.WrapErrServiceInternalMsg("object storage unavailable"),
			retryable: true,
			setup: func() (*writeBufferSyncEntry, func(bool)) {
				segmentID := int64(1130)
				start := &msgpb.MsgPosition{Timestamp: 200}
				entry := newTestSyncEntry(segmentID, 300, entryStartPosition(start.GetTimestamp()), entrySubmitted)
				s.wb.syncCheckpoint.Add(segmentID, start, "syncing task")
				return entry, func(fatalCalled bool) {
					s.False(fatalCalled, "a retryable failure must not escalate")
					s.Contains(s.wb.writeBufferSyncQueues, segmentID)
					s.True(entry.failed)
					s.False(entry.submitted)
					select {
					case <-entry.done:
						s.Fail("the task is not finished, so waiters must not be released")
					default:
					}
					candidate := s.wb.syncCheckpoint.GetEarliestWithDefault(nil)
					s.Require().NotNil(candidate)
					s.Equal(start.GetTimestamp(), candidate.position.GetTimestamp())
				}
			},
		},
		{
			// A retryable failure during Drop keeps the task queued: Drop is a
			// synchronous wait and waitSyncsSettled drives these retries itself.
			// Only a terminal error may end the entry while dropping.
			name:      "RetryableFailureDuringDropStaysQueued",
			dropping:  true,
			taskErr:   merr.WrapErrServiceInternalMsg("object storage throttled"),
			retryable: true,
			setup: func() (*writeBufferSyncEntry, func(bool)) {
				segmentID := int64(1132)
				entry := newTestSyncEntry(segmentID, 300, entrySubmitted)
				return entry, func(fatalCalled bool) {
					s.False(fatalCalled, "a retryable failure during drop must not crash the process")
					s.Contains(s.wb.writeBufferSyncQueues, segmentID)
					s.True(entry.failed)
					s.True(s.wb.writeBufferSyncQueues[segmentID].intent.owes, "the failed attempt must arm the segment clock so flushRetryInterval is honored")
					select {
					case <-entry.done:
						s.Fail("a queued retry must not release drop waiters")
					default:
					}
				}
			},
		},
		{
			// A terminal failure during Drop surfaces through entry.terminalErr
			// to the synchronous drop wait, whose caller escalates. Feeding it to
			// the fatal errHandler as well would crash the process twice for one
			// failure.
			name:     "TerminalFailureDuringDropDoesNotDoubleEscalate",
			dropping: true,
			taskErr:  terminalErr,
			setup: func() (*writeBufferSyncEntry, func(bool)) {
				segmentID := int64(1133)
				entry := newTestSyncEntry(segmentID, 300, entrySubmitted)
				s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Return().Once()
				return entry, func(fatalCalled bool) {
					s.False(fatalCalled, "drop escalation is owned by the drop wait, not the fatal handler")
					s.NotContains(s.wb.writeBufferSyncQueues, segmentID)
					select {
					case <-entry.done:
						s.ErrorIs(entry.terminalErr, terminalErr)
					default:
						s.Fail("a terminal entry must release its drop waiters")
					}
				}
			},
		},
		{
			// Cancellation during Drop discards the task outright: the queue
			// entry, its payload accounting and its metacache syncing counters
			// are all settled, and nothing claims the payload was restored.
			name:     "DropCancellationDiscardsWriteBufferSyncTask",
			dropping: true,
			taskErr:  context.Canceled,
			setup: func() (*writeBufferSyncEntry, func(bool)) {
				segmentID := int64(1111)
				segment := metacache.NewSegmentInfo(&datapb.SegmentInfo{ID: segmentID}, nil, nil, metacache.NewEmptySegmentStats())
				metacache.UpdateBufferedRows(10)(segment)
				metacache.StartSyncing(10)(segment)
				s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Run(
					func(action metacache.SegmentAction, _ ...metacache.SegmentFilter) {
						action(segment)
					}).Return().Once()
				entry := newTestSyncEntry(segmentID, 200, entryBatchRows(10), entrySubmitted)
				return entry, func(bool) {
					s.NotContains(s.wb.writeBufferSyncQueues, segmentID)
					s.Zero(s.wb.MemorySize())
					s.Zero(segment.BufferRows())
					s.Zero(segment.SyncingRows())
					s.True(metacache.WithNoSyncingTask().Filter(segment))
				}
			},
		},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			entry, verify := tc.setup()
			s.wb.registerWriteBufferSyncLocked(entry)
			s.wb.dropping = tc.dropping
			fatalCalled := false
			s.wb.errHandler = func(error) { fatalCalled = true }

			if tc.retryable {
				s.Require().Equal(syncmgr.SyncRetry, syncmgr.ClassifySyncError(context.Background(), tc.taskErr))
			}
			s.ErrorIs(s.wb.finishWriteBufferSync(context.Background(), entry, entry.task, tc.taskErr), tc.taskErr)
			verify(fatalCalled)
		})
	}
}

// driveRetries only re-submits after the configured interval has passed, so a
// dense timetick stream cannot turn into a retry storm.
func (s *WriteBufferSuite) TestDriveRetriesHonorsInterval() {
	segmentID := int64(1131)
	entry := newTestSyncEntry(segmentID, 300, entryFailed)
	s.wb.registerWriteBufferSyncLocked(entry)
	// want()+attempted(), not want() alone: the entry is marked failed, i.e. an
	// attempt was just made and the interval has to elapse before the next one.
	s.wb.writeBufferSyncQueues[segmentID].intent.want()
	s.wb.writeBufferSyncQueues[segmentID].intent.attempted(time.Now())

	paramtable.Get().Save(paramtable.Get().DataNodeCfg.FlushRetryInterval.Key, "3000")
	defer paramtable.Get().Reset(paramtable.Get().DataNodeCfg.FlushRetryInterval.Key)

	s.wb.driveRetries(context.Background())
	s.True(entry.failed, "retry must wait for the interval to elapse")

	s.wb.writeBufferSyncQueues[segmentID].intent.since = time.Now().Add(-time.Hour)
	// Exactly one re-submission: the mock fails the test if the elapsed interval
	// does not reach SyncData, or reaches it more than once.
	s.syncMgr.EXPECT().SyncData(mock.Anything, mock.Anything, mock.Anything).
		Return(nil, nil).Once()
	s.wb.driveRetries(context.Background())
	s.False(entry.failed, "an elapsed interval must re-drive the segment")
	s.True(entry.submitted, "the due entry must actually be handed back to the sync manager")
}

// The first failed attempt must honor flushRetryInterval too: an unstamped
// lastAttempt used to make the very next timetick retry immediately.
func (s *WriteBufferSuite) TestFirstFailureHonorsRetryInterval() {
	segmentID := int64(1134)
	entry := newTestSyncEntry(segmentID, 300, entrySubmitted)
	task := entry.task
	s.wb.registerWriteBufferSyncLocked(entry)
	s.wb.errHandler = func(error) {}

	paramtable.Get().Save(paramtable.Get().DataNodeCfg.FlushRetryInterval.Key, "3000")
	defer paramtable.Get().Reset(paramtable.Get().DataNodeCfg.FlushRetryInterval.Key)

	taskErr := merr.WrapErrServiceInternalMsg("object storage unavailable")
	s.ErrorIs(s.wb.finishWriteBufferSync(context.Background(), entry, task, taskErr), taskErr)
	s.Require().True(entry.failed)

	s.wb.driveRetries(context.Background())
	s.True(entry.failed, "the interval must be measured from the first failure, not from zero")
}

// Once nothing can drive retries again, parked entries must be released or
// their payload is retained forever.
func (s *WriteBufferSuite) TestParkedEntriesReleasedWhenRetryStops() {
	segmentID := int64(1135)
	admission := newTestSyncTaskAdmission(nil)
	parkedEntry := newTestSyncEntry(segmentID, 300, entryFailed)
	parkedEntry.admission = admission
	s.wb.registerWriteBufferSyncLocked(parkedEntry)

	inflight := newTestSyncEntry(segmentID+1, 300, entrySubmitted)
	s.wb.registerWriteBufferSyncLocked(inflight)

	s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Return().Once()

	s.wb.mut.Lock()
	parked := s.wb.takeParkedWriteBufferSyncsLocked()
	s.wb.mut.Unlock()
	s.Require().Len(parked, 1)
	s.Same(parkedEntry, parked[0])
	s.Contains(s.wb.writeBufferSyncQueues, segmentID+1, "in-flight entries are settled by their own callback")
	s.NotContains(s.wb.writeBufferSyncQueues, segmentID)

	s.wb.abandonParkedWriteBufferSyncs(parked, context.Canceled)
	select {
	case <-parkedEntry.done:
		s.ErrorIs(parkedEntry.terminalErr, context.Canceled)
	default:
		s.Fail("an abandoned parked entry must release its waiters")
	}
	s.EqualValues(1, admission.physicalReleases.Load(), "parked payload admission must be released exactly once")
}

// A segment whose flush has failed at least once gets completion-based
// backpressure even after its payload was released (a stuck Commit holds no
// payload); a healthy over-budget queue does not.
func (s *WriteBufferSuite) TestBackpressureWaitsOnImpairedQueueCompletion() {
	segmentID := int64(1136)
	entry := newTestSyncEntry(segmentID, 300, entryFailed)
	entry.everFailed = true
	s.wb.registerWriteBufferSyncLocked(entry)
	s.wb.buffers[segmentID] = &segmentBuffer{
		segmentID:    segmentID,
		insertBuffer: &InsertBuffer{BufferBase: BufferBase{size: int64(1) << 40}},
	}
	defer delete(s.wb.buffers, segmentID)

	s.NotNil(s.wb.backpressureWaiterLocked(), "an impaired over-budget queue must backpressure on completion")

	entry.everFailed = false
	s.Nil(s.wb.backpressureWaiterLocked(), "a healthy over-budget pipeline must not wait on completion")
}

// A failed head must NOT be re-driven while a successor of the same segment is
// still in flight: a re-drive replays the whole queue from its oldest task, so
// the in-flight task would be submitted a second time.
func (s *WriteBufferSuite) TestDueRetriesSkipQueueWithInFlightSuccessor() {
	segmentID := int64(1137)
	head := newTestSyncEntry(segmentID, 300, entryFailed)
	successor := newTestSyncEntry(segmentID, 400, entrySubmitted)
	s.wb.registerWriteBufferSyncLocked(head)
	s.wb.registerWriteBufferSyncLocked(successor)
	// The interval has long elapsed; only the in-flight successor holds it back.
	s.wb.writeBufferSyncQueues[segmentID].intent.want()
	s.wb.writeBufferSyncQueues[segmentID].intent.attempted(time.Now().Add(-time.Hour))

	due := s.wb.dueWriteBufferRetriesLocked(time.Now(), time.Millisecond)
	s.Empty(due, "a queue with an in-flight successor must not be re-driven")
	s.True(head.failed, "the parked head must stay parked")
	s.False(head.submitted)
	s.True(s.wb.writeBufferSyncQueues[segmentID].intent.owes, "the debt must survive the skipped round")

	// Once the successor settles (fails back into the queue), the same queue is due.
	successor.submitted = false
	successor.failed = true
	due = s.wb.dueWriteBufferRetriesLocked(time.Now(), time.Millisecond)
	s.Require().Len(due, 2, "with nothing in flight the whole queue replays from its oldest task")
	s.Same(head, due[0])
	s.Same(successor, due[1])
}

// waitSyncsSettled with a NON-cancellable caller context must still terminate:
// after the hard bound it reports failure — never nil, which would claim a
// successful drain for data still in flight.
func (s *WriteBufferSuite) TestWaitSyncsSettledHardBoundReportsFailure() {
	param := paramtable.Get()
	param.Save(param.DataNodeCfg.GracefulStopTimeout.Key, "1")
	defer param.Reset(param.DataNodeCfg.GracefulStopTimeout.Key)

	entry := newTestSyncEntry(1139, 300, entrySubmitted)
	defer close(entry.done)

	start := time.Now()
	err := s.wb.waitSyncsSettled(context.Background(), nil, []*writeBufferSyncEntry{entry})
	s.Require().Error(err, "a drain that gave up must not report success")
	s.ErrorContains(err, "hard bound")
	s.Less(time.Since(start), 10*time.Second, "the hard bound must terminate the wait")
}

// A failed growing-source task whose progress entry is already gone (abortDrop
// clears the map while tasks are in flight) must STILL settle what the task
// reserved: the metacache syncing rows and the pinned checkpoint candidate.
func (s *WriteBufferSuite) TestFailedGrowingTaskSettlesAfterProgressCleared() {
	segmentID := int64(1138)
	start := &msgpb.MsgPosition{Timestamp: 200}
	task := syncmgr.NewGrowingSourceSyncTask().
		WithSegmentID(segmentID).
		WithChannelName(s.channelName).
		WithStartPosition(start).
		WithCheckpoint(&msgpb.MsgPosition{Timestamp: 300}).
		WithBatchRows(5)
	s.wb.syncCheckpoint.Add(segmentID, start, "growing source syncing task")
	s.Require().NotContains(s.wb.growingSourceProgress, segmentID,
		"the point of this test: no progress entry exists for the callback")

	var captured metacache.SegmentAction
	s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).
		Run(func(action metacache.SegmentAction, _ ...metacache.SegmentFilter) {
			captured = action
		}).Return().Once()
	s.metacache.EXPECT().GetSegmentByID(segmentID).Return(nil, false).Once()

	taskErr := merr.WrapErrServiceInternalMsg("object storage unavailable")
	s.ErrorIs(s.wb.finishGrowingSourceSync(context.Background(), task, taskErr), taskErr)

	// The captured action must undo a StartSyncing(5) — i.e. it is AbortSyncing
	// for exactly the rows this task claimed.
	s.Require().NotNil(captured, "the failed task must settle its syncing reservation")
	probe := metacache.NewSegmentInfo(&datapb.SegmentInfo{ID: segmentID}, nil, nil, nil)
	metacache.StartSyncing(5)(probe)
	captured(probe)
	s.EqualValues(0, probe.SyncingRows(), "syncingRows must not stay inflated after abortDrop")
	s.Nil(s.wb.syncCheckpoint.GetEarliestWithDefault(nil), "the checkpoint candidate must not stay pinned")
}

// buildTerminalMetadataOnlyTask builds the terminal write-buffer task for a
// segment with NO buffered payload — the shape a ManualFlush produces when the
// periodic flush already drained the buffer, and the shape a drop produces for
// a drained segment. Returns the task and its queue entry, marked in flight.
func (s *WriteBufferSuite) buildTerminalMetadataOnlyTask(segmentID int64, state commonpb.SegmentState) (*syncmgr.SyncTask, *writeBufferSyncEntry) {
	segment := metacache.NewSegmentInfo(&datapb.SegmentInfo{
		ID:             segmentID,
		State:          state,
		StorageVersion: storage.StorageV2,
	}, nil, nil, metacache.NewEmptySegmentStats())
	s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Run(
		func(action metacache.SegmentAction, _ ...metacache.SegmentFilter) {
			action(segment)
		}).Return().Maybe()
	s.metacache.EXPECT().GetSegmentByID(segmentID).Return(segment, true).Maybe()

	s.wb.mut.Lock()
	task, err := s.wb.getWriteBufferSyncTask(context.Background(), segment)
	var entry *writeBufferSyncEntry
	if syncTask, ok := task.(*syncmgr.SyncTask); ok {
		entry = s.wb.writeBufferSyncEntryLocked(syncTask)
		if entry != nil {
			entry.submitted = true
		}
	}
	s.wb.mut.Unlock()
	s.Require().NoError(err)
	s.Require().NotNil(entry)
	return task.(*syncmgr.SyncTask), entry
}

// Reproduction of the flush-fence hole: a ManualFlush terminal task for a
// segment whose buffer was already drained is metadata-only (StartPosition ==
// nil). While it is in flight, GetCheckpoint must NOT pass the position the
// fence was consumed from — a crash in that window replays the WAL from the
// reported checkpoint, and only a position at-or-before the fence redelivers
// the FlushMsg that regenerates this task. Without the pin the segment would
// recover as Growing forever.
func (s *WriteBufferSuite) TestMetadataOnlyFlushTaskPinsCheckpoint() {
	segmentID := int64(1140)
	s.wb.checkpoint = &msgpb.MsgPosition{Timestamp: 500}

	task, _ := s.buildTerminalMetadataOnlyTask(segmentID, commonpb.SegmentState_Flushing)
	s.Require().Nil(task.StartPosition(), "a drained segment must yield a metadata-only task")
	s.Require().True(task.IsFlush())

	// The channel keeps consuming past the fence while the terminal task is in
	// flight.
	s.wb.mut.Lock()
	s.wb.checkpoint = &msgpb.MsgPosition{Timestamp: 900}
	s.wb.mut.Unlock()

	s.EqualValues(500, s.wb.GetCheckpoint().GetTimestamp(),
		"checkpoint must stay at the flush fence while its terminal task is in flight")
}

// Success settles the fence: once the metadata-only flush commits
// Growing→Flushed, the pin is released and the checkpoint may advance.
func (s *WriteBufferSuite) TestMetadataOnlyFlushTaskUnpinsCheckpointOnSuccess() {
	segmentID := int64(1141)
	s.wb.checkpoint = &msgpb.MsgPosition{Timestamp: 500}
	task, entry := s.buildTerminalMetadataOnlyTask(segmentID, commonpb.SegmentState_Flushing)
	s.metacache.EXPECT().RemoveSegments(mock.Anything).Return(nil).Once()

	s.wb.mut.Lock()
	s.wb.checkpoint = &msgpb.MsgPosition{Timestamp: 900}
	s.wb.mut.Unlock()
	s.Require().EqualValues(500, s.wb.GetCheckpoint().GetTimestamp())

	s.NoError(s.wb.finishWriteBufferSync(context.Background(), entry, task, nil))

	s.EqualValues(900, s.wb.GetCheckpoint().GetTimestamp(),
		"a committed flush must release its fence pin")
}

// A retryable failure keeps the entry queued for the retry drive, so the fence
// pin must keep holding the checkpoint: the flush is still owed.
func (s *WriteBufferSuite) TestMetadataOnlyFlushTaskKeepsPinAcrossRetryableFailure() {
	segmentID := int64(1142)
	s.wb.checkpoint = &msgpb.MsgPosition{Timestamp: 500}
	task, entry := s.buildTerminalMetadataOnlyTask(segmentID, commonpb.SegmentState_Flushing)

	s.wb.mut.Lock()
	s.wb.checkpoint = &msgpb.MsgPosition{Timestamp: 900}
	s.wb.mut.Unlock()

	taskErr := merr.WrapErrServiceInternalMsg("object storage unavailable")
	s.Require().Equal(syncmgr.SyncRetry, syncmgr.ClassifySyncError(context.Background(), taskErr))
	s.ErrorIs(s.wb.finishWriteBufferSync(context.Background(), entry, task, taskErr), taskErr)

	s.Contains(s.wb.writeBufferSyncQueues, segmentID, "a retryable failure keeps the entry queued")
	s.EqualValues(500, s.wb.GetCheckpoint().GetTimestamp(),
		"the fence pin must hold while the flush is still owed")
}

// A drop task for a drained segment is metadata-only too. Pinning it while in
// flight is harmless (replaying a drop is idempotent) and uniform with flush;
// completion releases the pin.
func (s *WriteBufferSuite) TestMetadataOnlyDropTaskPinsUntilCompletion() {
	segmentID := int64(1143)
	s.wb.checkpoint = &msgpb.MsgPosition{Timestamp: 500}
	task, entry := s.buildTerminalMetadataOnlyTask(segmentID, commonpb.SegmentState_Dropped)
	s.Require().Nil(task.StartPosition())
	s.Require().True(task.IsDrop())

	s.wb.mut.Lock()
	s.wb.checkpoint = &msgpb.MsgPosition{Timestamp: 900}
	s.wb.mut.Unlock()
	s.Require().EqualValues(500, s.wb.GetCheckpoint().GetTimestamp(),
		"an in-flight drop task must pin the checkpoint it was enqueued at")

	s.NoError(s.wb.finishWriteBufferSync(context.Background(), entry, task, nil))
	s.EqualValues(900, s.wb.GetCheckpoint().GetTimestamp(),
		"a completed drop must release its pin")
}

// Data-task regression: a task carrying payload still pins its start position
// and releases exactly that pin on success.
func (s *WriteBufferSuite) TestDataTaskPinRemovedOnSuccess() {
	segmentID := int64(1144)
	segment := metacache.NewSegmentInfo(&datapb.SegmentInfo{
		ID:             segmentID,
		State:          commonpb.SegmentState_Growing,
		StorageVersion: storage.StorageV2,
	}, nil, nil, metacache.NewEmptySegmentStats())
	s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Run(
		func(action metacache.SegmentAction, _ ...metacache.SegmentFilter) {
			action(segment)
		}).Return().Maybe()
	s.metacache.EXPECT().GetSegmentByID(segmentID).Return(segment, true).Maybe()

	startPos := &msgpb.MsgPosition{Timestamp: 300}
	s.wb.mut.Lock()
	s.wb.checkpoint = &msgpb.MsgPosition{Timestamp: 400}
	s.wb.bufferDelete(segmentID,
		[]storage.PrimaryKey{storage.NewInt64PrimaryKey(1)},
		[]uint64{300}, startPos, &msgpb.MsgPosition{Timestamp: 301})
	task, err := s.wb.getWriteBufferSyncTask(context.Background(), segment)
	var entry *writeBufferSyncEntry
	if syncTask, ok := task.(*syncmgr.SyncTask); ok {
		entry = s.wb.writeBufferSyncEntryLocked(syncTask)
		entry.submitted = true
	}
	s.wb.mut.Unlock()
	s.Require().NoError(err)
	s.Require().NotNil(entry)
	syncTask := task.(*syncmgr.SyncTask)
	s.Require().EqualValues(300, syncTask.StartPosition().GetTimestamp(),
		"a payload-carrying task pins its start position, unchanged")

	s.Require().EqualValues(300, s.wb.GetCheckpoint().GetTimestamp())
	s.NoError(s.wb.finishWriteBufferSync(context.Background(), entry, syncTask, nil))
	s.EqualValues(400, s.wb.GetCheckpoint().GetTimestamp(),
		"a committed data task must release its start-position pin")
}

// Unique pin keys cannot collide with each other or with the plain
// segmentID-timestamp keys the growing-source path uses, so removing one pin
// never strips another that happens to share a timestamp.
func (s *WriteBufferSuite) TestCheckpointPinUniqueKeysDoNotCollide() {
	candidates := newCheckpointCandiates()
	position := &msgpb.MsgPosition{Timestamp: 500}

	candidates.Add(1, position, "growing source syncing task")
	firstKey := candidates.AddUnique(1, position, "syncing task")
	secondKey := candidates.AddUnique(1, position, "syncing task")
	s.NotEqual(firstKey, secondKey)

	candidates.RemoveUnique(firstKey)
	s.NotNil(candidates.GetEarliestWithDefault(nil), "the second unique pin and the plain pin must survive")
	candidates.RemoveUnique(secondKey)
	s.NotNil(candidates.GetEarliestWithDefault(nil), "the plain-keyed pin must survive unique removals")
	candidates.Remove(1, 500)
	s.Nil(candidates.GetEarliestWithDefault(nil))
}

// A retry driven while ANOTHER segment's admission wait is in progress must be
// bound to the buffer's lifetime (wb.syncCtx), not to that wait: the original
// bug was a task submitted under a context the admission wait canceled the
// moment it returned — classified SyncCanceled and terminally discarded, a
// silent data drop on a healthy node. Driving now flows through the
// bufferManager retry ticker (driveSyncRetries), simulated by a direct call
// here while reserveSyncTask is parked.
func (s *WriteBufferSuite) TestRetryDrivenDuringAdmissionWaitSurvivesReserveReturn() {
	segmentID := int64(1145)
	entry := newTestSyncEntry(segmentID, 300, entryFailed)
	s.wb.registerWriteBufferSyncLocked(entry)
	// A failed attempt far enough in the past that the next drive is due.
	s.wb.writeBufferSyncQueues[segmentID].intent.want()
	s.wb.writeBufferSyncQueues[segmentID].intent.attempted(time.Now().Add(-time.Hour))
	s.wb.flushRetryInterval = time.Millisecond

	taskCtxCh := make(chan context.Context, 1)
	s.syncMgr.EXPECT().SyncData(mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(ctx context.Context, _ syncmgr.Task, _ ...func(error) error) (*conc.Future[struct{}], error) {
			select {
			case taskCtxCh <- ctx:
			default:
			}
			return completedSyncFuture(nil), nil
		})

	allowReserve := make(chan struct{})
	reservable := &reservableTestSyncManager{
		SyncManager:    s.syncMgr,
		reserveStarted: make(chan struct{}),
		allowReserve:   allowReserve,
	}
	s.wb.syncMgr = reservable

	reserveDone := make(chan struct{})
	go func() {
		defer close(reserveDone)
		s.wb.reserveSyncTask(context.Background())
	}()
	select {
	case <-reservable.reserveStarted:
	case <-time.After(time.Second):
		s.FailNow("reserveSyncTask did not reach the sync manager")
	}

	// A backstop tick fires while the admission wait is parked.
	s.wb.driveSyncRetries()

	var taskCtx context.Context
	select {
	case taskCtx = <-taskCtxCh:
	case <-time.After(time.Second):
		s.FailNow("the backstop drive did not re-submit the parked entry")
	}

	close(allowReserve)
	select {
	case <-reserveDone:
	case <-time.After(time.Second):
		s.FailNow("reserveSyncTask did not return after the reservation was granted")
	}

	// Decisive: the admission wait has returned; the driven task's context must
	// still be alive...
	s.NoError(taskCtx.Err(), "a retried task must survive the admission wait that happened to overlap it")
	// ...and it must be the BUFFER's lifetime, nothing else: canceling
	// wb.syncCtx is what may end it.
	s.wb.syncCancel()
	s.Error(taskCtx.Err(), "the driven task must be bound to wb.syncCtx")
}
