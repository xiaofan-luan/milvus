package types

const (
	// StreamingVersionChunking marks that every StreamingNode in the cluster can
	// read chunked WAL records, so writers may start creating them.
	StreamingVersionChunking int64 = 4
)
