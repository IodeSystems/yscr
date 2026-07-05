module github.com/iodesystems/yscr

go 1.26.2

// Local dev against the sibling checkout until agentkit is a resolvable
// (public) module fetch. Drop once agentkit is go-gettable.
replace github.com/iodesystems/agentkit => ../agentkit

require github.com/iodesystems/agentkit v0.0.0-00010101000000-000000000000

require github.com/google/uuid v1.6.0 // indirect
