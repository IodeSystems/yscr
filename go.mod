module github.com/iodesystems/yscr

go 1.26.2

// Local dev against the sibling checkout until agentkit is a resolvable
// (public) module fetch. Drop once agentkit is go-gettable.
replace github.com/iodesystems/agentkit => ../agentkit

require (
	github.com/SherClockHolmes/webpush-go v1.4.0
	github.com/iodesystems/agentkit v0.0.0-00010101000000-000000000000
	github.com/jackc/pgx/v5 v5.10.0
)

require (
	github.com/golang-jwt/jwt/v5 v5.2.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/crypto v0.31.0 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/text v0.29.0 // indirect
)
