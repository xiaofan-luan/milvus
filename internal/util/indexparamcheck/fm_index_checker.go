package indexparamcheck

import (
	"strconv"

	"github.com/milvus-io/milvus-proto/go-api/v3/schemapb"
	"github.com/milvus-io/milvus/pkg/v3/util/merr"
	"github.com/milvus-io/milvus/pkg/v3/util/typeutil"
)

const (
	// FmSaSampleRateKey is the optional suffix-array sampling rate (space vs.
	// locate latency; no effect on Count). Default 8 when unset.
	FmSaSampleRateKey = "fm_sa_sample_rate"

	fmSaSampleRateMin = 4
	fmSaSampleRateMax = 256
)

// FMIndexChecker validates params for the FM-index scalar index, an exact
// byte-level substring index for VARCHAR that accelerates LIKE
// prefix/infix/suffix with no candidate recheck.
type FMIndexChecker struct {
	scalarIndexChecker
}

func newFMIndexChecker() *FMIndexChecker {
	return &FMIndexChecker{}
}

func (c *FMIndexChecker) CheckTrain(dataType schemapb.DataType, elementType schemapb.DataType, params map[string]string) error {
	if dataType != schemapb.DataType_VarChar {
		return merr.WrapErrParameterInvalidMsg("FM-index can only be created on VARCHAR field")
	}

	if rateStr, ok := params[FmSaSampleRateKey]; ok {
		rate, err := strconv.Atoi(rateStr)
		if err != nil {
			return merr.WrapErrParameterInvalidMsg("fm_sa_sample_rate for FM-index must be an integer, got: %s", rateStr)
		}
		if rate < fmSaSampleRateMin || rate > fmSaSampleRateMax {
			return merr.WrapErrParameterInvalidMsg("fm_sa_sample_rate for FM-index must be in [%d, %d], got: %d", fmSaSampleRateMin, fmSaSampleRateMax, rate)
		}
	}

	return c.scalarIndexChecker.CheckTrain(dataType, elementType, params)
}

func (c *FMIndexChecker) CheckValidDataType(indexType IndexType, field *schemapb.FieldSchema) error {
	if !typeutil.IsStringType(field.GetDataType()) {
		return merr.WrapErrParameterInvalidMsg("FM-index can only be created on VARCHAR field")
	}
	return nil
}
