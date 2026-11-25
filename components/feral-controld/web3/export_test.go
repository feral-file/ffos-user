package web3

// SelectorForTest exposes the function selector so a test can assert the
// exact calldata a URL builds without re-implementing keccak.
func SelectorForTest(signature string) []byte { return selector(signature) }
