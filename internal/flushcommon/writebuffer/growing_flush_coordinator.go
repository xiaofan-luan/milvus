// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package writebuffer

import (
	"context"
	"fmt"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/samber/lo"
	"golang.org/x/time/rate"

	"github.com/milvus-io/milvus-proto/go-api/v3/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v3/msgpb"
	"github.com/milvus-io/milvus/internal/flushcommon/metacache"
	"github.com/milvus-io/milvus/internal/flushcommon/syncmgr"
	"github.com/milvus-io/milvus/internal/storage"
	"github.com/milvus-io/milvus/internal/storagev2/packed"
	"github.com/milvus-io/milvus/pkg/v3/metrics"
	"github.com/milvus-io/milvus/pkg/v3/mlog"
	"github.com/milvus-io/milvus/pkg/v3/proto/datapb"
	"github.com/milvus-io/milvus/pkg/v3/util/merr"
	"github.com/milvus-io/milvus/pkg/v3/util/paramtable"
	"github.com/milvus-io/milvus/pkg/v3/util/tsoutil"
	"github.com/milvus-io/milvus/pkg/v3/util/typeutil"
)

// The write buffer's growing-segment flush path: source selection, per-segment
// progress, handoff to the write-buffer path, and retry state. Counterpart of
// write_buffer_sync_coordinator.go, which owns the path that flushes payload
// yielded out of the write buffer.
//
// The two differ in one way that explains most of the code below: a
// growing-source flush does NOT take ownership of the rows. They stay pinned in
// the segcore growing segment until CommitGrowingFlush, so a failed attempt
// costs a round trip and nothing else. The write-buffer path yields its payload and
// must therefore hold on to a failed task until it succeeds.
//
// State lives in writeBufferBase.growingSourceProgress (plus growingSettled) and
// is guarded by writeBufferBase.mut.
// It deliberately does not take a lock of its own: every decision here reads
// buffer state (buffers, metaCache, checkpoint) under that same lock, and a
// second lock would only add an ordering problem.

func (wb *writeBufferBase) AllowGrowingSourceFlush() bool {
	return wb.allowGrowingSourceFlush
}

// finishGrowingSourceSync owns the growing-source side of task completion, the
// counterpart of finishWriteBufferSync.
//
// Both ways a growing-source task can end — the sync manager's completion
// callback, and SyncData refusing the submission outright — go through here, so
// the failure bookkeeping (pendingCommitted capture, error classification,
// retry-vs-escalate) is stated exactly once.
//
// Returns the error the caller should propagate.
func (wb *writeBufferBase) finishGrowingSourceSync(ctx context.Context, task *syncmgr.GrowingSourceSyncTask, taskErr error) error {
	segmentID := task.SegmentID()
	var resyncSegmentID int64
	var fatalErr error
	removeFlushedSegment := taskErr == nil && task.IsFlush()
	decision := syncmgr.SyncRetry
	if taskErr != nil {
		decision = syncmgr.ClassifySyncError(ctx, taskErr)
		// Commit releases the source inline on its success paths. A task that
		// fails in Prepare, is rejected by the dispatcher, or is canceled while
		// queued never reaches it — release idempotently here for those.
		task.ReleaseSource()
	}

	wb.mut.Lock()
	// Task-derived settlement runs FIRST and unconditionally. Everything below it
	// is progress bookkeeping, which is legitimately skipped when the progress is
	// gone; giving back what this task reserved is not.
	if taskErr != nil {
		wb.settleFailedGrowingTaskLocked(task)
	}
	segmentDropped := false
	if segment, ok := wb.metaCache.GetSegmentByID(segmentID); ok {
		segmentDropped = segment.State() == commonpb.SegmentState_Dropped
	}
	if segmentDropped {
		// Drop is a monotonic debt independent of the flags frozen on the task.
		// In particular, a final-flush replay built before Drop must not erase the
		// Dropped state or remove the segment before a real drop task is committed.
		removeFlushedSegment = false
	}
	if progress, exists := wb.growingSourceProgress[segmentID]; exists {
		if segmentDropped {
			progress.owesDrop = true
		}
		if taskErr != nil {
			if task.HasCommittedFlush() && task.CommittedManifestPath() != "" {
				progress.pendingCommitted = &growingSourcePendingCommittedFlush{
					checkpoint:       task.Checkpoint(),
					batchRows:        task.BatchRows(),
					flushedThroughTs: task.FlushThroughTs(),
					isFlush:          task.IsFlush(),
					isDrop:           task.IsDrop(),
					manifestPath:     task.CommittedManifestPath(),
					bm25Stats:        cloneBM25StatsMap(task.CommittedBM25Stats()),
					insertBinlogs:    task.CommittedInsertBinlogs(),
					pkStats:          task.CommittedPKStats(),
				}
			}
			switch decision {
			case syncmgr.SyncCanceled:
				// Cancellation is lifecycle, not a failed storage attempt. Keep any
				// previous streak intact, but do not create/increment the failure
				// gauge or arm a retry that shutdown will never drive.
				wb.cancelGrowingSyncLocked(progress)
			case syncmgr.SyncTerminal:
				wb.failGrowingSyncLocked(progress, taskErr)
				wb.observeGrowingSourceSyncFailureLocked(segmentID, progress)
				// markNonRetryableFailure permanently parks this segment:
				// growingSourceProgressSyncable refuses it forever, so its batches
				// are never trimmed and the channel checkpoint stays pinned at
				// firstUncommittedPosition. Left silent that is an unbounded,
				// alert-less stall — strictly worse than a crash, because nothing
				// ever reports it. Fail loudly instead: the rows are still
				// recoverable from the WAL, and a human has to look at this.
				progress.markNonRetryableFailure()
				mlog.Error(ctx, "growing-source sync hit a terminal failure, escalating",
					mlog.Int64("segmentID", segmentID),
					mlog.Uint64("lastFlushedTs", progress.lastFlushedTs),
					mlog.String("lastFailure", progress.lastFailure))
				fatalErr = errors.Wrapf(taskErr, "growing-source sync unrecoverable, segmentID=%d lastFlushedTs=%d",
					segmentID, progress.lastFlushedTs)
			case syncmgr.SyncRetry:
				wb.failGrowingSyncLocked(progress, taskErr)
				wb.observeGrowingSourceSyncFailureLocked(segmentID, progress)
				wb.scheduleGrowingSourceRetryLocked(segmentID)
			}
		} else {
			if task.IsFlush() {
				progress.owesFlush = false
			}
			if task.IsDrop() {
				progress.owesDrop = false
			}
			if task.SourceFinalized() {
				progress.sourceFinalized = true
				if task.FlushThroughTs() > progress.sourceFinalizedThroughTs {
					progress.sourceFinalizedThroughTs = task.FlushThroughTs()
				}
			}
			wb.ackGrowingSyncLocked(progress, task.Checkpoint())
			wb.resetGrowingSourceSyncFailureMetric(segmentID)
			if progress.owesDrop {
				// A frozen replay only settles the range its manifest covers. Drop
				// remains owed and is built from the live tail after this callback.
				removeFlushedSegment = false
				if !wb.closed && !wb.dropping {
					resyncSegmentID = segmentID
				}
			} else if progress.owesFlush && len(progress.batches) == 0 {
				if _, ok := wb.metaCache.GetSegmentByID(segmentID); !ok {
					delete(wb.growingSourceProgress, segmentID)
				} else {
					// No claim here: the resync below goes through getSyncTask,
					// which claims Sealed itself.
					resyncSegmentID = segmentID
				}
			} else if len(progress.batches) == 0 {
				segment, ok := wb.metaCache.GetSegmentByID(segmentID)
				if task.IsFlush() || task.IsDrop() || !ok ||
					segment.State() == commonpb.SegmentState_Flushed ||
					segment.State() == commonpb.SegmentState_Dropped {
					delete(wb.growingSourceProgress, segmentID)
				}
			}
		}
	}
	if removeFlushedSegment {
		// Keep the decision and removal in the same write-buffer critical section
		// as DropPartitions. Otherwise Drop can mark the segment after the check
		// above but before RemoveSegments and lose its terminal debt.
		wb.metaCache.RemoveSegments(metacache.WithSegmentIDs(segmentID))
	}
	wb.mut.Unlock()

	// Deferred, not called inline: the fatal handler panics by default, and the
	// observer callback below must still run for this task first.
	if fatalErr != nil {
		defer wb.errHandler(fatalErr)
	}
	if resyncSegmentID != 0 {
		// The dispatcher keeps this task's key position until the callback
		// returns. Drive the follow-up independently so an admission wait cannot
		// pin that position and block the work needed to free another slot.
		go wb.syncSegments(wb.syncCtx, []int64{resyncSegmentID})
	}

	if taskErr != nil {
		if wb.taskObserverCallback != nil {
			wb.taskObserverCallback(task, taskErr)
		}
		return taskErr
	}

	if task.StartPosition() != nil {
		wb.syncCheckpoint.Remove(segmentID, task.StartPosition().GetTimestamp())
	}
	if removeFlushedSegment {
		mlog.Info(ctx, "flushed segment removed", mlog.FieldSegmentID(segmentID), mlog.String("channel", task.ChannelName()))
	}
	if wb.taskObserverCallback != nil {
		wb.taskObserverCallback(task, nil)
	}
	return nil
}

