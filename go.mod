module github.com/iodesystems/yscr

go 1.26.2

// Local dev against the sibling checkout until agentkit is a resolvable
// (public) module fetch. Drop once agentkit is go-gettable.
replace github.com/iodesystems/agentkit => ../agentkit
