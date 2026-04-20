// Module dnsfoxv2-workers: background workers for DNSFox v2.
// DB sharing: Option A — import dnsfoxv2-api as a Go module via local replace
// directive. This avoids duplicating db.go and repository code. No circular
// dependency risk because workers only import internal/db (no HTTP handlers).
module github.com/itsBOTzilla/dnsfoxv2-workers

go 1.26

require (
	connectrpc.com/connect v1.19.1
	github.com/google/uuid v1.6.0
	github.com/itsBOTzilla/dnsfoxv2-proto v0.0.0
	github.com/jackc/pgx/v5 v5.9.2
	github.com/redis/go-redis/v9 v9.18.0
	golang.org/x/crypto v0.50.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/text v0.36.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace (
	github.com/itsBOTzilla/dnsfoxv2-api => ../dnsfoxv2-api
	github.com/itsBOTzilla/dnsfoxv2-proto => ../dnsfoxv2-proto
)