// GetGrowingFlushProgress reports growing-source progress as of right now.
//
// It deliberately does NOT wait for the write buffer to consume the release
// fence first. That wait would change nothing: growingProgressRequiresHandoff
// reports a growing-source segment as owing a flush until it is Flushed,
// whether or not the seal has been consumed yet, so WaitGrowingFlushDrained
// waits the ManualFlush out regardless. Waiting here only delayed the release
// on channels that owed nothing at all.
func (wb *writeBufferBase) GetGrowingFlushProgress(ctx context.Context, segmentIDs []int64) ([]GrowingFlushSegmentProgress, error) {
	// The caller must already have fenced growing-source admission — see
	// FenceGrowingSourceAdmission for why it has to happen before the
	// ManualFlush is appended, not here.
	// Reporting only. Waiting for these flushes to finish is a separate,
	// explicit step (WaitGrowingFlushDrained) so that callers who just want to
	// read progress are never blocked by one that is stuck.
	return wb.growingFlushProgressSnapshot(segmentIDs), nil
}

// FenceGrowingSourceAdmission stops NEW segments on this channel from being
// admitted to growing-source mode.
//
// It must be called BEFORE the release ManualFlush is appended, and that
// ordering is the whole correctness argument:
//
//   - A segment admitted before the fence was created by an insert that is
//     already in the WAL. WAL timestamps are monotonic and the ManualFlush is
//     appended afterwards, so its fence timestamp is above every such insert
//     and it seals all of them. They get flushed, and the drain converges.
//   - A segment admitted after the fence is refused growing-source mode and
//     buffers its rows in the write buffer, where they survive the release
//     without needing the delegator at all.
//
// Fencing any later leaves a window — even one as small as the gap between the
// append returning and this call — in which a segment can be admitted to
// growing-source mode without being sealed by that ManualFlush. Such a segment
// owes a flush forever from the drain's point of view (still FlushSourceGrowing,
// never Flushed), so the release blocks until its deadline.
//
// The fence records the newest provider registration token;
// growingSourceAdmissionOpenLocked reopens admission once a NEWER registration
// appears (a fresh local subscription after the release). It never moves
// backward, so a retried release only re-asserts it. An abandoned release
// therefore leaves the channel in write-buffer mode until it is re-subscribed:
// safe, and the cost of not having a rollback to get wrong.
func (wb *writeBufferBase) FenceGrowingSourceAdmission() {
	token := syncmgr.DefaultGrowingSourceRegistry().LatestRegistrationToken(wb.channelName)
	wb.mut.Lock()
	defer wb.mut.Unlock()
	if token > wb.growingSourceAdmissionFence {
		wb.growingSourceAdmissionFence = token
	}
}

// growingSourceAdmissionOpenLocked reports whether a NEW segment may still
// choose FlushSourceGrowing. Callers must hold mut.
func (wb *writeBufferBase) growingSourceAdmissionOpenLocked() bool {
	if wb.growingSourceAdmissionFence == 0 {
		return true
	}
	return syncmgr.DefaultGrowingSourceRegistry().LatestRegistrationToken(wb.channelName) > wb.growingSourceAdmissionFence
}

