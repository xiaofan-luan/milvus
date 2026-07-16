from pymilvus import DataType

success = "success"


class FMINDEX:
    # FM-index is an exact byte-level index; VARCHAR only (no JSON, unlike NGRAM).
    supported_field_types = [
        DataType.VARCHAR,
    ]

    # Build-parameter test configurations. fm_sa_sample_rate is optional
    # (defaults to 32) and, when present, must be an integer in [4, 256].
    build_params = [
        {
            "description": "no optional params (defaults applied)",
            "params": {},
            "expected": success,
        },
        {
            "description": "default sample rate",
            "params": {"fm_sa_sample_rate": 32},
            "expected": success,
        },
        {
            "description": "minimum sample rate",
            "params": {"fm_sa_sample_rate": 4},
            "expected": success,
        },
        {
            "description": "maximum sample rate",
            "params": {"fm_sa_sample_rate": 256},
            "expected": success,
        },
        {
            "description": "sample rate below minimum",
            "params": {"fm_sa_sample_rate": 3},
            "expected": {"err_code": 1100, "err_msg": "fm_sa_sample_rate for FM-index must be in"},
        },
        {
            "description": "sample rate above maximum",
            "params": {"fm_sa_sample_rate": 257},
            "expected": {"err_code": 1100, "err_msg": "fm_sa_sample_rate for FM-index must be in"},
        },
        {
            "description": "sample rate not an integer",
            "params": {"fm_sa_sample_rate": "abc"},
            "expected": {"err_code": 1100, "err_msg": "fm_sa_sample_rate for FM-index must be an integer"},
        },
    ]
