// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package proxy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/milvus-io/milvus-proto/go-api/v3/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v3/msgpb"
	"github.com/milvus-io/milvus-proto/go-api/v3/schemapb"
	"github.com/milvus-io/milvus/pkg/v3/mq/msgstream"
	"github.com/milvus-io/milvus/pkg/v3/proto/messagespb"
	streamingmessage "github.com/milvus-io/milvus/pkg/v3/streaming/util/message"
)

// newInt64VarCharInsertMsgForRepackTest builds a column-based source request
// whose per-row values are positionally distinct.
func newInt64VarCharInsertMsgForRepackTest(collectionID int64, rows ...string) *msgstream.InsertMsg {
	namespace := "test-namespace"
	rowIDs := make([]int64, len(rows))
	timestamps := make([]uint64, len(rows))
	longValues := make([]int64, len(rows))
	for i := range rows {
		rowIDs[i] = int64(i + 1)
		timestamps[i] = uint64(10 + i)
		longValues[i] = int64(100 + i)
	}

	return &msgstream.InsertMsg{
		BaseMsg: msgstream.BaseMsg{
			Ctx:            context.Background(),
			BeginTimestamp: uint64(99),
			EndTimestamp:   uint64(99),
		},
		InsertRequest: &msgpb.InsertRequest{
			Base: &commonpb.MsgBase{
				MsgType:  commonpb.MsgType_Insert,
				SourceID: 42,
			},
			DbID:           8,
			CollectionID:   collectionID,
			DbName:         "db",
			CollectionName: "collection",
			PartitionName:  "source-partition",
			NumRows:        uint64(len(rows)),
			RowIDs:         rowIDs,
			Timestamps:     timestamps,
			FieldsData: []*schemapb.FieldData{
				{
					Type:      schemapb.DataType_Int64,
					FieldId:   100,
					FieldName: "pk",
					Field: &schemapb.FieldData_Scalars{
						Scalars: &schemapb.ScalarField{
							Data: &schemapb.ScalarField_LongData{
								LongData: &schemapb.LongArray{Data: longValues},
							},
						},
					},
				},
				{
					Type:      schemapb.DataType_VarChar,
					FieldId:   101,
					FieldName: "large_text",
					Field: &schemapb.FieldData_Scalars{
						Scalars: &schemapb.ScalarField{
							Data: &schemapb.ScalarField_StringData{
								StringData: &schemapb.StringArray{Data: rows},
							},
						},
					},
				},
			},
			Version:   msgpb.InsertDataVersion_ColumnBased,
			Namespace: &namespace,
		},
	}
}

func TestRepackInsertDataByPartitionForStreamingServiceSelectsRows(t *testing.T) {
	source := newInt64VarCharInsertMsgForRepackTest(100, "a", "bb", "ccc", "dddd")
	selection := []int{1, 3}

	msgs, err := repackInsertDataByPartitionForStreamingService(
		200,
		"target-partition",
		selection,
		"vchannel-1",
		source,
		nil,
		7,
		nil,
	)
	require.NoError(t, err)
	require.Len(t, msgs, 1)

	insert := streamingmessage.MustAsMutableInsertMessageV1(msgs[0])
	header := insert.Header()
	assert.Equal(t, int64(100), header.GetCollectionId())
	assert.Equal(t, int32(7), header.GetSchemaVersion())
	require.Len(t, header.GetPartitions(), 1)
	assert.Equal(t, int64(200), header.GetPartitions()[0].GetPartitionId())
	assert.Equal(t, uint64(len(selection)), header.GetPartitions()[0].GetRows())

	body := insert.MustBody()
	assert.Equal(t, uint64(len(selection)), body.GetNumRows())
	assert.Equal(t, int64(200), body.GetPartitionID())
	assert.Equal(t, "target-partition", body.GetPartitionName())
	assert.Equal(t, "vchannel-1", body.GetShardName())
	assert.Equal(t, "db", body.GetDbName())
	assert.Equal(t, int64(8), body.GetDbID())
	assert.Equal(t, "collection", body.GetCollectionName())
	assert.Equal(t, "test-namespace", body.GetNamespace())
	assert.Equal(t, uint64(99), body.GetBase().GetTimestamp())
	assert.Equal(t, int64(42), body.GetBase().GetSourceID())
	// The body must carry exactly the selected source rows.
	assert.Equal(t, []int64{2, 4}, body.GetRowIDs())
	assert.Equal(t, []uint64{11, 13}, body.GetTimestamps())
	assert.Equal(t, []int64{101, 103}, body.GetFieldsData()[0].GetScalars().GetLongData().GetData())
	assert.Equal(t, []string{"bb", "dddd"}, body.GetFieldsData()[1].GetScalars().GetStringData().GetData())

	// Repacking must not mutate or compact the source request.
	assert.Equal(t, []int64{1, 2, 3, 4}, source.GetRowIDs())
	assert.Equal(t, []string{"a", "bb", "ccc", "dddd"}, source.GetFieldsData()[1].GetScalars().GetStringData().GetData())
}