// growingFlushProgressSnapshot reports per-segment progress.
func (wb *writeBufferBase) growingFlushProgressSnapshot(segmentIDs []int64) []GrowingFlushSegmentProgress {
	wb.mut.RLock()
	defer wb.mut.RUnlock()

	if len(segmentIDs) == 0 {
		segmentIDs = lo.Keys(wb.growingSourceProgress)
	} else {
		segmentIDs = lo.Uniq(append(segmentIDs, lo.Keys(wb.growingSourceProgress)...))
	}

	progresses := make([]GrowingFlushSegmentProgress, 0, len(segmentIDs))
	for _, segmentID := range segmentIDs {
		progress := GrowingFlushSegmentProgress{
			SegmentID:  segmentID,
			SourceMode: metacache.FlushSourceUnknown,
		}
		if segment, ok := wb.metaCache.GetSegmentByID(segmentID); ok {
			progress.SourceMode = segment.FlushSourceMode()
		}
		if growingProgress, ok := wb.growingSourceProgress[segmentID]; ok {
			progress.FlushThroughTs = growingProgress.handoffFenceTs()
			progress.NeedReleaseHandoff = wb.growingProgressRequiresHandoff(segmentID, growingProgress)
			progress.SourceMode = metacache.FlushSourceGrowing
		}
		progresses = append(progresses, progress)
	}
	return progresses
}

