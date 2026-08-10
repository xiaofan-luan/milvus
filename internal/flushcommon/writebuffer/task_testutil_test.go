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

	"github.com/milvus-io/milvus-proto/go-api/v3/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v3/msgpb"
	"github.com/milvus-io/milvus/internal/flushcommon/metacache"
	"github.com/milvus-io/milvus/internal/flushcommon/syncmgr"
	"github.com/milvus-io/milvus/pkg/v3/proto/datapb"
)

// testSyncEntryOpt adjusts the sync pack or the queue entry newTestSyncEntry
// builds, before the task wrapping the pack is created.
type testSyncEntryOpt func(*syncmgr.SyncPack, *writeBufferSyncEntry)

// newTestSyncEntry builds the standard write-buffer queue-entry fixture: a
// SyncTask whose pack names the segment and checkpoint, wrapped in an entry
// with a fresh done channel.
func newTestSyncEntry(segmentID int64, ckptTs uint64, opts ...testSyncEntryOpt) *writeBufferSyncEntry {
	pack := new(syncmgr.SyncPack).
		WithSegmentID(segmentID).
		WithCheckpoint(&msgpb.MsgPosition{Timestamp: ckptTs})
	entry := &writeBufferSyncEntry{done: make(chan struct{})}
	for _, opt := range opts {
		opt(pack, entry)
	}
	entry.task = syncmgr.NewSyncTask().WithSyncPack(pack)
	return entry
}

func entrySubmitted(_ *syncmgr.SyncPack, entry *writeBufferSyncEntry) {
	entry.submitted = true
}

func entryFailed(_ *syncmgr.SyncPack, entry *writeBufferSyncEntry) {
	entry.failed = true
}

func entryStartPosition(ts uint64) testSyncEntryOpt {
	return func(pack *syncmgr.SyncPack, _ *writeBufferSyncEntry) {
		pack.WithStartPosition(&msgpb.MsgPosition{Timestamp: ts})
	}
}

func entryBatchRows(rows int64) testSyncEntryOpt {
	return func(pack *syncmgr.SyncPack, _ *writeBufferSyncEntry) {
		pack.WithBatchRows(rows)
	}
}

// growingSegment builds the standard growing-segment fixture the write-buffer
// tests share: partition 10, growing state, empty stats.
func growingSegment(id int64, storageVersion int64) *metacache.SegmentInfo {
	return metacache.NewSegmentInfo(&datapb.SegmentInfo{
		ID:             id,
		PartitionID:    10,
		State:          commonpb.SegmentState_Growing,
		StorageVersion: storageVersion,
	}, nil, nil, metacache.NewEmptySegmentStats())
}

// runSyncTaskInline runs both phases the way the dispatcher does, for tests that
// stub the sync manager and only need the task's end-to-end effect.
func runSyncTaskInline(ctx context.Context, task syncmgr.Task) error {
	if err := task.Prepare(ctx); err != nil {
		return err
	}
	return task.Commit(ctx)
}

// anyGrowingRetryArmed replaces the old channel-wide growingSourceRetryScheduled
// flag: the clock is per segment now, so "is a growing retry pending" is a
// question about the set.
func anyGrowingRetryArmed(wb *writeBufferBase) bool {
	for _, progress := range wb.growingSourceProgress {
		if progress.intent.owes {
			return true
		}
	}
	return false
}