func TestRepackInsertDataByPartitionForStreamingServiceCarriesPartialUpdateCASInBody(t *testing.T) {
	source := newInt64VarCharInsertMsgForRepackTest(100, "a", "bb")
	meta := &messagespb.PartialUpdateCAS{
		ReadTs:               100,
		ObservedPchannelTerm: 1,
	}

	msgs, err := repackInsertDataByPartitionForStreamingService(
		200,
		"target-partition",
		[]int{0, 1},
		"vchannel-1",
		source,
		nil,
		7,
		meta,
	)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.True(t, streamingmessage.HasPartialUpdateCAS(msgs[0]))
	extracted, err := streamingmessage.ExtractPartialUpdateCAS(msgs[0])
	require.NoError(t, err)
	require.True(t, proto.Equal(meta, extracted))

	bodyProperties := streamingmessage.MustAsMutableInsertMessageV1(msgs[0]).MustBody().GetBase().GetProperties()
	require.NotEmpty(t, bodyProperties)
}

func TestRepackInsertDataByPartitionForStreamingServiceSelectsCompactNullableVector(t *testing.T) {
	source := newInt64VarCharInsertMsgForRepackTest(100, "a", "b", "c", "d")
	source.FieldsData = []*schemapb.FieldData{
		{
			Type:    schemapb.DataType_FloatVector,
			FieldId: 102,
			Field: &schemapb.FieldData_Vectors{
				Vectors: &schemapb.VectorField{
					Dim:       2,
					ValidData: []bool{true, false, true, false},
					Data: &schemapb.VectorField_FloatVector{
						FloatVector: &schemapb.FloatArray{Data: []float32{1, 2, 3, 4}},
					},
				},
			},
		},
	}

	msgs, err := repackInsertDataByPartitionForStreamingService(
		200,
		"target-partition",
		[]int{1, 2},
		"vchannel-1",
		source,
		nil,
		7,
		nil,
	)
	require.NoError(t, err)
	require.Len(t, msgs, 1)

	field := streamingmessage.MustAsMutableInsertMessageV1(msgs[0]).MustBody().GetFieldsData()[0]
	assert.Equal(t, []bool{false, true}, field.GetVectors().GetValidData())
	assert.Equal(t, []float32{3, 4}, field.GetVectors().GetFloatVector().GetData())
}

func TestRepackInsertDataByPartitionForStreamingServiceEmptySelection(t *testing.T) {
	source := newInt64VarCharInsertMsgForRepackTest(100, "a")

	msgs, err := repackInsertDataByPartitionForStreamingService(
		200,
		"target-partition",
		nil,
		"vchannel-1",
		source,
		nil,
		7,
		nil,
	)
	require.NoError(t, err)
	require.Empty(t, msgs)
}
