package chaintype

// Registered chain type names. internal/config re-exports them as typed
// ChainType constants; everything else refers to those.
const (
	EVM     = "evm"
	Solana  = "solana"
	Tron    = "tron"
	Sui     = "sui"
	Soroban = "soroban"
	Horizon = "horizon"
	TON     = "ton"
	Stacks  = "stacks"
)
