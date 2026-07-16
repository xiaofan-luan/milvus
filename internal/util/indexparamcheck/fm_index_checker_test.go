package indexparamcheck

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/milvus-io/milvus-proto/go-api/v3/schemapb"
)

func Test_FMIndexChecker_CheckTrain(t *testing.T) {
	checker := newFMIndexChecker()

	t.Run("valid fmindex on varchar without sample rate", func(t *testing.T) {
		params := map[string]string{}
		err := checker.CheckTrain(schemapb.DataType_VarChar, schemapb.DataType_None, params)
		assert.NoError(t, err)
	})

	t.Run("valid fmindex on varchar with sample rate", func(t *testing.T) {
		for _, rate := range []string{"4", "32", "256"} {
			params := map[string]string{FmSaSampleRateKey: rate}
			err := checker.CheckTrain(schemapb.DataType_VarChar, schemapb.DataType_None, params)
			assert.NoError(t, err)
		}
	})

	t.Run("invalid fmindex on non-varchar field", func(t *testing.T) {
		for _, dtype := range []schemapb.DataType{
			schemapb.DataType_Int64,
			schemapb.DataType_JSON,
			schemapb.DataType_Bool,
		} {
			params := map[string]string{}
			err := checker.CheckTrain(dtype, schemapb.DataType_None, params)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "FM-index can only be created on VARCHAR field")
		}
	})

	t.Run("invalid sample rate", func(t *testing.T) {
		testCases := []struct {
			name   string
			rate   string
			errMsg string
		}{
			{"non-integer", "abc", "fm_sa_sample_rate for FM-index must be an integer"},
			{"below min", "3", "fm_sa_sample_rate for FM-index must be in"},
			{"above max", "257", "fm_sa_sample_rate for FM-index must be in"},
		}
		for _, tc := range testCases {
			params := map[string]string{FmSaSampleRateKey: tc.rate}
			err := checker.CheckTrain(schemapb.DataType_VarChar, schemapb.DataType_None, params)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tc.errMsg)
		}
	})
}

func Test_FMIndexChecker_CheckValidDataType(t *testing.T) {
	checker := newFMIndexChecker()

	t.Run("valid data type", func(t *testing.T) {
		field := &schemapb.FieldSchema{DataType: schemapb.DataType_VarChar}
		err := checker.CheckValidDataType(IndexFMINDEX, field)
		assert.NoError(t, err)
	})

	t.Run("invalid data types", func(t *testing.T) {
		invalidTypes := []schemapb.DataType{
			schemapb.DataType_Int64,
			schemapb.DataType_Float,
			schemapb.DataType_Bool,
			schemapb.DataType_Array,
			schemapb.DataType_JSON,
			schemapb.DataType_FloatVector,
		}
		for _, dtype := range invalidTypes {
			field := &schemapb.FieldSchema{DataType: dtype}
			err := checker.CheckValidDataType(IndexFMINDEX, field)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "FM-index can only be created on VARCHAR field")
		}
	})
}
