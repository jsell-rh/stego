package fill

import "github.com/jsell-rh/stego/internal/types"

// FillScaffolded tells that a new fill and its test were written for one slot
// and collection (ST-EXT-EV01). It is delivered in process.
type FillScaffolded struct {
	FillName   string
	Implements types.SlotRef
}

// FillTestsPassed tells that a fill's tests passed and its evidence was
// recorded (ST-EXT-EV02). It is delivered in process.
type FillTestsPassed struct {
	FillName       string
	FillDigest     string
	ContractDigest string
}

// FillSignedOff tells that a person signed a fill's evidence (ST-EXT-EV03).
// It is delivered in process.
type FillSignedOff struct {
	FillName       string
	Signer         string
	EvidenceDigest string
}
