package service

import "sync/atomic"

// Stats are cumulative counters surfaced on the resource page. They are the only
// way to tell what the routing policy actually did, so every branch of the
// selector records into exactly one of them.
type Stats struct {
	Picks         atomic.Int64
	Bound         atomic.Int64
	Assigned      atomic.Int64
	Rebound       atomic.Int64
	Pinned        atomic.Int64
	Fallback      atomic.Int64
	Unhandled     atomic.Int64
	MappingFailed atomic.Int64

	Fetches       atomic.Int64
	FetchFailures atomic.Int64
	FetchTimeouts atomic.Int64
	Revoked       atomic.Int64
	Suspected429  atomic.Int64

	ConfigSyncs      atomic.Int64
	ConfigSyncErrors atomic.Int64
}