// WaitGrowingFlushDrained blocks until no segment on this channel still owes a
// growing-source flush. segmentIDs is advisory (logging/context); the scan
// always covers the whole progress map so a segment admitted between the
// caller's snapshot and the admission fence is still waited out — the fence
// guarantees the map gains no NEW entries afterward, so this converges.
//
// Bounded only by ctx on purpose. Giving up early and releasing anyway would
// reintroduce exactly the unflushable state this wait exists to prevent, so a
// timeout is surfaced to the caller, which fails the release and lets the
// coordinator retry it.
func (wb *writeBufferBase) WaitGrowingFlushDrained(ctx context.Context, segmentIDs []int64) error {
	var pending []int64
	err := wb.waitFor(ctx, func(closed bool) (bool, error) {
		pending = pending[:0]
		for segmentID, progress := range wb.growingSourceProgress {
			if wb.growingProgressRequiresHandoff(segmentID, progress) {
				pending = append(pending, segmentID)
			}
		}
		if len(pending) == 0 {
			return true, nil
		}
		if closed {
			// The buffer is going down with the channel, so no further flush
			// will run for it. Safe to stop waiting: a batch that never
			// committed also never advanced the checkpoint, so recovery replays
			// the same WAL range.
			mlog.Info(ctx, "write buffer closed while waiting for growing-source flush to drain",
				mlog.String("channel", wb.channelName),
				mlog.Int64s("pendingSegments", pending))
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return errors.Wrapf(err,
			"growing-source flush not drained for segments %v on channel %s", pending, wb.channelName)
	}
	return nil
}

func (wb *writeBufferBase) growingProgressRequiresHandoff(segmentID int64, progress *growingSourceProgress) bool {
	if progress == nil {
		return false
	}
	if len(progress.batches) > 0 {
		return true
	}
	// Nothing left to write. Whatever terminal task remains is metadata-only,
	// and getGrowingSourceSyncTask builds that one WITHOUT reacquiring the
	// source once this same settlement proof holds (see
	// sourceSettlementSatisfied). Blocking the release on a source this segment
	// will never ask for again only delays the release.
	//
	// The proof is trustworthy: sourceFinalized is set at exactly one site, on
	// the success path of a terminal task, immediately after the source was told
	// the flush through that fence is durable.
	if progress.sourceFinalized && progress.sourceFinalizedThroughTs >= progress.handoffFenceTs() {
		return false
	}
	segment, ok := wb.metaCache.GetSegmentByID(segmentID)
	if !ok {
		return false
	}
	return segment.FlushSourceMode() == metacache.FlushSourceGrowing &&
		segment.State() != commonpb.SegmentState_Flushed
}

func (wb *writeBufferBase) hasGrowingSourceProgress(segmentID int64) bool {
	_, ok := wb.growingSourceProgress[segmentID]
	return ok
}

func (wb *writeBufferBase) decideGrowingFlushSource(segmentID int64, endPos *msgpb.MsgPosition) metacache.FlushSourceMode {
	// 1. Honor the sticky decision recorded in metacache. Once the first
	//    insert for a segment commits a source choice, every subsequent call
	//    must return the same kind so that progress / payload tracking stays
	//    consistent for the segment's lifetime.
	if seg, ok := wb.metaCache.GetSegmentByID(segmentID); ok {
		if seg.GetStorageVersion() != storage.StorageV3 {
			return metacache.FlushSourceWriteBuffer
		}
		switch seg.FlushSourceMode() {
		case metacache.FlushSourceGrowing:
			return metacache.FlushSourceGrowing
		case metacache.FlushSourceWriteBuffer:
			return metacache.FlushSourceWriteBuffer
		}
	}

	// 2. Fallback for the brief window where in-memory bookkeeping has been
	//    populated but the metacache sticky bit hasn't been set yet (e.g. on
	//    re-entry after a partial state).
	if wb.hasGrowingSourceProgress(segmentID) {
		return metacache.FlushSourceGrowing
	}

	if wb.hasWriteBufferInsertPayload(segmentID) {
		return metacache.FlushSourceWriteBuffer
	}

	// 3. Release fence. Once a release handoff has been prepared for this
	//    channel, a segment seen here for the first time was created after the
	//    release fence and its growing segment is about to be dropped with the
	//    channel unsubscribe. Admitting it to growing-source mode would leave
	//    its only data copy in a segment that will not survive the release, so
	//    buffer its rows in the write buffer instead. Segments admitted before
	//    the fence returned above via their sticky decision or progress entry
	//    and are waited out by WaitGrowingFlushDrained.
	if !wb.growingSourceAdmissionOpenLocked() {
		wb.warnGrowingSourceFallback(segmentID, endPos)
		return metacache.FlushSourceWriteBuffer
	}

	if state := wb.getGrowingSourceState(segmentID, endPos); state == syncmgr.GrowingSourceUsable || state == syncmgr.GrowingSourcePending {
		return metacache.FlushSourceGrowing
	}
	wb.warnGrowingSourceFallback(segmentID, endPos)
	return metacache.FlushSourceWriteBuffer
}

func (wb *writeBufferBase) getGrowingSource(segmentID int64, endPos *msgpb.MsgPosition) (syncmgr.GrowingFlushSource, syncmgr.GrowingSourceState) {
	if wb.growingSourceResolver == nil {
		return nil, syncmgr.GrowingSourceUnavailable
	}
	return wb.growingSourceResolver(segmentID, endPos)
}

func (wb *writeBufferBase) getGrowingSourceState(segmentID int64, endPos *msgpb.MsgPosition) syncmgr.GrowingSourceState {
	source, state := wb.getGrowingSource(segmentID, endPos)
	if source != nil {
		source.Release()
	}
	return state
}

func (wb *writeBufferBase) warnGrowingSourceFallback(segmentID int64, endPos *msgpb.MsgPosition) {
	if !wb.allowGrowingSourceFlush {
		return
	}
	wb.logger.RatedWarn(wb.syncCtx, rate.Limit(1), "growing-source source is unavailable, fallback to WriteBuffer",
		mlog.Int64("segmentID", segmentID),
		mlog.Any("endPosition", endPos),
	)
}

// growingSourceProgressSyncable reports whether this progress can produce a task
// now. Source resolution belongs to task construction, so every attempt takes
// exactly one source lease and has one owner responsible for releasing it.
//
// Its only writes record terminal debt observed while another task owns the
// segment. It does NOT touch metacache segment state: claiming the flush belongs
// to getSyncTask, where the content is fixed.
func (wb *writeBufferBase) growingSourceProgressSyncable(segmentID int64, progress *growingSourceProgress) bool {
	if progress.nonRetryableFailure {
		return false
	}
	segment, segmentExists := wb.metaCache.GetSegmentByID(segmentID)
	if segmentExists && segment.State() == commonpb.SegmentState_Dropped {
		progress.owesDrop = true
	}
	if progress.syncing {
		if segmentExists {
			switch segment.State() {
			case commonpb.SegmentState_Sealed, commonpb.SegmentState_Flushing:
				progress.owesFlush = true
			case commonpb.SegmentState_Dropped:
				progress.owesDrop = true
			}
		}
		return false
	}
	if progress.pendingCommitted != nil {
		return true
	}
	if len(progress.batches) == 0 && !progress.owesFlush && !progress.owesDrop {
		return false
	}
	if len(progress.batches) == 0 && !progress.owesDrop {
		if !segmentExists || (segment.State() != commonpb.SegmentState_Sealed && segment.State() != commonpb.SegmentState_Flushing) {
			return false
		}
	}
	checkpoint := wb.checkpoint
	if len(progress.batches) > 0 {
		checkpoint = progress.batches[len(progress.batches)-1].endPosition
	}
	if checkpoint == nil {
		return false
	}
	return true
}

// scheduleGrowingSourceRetryLocked arms one segment's clock. There is no timer:
// driveGrowingSourceRetries picks it up on the next timetick, the same signal
// the write-buffer queue rides.
func (wb *writeBufferBase) scheduleGrowingSourceRetryLocked(segmentID int64) {
	if wb.closed || wb.dropping || wb.flushRetryInterval < 0 {
		return
	}
	if progress, ok := wb.growingSourceProgress[segmentID]; ok {
		// The attempt that could not be made (or just failed) restarts the
		// clock; the debt itself only settles on a completed task.
		progress.intent.want()
		progress.intent.attempted(time.Now())
	}
}

// driveGrowingSourceRetries re-submits growing-source flushes that asked for
// another round, no more often than the configured interval.
func (wb *writeBufferBase) driveGrowingSourceRetries(ctx context.Context, now time.Time, interval time.Duration) {
	wb.mut.Lock()
	if wb.closed || wb.dropping || wb.checkpoint == nil || len(wb.growingSourceProgress) == 0 {
		wb.mut.Unlock()
		return
	}
	segmentIDs := wb.getGrowingSourceSegmentsToRetry(now, interval)
	for _, segmentID := range segmentIDs {
		// The source resolution in getGrowingSourceSyncTask is the attempt. Stamp
		// it now so an unavailable source is re-armed from this round rather than
		// from the first failure forever.
		wb.growingSourceProgress[segmentID].intent.attempted(now)
	}
	if len(segmentIDs) > 0 {
		wb.logger.Info(ctx, "retry growing-source sync", mlog.Int64s("segmentIDs", segmentIDs))
	}
	wb.mut.Unlock()

	if len(segmentIDs) > 0 {
		wb.syncSegments(wb.syncCtx, segmentIDs)
	}
}

// getGrowingSourceSegmentsToRetry returns the segments whose clock is due and
// which can produce a task now. Ineligible segments keep their existing debt;
// source-unavailable failures are re-armed by task construction.
func (wb *writeBufferBase) getGrowingSourceSegmentsToRetry(now time.Time, interval time.Duration) (due []int64) {
	for segmentID, progress := range wb.growingSourceProgress {
		if !progress.intent.due(now, interval) {
			continue
		}
		if wb.growingSourceProgressSyncable(segmentID, progress) {
			due = append(due, segmentID)
		}
	}
	return due
}

func (wb *writeBufferBase) recordGrowingSourceProgress(inData *InsertData, startPos, endPos *msgpb.MsgPosition, schemaVersion int32) error {
	err := wb.CreateNewGrowingSegment(CreateGrowingSegmentInfo{
		PartitionID:   inData.partitionID,
		SegmentID:     inData.segmentID,
		StartPos:      startPos,
		SchemaVersion: schemaVersion,
	})
	if err != nil {
		return err
	}
	segment, ok := wb.metaCache.GetSegmentByID(inData.segmentID)
	if !ok {
		return merr.WrapErrSegmentNotFound(inData.segmentID)
	}
	if segment.GetStorageVersion() != storage.StorageV3 {
		return merr.WrapErrServiceInternalMsg("growing-source flush requires StorageV3 segment, segmentID=%d storageVersion=%d",
			inData.segmentID, segment.GetStorageVersion())
	}
	progress, ok := wb.growingSourceProgress[inData.segmentID]
	if !ok {
		lastFlushedPosition := segment.LastFlushPosition()
		var clonedLastFlushedPosition *msgpb.MsgPosition
		if lastFlushedPosition != nil {
			clonedLastFlushedPosition = typeutil.Clone(lastFlushedPosition)
		}
		progress = &growingSourceProgress{
			segmentID:           inData.segmentID,
			lastFlushedPosition: clonedLastFlushedPosition,
			// Where this segment was last flushed to. On a fresh segment it is
			// zero; on one recovered mid-flush it comes from the position the
			// last successful flush persisted.
			lastFlushedTs: lastFlushedPosition.GetTimestamp(),
		}
		wb.growingSourceProgress[inData.segmentID] = progress
	}
	progress.batches = append(progress.batches, growingSourceProgressBatch{
		startPosition: startPos,
		endPosition:   endPos,
		rowNum:        inData.rowNum,
	})
	// SetFlushSourceMode is sticky: only the first call commits the choice,
	// so we can include it unconditionally here without overriding a prior
	// FlushSourceWriteBuffer decision.
	wb.metaCache.UpdateSegments(metacache.MergeSegmentAction(
		metacache.SetStartPositionIfNil(startPos),
		metacache.SetFlushSourceMode(metacache.FlushSourceGrowing),
		wb.updateGrowingSourceBufferedRows(progress),
	), metacache.WithSegmentIDs(inData.segmentID))
	wb.notifyFlushSourceMode(inData.segmentID)
	return nil
}

func (wb *writeBufferBase) updateGrowingSourceBufferedRows(progress *growingSourceProgress) metacache.SegmentAction {
	// pendingRows already excludes everything a flush has acknowledged, so
	// FlushedRows does not enter this — one less place where a row count
	// from this side has to line up with the growing segment's own. Both
	// terms belong to progress and move under wb.mut, so the difference
	// cannot go negative.
	return metacache.UpdateBufferedRows(progress.pendingRows() - progress.claimedRows)
}

func (wb *writeBufferBase) growingSourceProgressSelectedByPolicy(ts typeutil.Timestamp, segmentID int64, progress *growingSourceProgress) bool {
	if progress == nil {
		return false
	}
	if progress.nonRetryableFailure {
		return false
	}
	if progress.owesFlush || progress.owesDrop {
		return true
	}
	segment, ok := wb.metaCache.GetSegmentByID(segmentID)
	if ok {
		switch segment.State() {
		case commonpb.SegmentState_Sealed, commonpb.SegmentState_Flushing, commonpb.SegmentState_Dropped:
			return true
		}
		if wb.growingSourceProgressFull(segment, progress) {
			return true
		}
	}
	startPos := progress.firstUncommittedPosition()
	if startPos == nil {
		return false
	}
	staleDuration := paramtable.Get().DataNodeCfg.SyncPeriod.GetAsDuration(time.Second)
	current := tsoutil.PhysicalTime(ts)
	start := tsoutil.PhysicalTime(startPos.GetTimestamp())
	return current.Sub(start) > staleDuration
}

func (wb *writeBufferBase) growingSourceProgressFull(segment *metacache.SegmentInfo, progress *growingSourceProgress) bool {
	if segment == nil || progress == nil {
		return false
	}
	rows := progress.pendingRows() - progress.claimedRows
	if rows <= 0 {
		return false
	}
	if wb.estSizePerRecord <= 0 {
		return false
	}
	thresholdRows := int64(wb.getEstBatchSize())
	if thresholdRows <= 0 {
		return true
	}
	return rows >= thresholdRows
}

// noteGrowingSourceCandidateFailed records the failed attempt of a candidate that
// could not produce its task: failure counters, metric, retry intent.
//
// Nothing is rolled back — getGrowingSourceSyncTask sets syncing only on its
// success paths, so there is no bookkeeping to reverse. The segment's own state
// is left alone too: a claimed flush stays claimed, and GetSealedSegmentsPolicy
// re-selects Flushing segments, so the retry resumes the SAME flush instead of
// re-deciding what to flush.
func (wb *writeBufferBase) noteGrowingSourceCandidateFailed(segmentID int64) {
	if progress, ok := wb.growingSourceProgress[segmentID]; ok {
		wb.failGrowingSyncLocked(progress, errGrowingSourceUnavailable)
		wb.observeGrowingSourceSyncFailureLocked(segmentID, progress)
		wb.scheduleGrowingSourceRetryLocked(segmentID)
	}
}

// settleFailedGrowingTaskLocked returns what a failed task RESERVED: the
// metacache syncing rows it claimed, and the checkpoint candidate it pinned.
//
// Derived entirely from the task, and therefore run unconditionally — never
// under a lookup of growingSourceProgress. A concurrent abortDrop clears that
// map while tasks are still in flight, and a callback landing afterwards used to
// skip this entirely: the segment kept inflated syncingRows forever and the
// channel checkpoint stayed pinned behind a candidate nobody would remove.
func (wb *writeBufferBase) settleFailedGrowingTaskLocked(task *syncmgr.GrowingSourceSyncTask) {
	if task.BatchRows() > 0 {
		wb.metaCache.UpdateSegments(metacache.AbortSyncing(task.BatchRows()), metacache.WithSegmentIDs(task.SegmentID()))
	}
	if task.StartPosition() != nil {
		wb.syncCheckpoint.Remove(task.SegmentID(), task.StartPosition().GetTimestamp())
	}
}

func (wb *writeBufferBase) observeGrowingSourceSyncFailureLocked(segmentID int64, progress *growingSourceProgress) {
	wb.updateGrowingSourceSyncFailureMetricLocked()

	if progress.failureCount < growingSourceSyncFailureWarnThreshold ||
		progress.failureCount%growingSourceSyncFailureWarnThreshold != 0 {
		return
	}

	wb.logger.RatedWarn(wb.syncCtx, rate.Limit(1), "growing-source source sync keeps failing",
		mlog.Int64("segmentID", segmentID),
		mlog.Int64("failureCount", progress.failureCount),
		mlog.Uint64("lastFlushedTs", progress.lastFlushedTs),
		mlog.String("lastFailure", progress.lastFailure),
	)
}

func (wb *writeBufferBase) resetGrowingSourceSyncFailureMetric(segmentID int64) {
	if progress, ok := wb.growingSourceProgress[segmentID]; ok {
		progress.failureCount = 0
		progress.lastFailure = ""
	}
	wb.updateGrowingSourceSyncFailureMetricLocked()
}

// updateGrowingSourceSyncFailureMetricLocked publishes the worst consecutive
// failure streak on this channel. A channel-scoped gauge cannot safely be set to
// one segment's value: a success on a different segment would hide the failure.
func (wb *writeBufferBase) updateGrowingSourceSyncFailureMetricLocked() {
	if wb.growingSourceFailureMetricSettled {
		return
	}
	var maxFailures int64
	for _, progress := range wb.growingSourceProgress {
		if progress.failureCount > maxFailures {
			maxFailures = progress.failureCount
		}
	}
	if maxFailures == 0 {
		metrics.DataNodeGrowingSourceSyncFailureCount.DeleteLabelValues(
			paramtable.GetStringNodeID(),
			fmt.Sprint(wb.collectionID),
			wb.channelName,
		)
		return
	}
	metrics.DataNodeGrowingSourceSyncFailureCount.WithLabelValues(
		paramtable.GetStringNodeID(),
		fmt.Sprint(wb.collectionID),
		wb.channelName,
	).Set(float64(maxFailures))
}

// settleGrowingSourceFailureMetricLocked ends this write buffer's ownership of
// the channel-scoped gauge. DataSyncService closes before the write-buffer
// manager on the streaming path, so flowgraph cleanup cannot safely delete it:
// an in-flight callback could recreate the series afterwards. The settled flag
// makes every such late callback a no-op.
func (wb *writeBufferBase) settleGrowingSourceFailureMetricLocked() {
	if wb.growingSourceFailureMetricSettled {
		return
	}
	wb.growingSourceFailureMetricSettled = true
	metrics.DataNodeGrowingSourceSyncFailureCount.DeleteLabelValues(
		paramtable.GetStringNodeID(),
		fmt.Sprint(wb.collectionID),
		wb.channelName,
	)
}

func (wb *writeBufferBase) getGrowingSourceSyncTask(ctx context.Context, segmentInfo *metacache.SegmentInfo, progress *growingSourceProgress) (syncmgr.Task, error) {
	if segmentInfo.GetStorageVersion() != storage.StorageV3 {
		return nil, merr.WrapErrServiceInternalMsg("growing-source sync requires StorageV3 segment, segmentID=%d storageVersion=%d",
			segmentInfo.SegmentID(), segmentInfo.GetStorageVersion())
	}
	pendingCommitted := progress.pendingCommitted
	if segmentInfo.State() == commonpb.SegmentState_Dropped {
		progress.owesDrop = true
	}
	startPos := progress.firstUncommittedPosition()

	// The flush target is a POSITION this side already holds — the newest pack
	// recorded for this segment — not a count derived from anything. It is
	// published unchanged as the checkpoint, so the range written and the
	// position published cannot drift apart.
	checkpoint := progress.flushTarget()
	if pendingCommitted != nil {
		// Replay of a flush whose data is already in storage: publish the exact
		// position that manifest covers, never a newer one.
		checkpoint = pendingCommitted.checkpoint
	}
	if checkpoint == nil {
		// No pack recorded: this is a metadata-only flush (a sealed segment that
		// owes a flush but holds no new rows). The fence must NOT advance here —
		// falling back to the channel's latest consumed position would move
		// lastFlushedTs over ground this segment never verified, and the next
		// flush would start above it. Re-publish the position already reached.
		checkpoint = segmentInfo.LastFlushPosition()
	}
	if checkpoint == nil {
		checkpoint = startPos
	}
	schemaTimestamp := uint64(0)
	if startPos != nil {
		schemaTimestamp = startPos.GetTimestamp()
	}
	// This side's own tally of the rows in the range, used only to cross-check
	// what the source reports it wrote. A committed-flush replay carries the
	// count frozen with its manifest instead.
	batchSize := progress.pendingRows() - progress.claimedRows
	if pendingCommitted != nil {
		batchSize = pendingCommitted.batchRows
	}
	sourceSettlementSatisfied := progress.sourceFinalized &&
		batchSize == 0 &&
		progress.sourceFinalizedThroughTs >= checkpoint.GetTimestamp() &&
		(progress.owesFlush || progress.owesDrop)
	source, state := wb.getGrowingSource(progress.segmentID, checkpoint)
	if state != syncmgr.GrowingSourceUsable {
		if source != nil {
			source.Release()
			source = nil
		}
		// One three-way disposition of an unusable source: a committed-manifest
		// replay proceeds without it (SaveBinlogPaths needs no re-flush), a
		// settled metadata-only terminal sync proceeds without it, and anything
		// else must wait for the source to come back.
		switch {
		case pendingCommitted != nil:
			wb.logger.Warn(ctx, "growing source unavailable during committed flush ack retry; retrying SaveBinlogPaths without re-flush",
				mlog.Int64("segmentID", progress.segmentID),
				mlog.Uint64("flushThroughTs", checkpoint.GetTimestamp()),
				mlog.Int("state", int(state)))
		case sourceSettlementSatisfied:
			wb.logger.Info(ctx, "growing source already finalized; continue metadata-only terminal sync without reacquiring it",
				mlog.FieldSegmentID(progress.segmentID),
				mlog.Uint64("flushThroughTs", checkpoint.GetTimestamp()))
		default:
			return nil, errors.Wrapf(errGrowingSourceUnavailable, "segment %d state %d", progress.segmentID, state)
		}
	}

	buildTask := func(batchRows int64) *syncmgr.GrowingSourceSyncTask {
		task := syncmgr.NewGrowingSourceSyncTask().
			WithCollectionID(wb.collectionID).
			WithPartitionID(segmentInfo.PartitionID()).
			WithSegmentID(progress.segmentID).
			WithChannelName(wb.channelName).
			WithStartPosition(startPos).
			WithCheckpoint(checkpoint).
			WithBatchRows(batchRows).
			WithFlushFromTs(progress.lastFlushedTs).
			WithLevel(segmentInfo.Level()).
			WithMetaCache(wb.metaCache).
			WithMetaWriter(wb.metaWriter).
			WithSchema(wb.metaCache.GetSchema(schemaTimestamp)).
			WithAllocator(wb.allocator).
			WithStorageConfig(packed.CreateStorageConfig()).
			// Non-fatal on purpose: the rows stay pinned in the growing segment
			// until CommitGrowingFlush, so a failed attempt costs nothing but a
			// round trip. A retryable failure only arms the segment's intent
			// (scheduleGrowingSourceRetryLocked); driveGrowingSourceRetries
			// builds the NEXT attempt. Escalation to the fatal handler happens
			// only where recovery is impossible — see the SyncTerminal branch in
			// finishGrowingSourceSync.
			WithFailureCallback(wb.growingSourceErrHandler)
		if source != nil {
			task.WithSource(source)
		}
		if pendingCommitted != nil {
			task.WithCommittedFlush(pendingCommitted.manifestPath, cloneBM25StatsMap(pendingCommitted.bm25Stats), pendingCommitted.insertBinlogs)
			task.WithCommittedPKStats(pendingCommitted.pkStats)
		}
		// The finalization flags come from the frozen attempt when replaying a
		// committed manifest: the manifest covers exactly the frozen range, so
		// only the attempt that wrote it knows whether it was final. Deriving
		// them from the CURRENT state would upgrade a periodic sync to the
		// final flush after a concurrent seal — publishing a manifest that does
		// not cover the sealed tail as the segment's last word.
		if pendingCommitted != nil {
			if pendingCommitted.isFlush {
				task.WithFlush()
			}
			if pendingCommitted.isDrop {
				task.WithDrop()
			}
			return task
		}
		if segmentInfo.State() == commonpb.SegmentState_Flushing {
			task.WithFlush()
		}
		if progress.owesDrop {
			task.WithDrop()
		}
		return task
	}

	if batchSize <= 0 {
		progress.intent.clear()
		progress.syncing = true
		return buildTask(0), nil
	}

	if startPos != nil {
		wb.syncCheckpoint.Add(progress.segmentID, startPos, "growing source syncing task")
	}
	progress.syncing = true
	progress.claimedRows = batchSize
	progress.intent.clear()
	wb.metaCache.UpdateSegments(metacache.StartSyncing(batchSize), metacache.WithSegmentIDs(progress.segmentID))

	return buildTask(batchSize), nil
}

// growingSourceProgress and friends: the per-segment state this path owns.
//
// A growing-source flush does not take the rows — they stay pinned in segcore
// until CommitGrowingFlush — so progress is expressed as offsets and batches
// rather than as a queue of tasks holding payload.

const growingSourceSyncFailureWarnThreshold = 600

var errGrowingSourceUnavailable = errors.New("growing source is unavailable")

type growingSourceProgress struct {
	segmentID int64
	// lastFlushedPosition is the complete checkpoint already known durable for
	// this segment. Metadata-only flush/drop debt uses it to keep the WAL behind
	// the control message that created the debt even when no data batch remains.
	lastFlushedPosition *msgpb.MsgPosition
	// lastFlushedTs is the timestamp of the position this segment was last
	// flushed through — the lower fence of the next flush. It is the only
	// "how far along am I" state this side keeps, and it is a POSITION, not a
	// row count: row counts live in the growing segment's own coordinate
	// system, which a WAL replay resets.
	lastFlushedTs uint64
	syncing       bool
	// claimedRows is how many of pendingRows() the in-flight task took as its
	// batch. Owned by progress and updated under the same lock as batches, so
	// "buffered = pendingRows() - claimedRows" is a single-clock read — the
	// metacache SyncingRows counter tracks the same quantity but is advanced by
	// the task at different moments, and subtracting across the two clocks
	// produced transient negatives. Zero whenever no task is in flight.
	claimedRows int64
	// sourceFinalized records the proof separately from its fence: timestamp zero
	// is also the protobuf default and must not mean that a source was notified.
	// sourceFinalizedThroughTs is the highest terminal fence already delivered to
	// the growing source. A concurrent Drop may still need a metadata-only task,
	// but it must not require a source that the earlier finalization released.
	sourceFinalized          bool
	sourceFinalizedThroughTs uint64
	// owesFlush is sticky: seal sets it, and only a successful flush task clears
	// it. NOT the same bit as intent — a segment can owe a flush while having no
	// outstanding attempt to make, and vice versa.
	owesFlush bool
	// owesDrop is a separate monotonic terminal debt. A committed-manifest
	// replay never absorbs a Drop that arrived later; after the frozen replay is
	// acknowledged, this bit drives a new task over the remaining live tail.
	owesDrop            bool
	pendingCommitted    *growingSourcePendingCommittedFlush
	nonRetryableFailure bool
	batches             []growingSourceProgressBatch
	failureCount        int64
	lastFailure         string
	// intent is this segment's flush debt, the same type the write-buffer queue
	// uses. Distinct from owesFlush: intent is "try again", owesFlush is "a
	// FLUSH is still owed".
	intent flushIntent
}

// growingSourcePendingCommittedFlush is a flush whose DATA reached storage but
// whose metadata commit did not. The retry must re-publish exactly what was
// written, so the position and the row count are FROZEN here alongside the
// manifest.
//
// Re-deriving them from the live progress would be silent data loss: by the time
// the retry runs, later packs may have been recorded, and the retry would then
// publish the newer position while reusing the old manifest — acking away rows
// that were never persisted.
type growingSourcePendingCommittedFlush struct {
	checkpoint       *msgpb.MsgPosition
	batchRows        int64
	flushedThroughTs uint64
	// isFlush/isDrop are frozen with the manifest, NOT re-derived from the
	// segment's state at retry time. A periodic sync to T1 whose ack failed can
	// be retried after the segment sealed with T2 data recorded; deriving the
	// flag then would replay the T1-only manifest as the FINAL flush — Commit
	// would mark the segment Flushed and remove it while T2 still pins the
	// checkpoint, with no way to ever build the task that covers it.
	isFlush       bool
	isDrop        bool
	manifestPath  string
	bm25Stats     map[int64]*storage.BM25Stats
	insertBinlogs map[int64]*datapb.FieldBinlog
	pkStats       *storage.PrimaryKeyStats
}

// growingSourceProgressBatch is one WAL message pack's contribution to a
// segment, recorded when the pack was consumed.
//
// endPosition is the flush fence AND the checkpoint: a pack's rows all carry a
// timestamp <= its end position's, and the next pack's rows carry a strictly
// greater one, so a fence set here can never split a pack. rowNum is this
// side's own tally, used only to cross-check what the source reports it wrote.
type growingSourceProgressBatch struct {
	startPosition *msgpb.MsgPosition
	endPosition   *msgpb.MsgPosition
	rowNum        int64
}

func (p *growingSourceProgress) firstUncommittedPosition() *msgpb.MsgPosition {
	if len(p.batches) == 0 {
		return nil
	}
	return p.batches[0].startPosition
}

// checkpointPosition is the earliest WAL position this progress still owns.
// Data batches pin their own start position. Once all data is durable, a
// metadata-only flush/drop still pins the last durable position until that
// terminal metadata is committed; otherwise the control-message pack can be
// checkpointed before the task that applies it finishes.
func (p *growingSourceProgress) checkpointPosition() *msgpb.MsgPosition {
	if position := p.firstUncommittedPosition(); position != nil {
		return position
	}
	if p.owesFlush || p.owesDrop || p.syncing || p.pendingCommitted != nil {
		return p.lastFlushedPosition
	}
	return nil
}

// flushTarget is the position the next flush should run to: the newest pack
// recorded for this segment. Everything recorded is flushed in one go — there is
// no partial target to compute, because the fence is a position this side
// already holds rather than a count it has to derive.
func (p *growingSourceProgress) flushTarget() *msgpb.MsgPosition {
	if len(p.batches) == 0 {
		return nil
	}
	return p.batches[len(p.batches)-1].endPosition
}

// handoffFenceTs is how far this segment must be flushed before its growing
// source may be released, as a WAL timestamp.
//
// With packs outstanding it is the newest one's end position — releasing before
// that would drop rows still only in the segment. With none outstanding the
// segment still owes a metadata-only final flush, so the fence is where it was
// last flushed to; reporting zero there would skip retention entirely and let
// the source go away before that flush runs.
func (p *growingSourceProgress) handoffFenceTs() uint64 {
	if target := p.flushTarget(); target != nil {
		return target.GetTimestamp()
	}
	return p.lastFlushedTs
}

// pendingRows is this side's tally of the rows recorded but not yet flushed.
// Cross-checked against the source's report; never used to bound the flush.
func (p *growingSourceProgress) pendingRows() int64 {
	var rows int64
	for _, batch := range p.batches {
		rows += batch.rowNum
	}
	return rows
}

// ack records that everything through checkpoint is now persisted.
func (p *growingSourceProgress) ack(checkpoint *msgpb.MsgPosition) {
	flushedThroughTs := checkpoint.GetTimestamp()
	keepIdx := 0
	for keepIdx < len(p.batches) && p.batches[keepIdx].endPosition.GetTimestamp() <= flushedThroughTs {
		keepIdx++
	}
	p.batches = p.batches[keepIdx:]
	if checkpoint != nil && (flushedThroughTs > p.lastFlushedTs || p.lastFlushedPosition == nil) {
		p.lastFlushedTs = flushedThroughTs
		p.lastFlushedPosition = typeutil.Clone(checkpoint)
	}
	if p.pendingCommitted != nil && flushedThroughTs >= p.pendingCommitted.flushedThroughTs {
		p.pendingCommitted = nil
	}
	p.syncing = false
	p.claimedRows = 0
	p.failureCount = 0
	p.lastFailure = ""
}

func (p *growingSourceProgress) failSync(err error) {
	p.syncing = false
	p.claimedRows = 0
	p.failureCount++
	if err != nil {
		p.lastFailure = err.Error()
	}
}

// ackGrowingSyncLocked, failGrowingSyncLocked and cancelGrowingSyncLocked are the
// ONLY ways a progress may leave the in-flight state. They exist so the wake-up
// cannot be forgotten: waitSyncsSettled blocks on growingSettled rather than
// polling, so a caller that cleared `syncing` without broadcasting would hang
// every shutdown until the grace expired.
func (wb *writeBufferBase) ackGrowingSyncLocked(progress *growingSourceProgress, checkpoint *msgpb.MsgPosition) {
	progress.ack(checkpoint)
	wb.notifyGrowingSettledLocked()
}

func (wb *writeBufferBase) failGrowingSyncLocked(progress *growingSourceProgress, err error) {
	progress.failSync(err)
	wb.notifyGrowingSettledLocked()
}

func (wb *writeBufferBase) cancelGrowingSyncLocked(progress *growingSourceProgress) {
	progress.syncing = false
	progress.claimedRows = 0
	wb.notifyGrowingSettledLocked()
}

func (wb *writeBufferBase) notifyGrowingSettledLocked() {
	close(wb.growingSettled)
	wb.growingSettled = make(chan struct{})
}

// anyGrowingSyncingLocked reports whether any growing-source flush is in flight.
func (wb *writeBufferBase) anyGrowingSyncingLocked() bool {
	for _, progress := range wb.growingSourceProgress {
		if progress.syncing {
			return true
		}
	}
	return false
}

func (p *growingSourceProgress) markNonRetryableFailure() {
	p.nonRetryableFailure = true
}

func cloneBM25StatsMap(stats map[int64]*storage.BM25Stats) map[int64]*storage.BM25Stats {
	if len(stats) == 0 {
		return nil
	}
	cloned := make(map[int64]*storage.BM25Stats, len(stats))
	for fieldID, stat := range stats {
		if stat != nil {
			cloned[fieldID] = stat.Clone()
		}
	}
	return cloned
}
