package types

import (
	"fmt"
	"strconv"

	"gopkg.in/yaml.v3"
)

// The kinds of an entity (ST-DECL-EN02). Omitted means data.
const (
	EntityKindData   = "data"
	EntityKindLedger = "ledger"
)

// The operations that only a collection over a ledger serves (ST-DECL-INV27): post posts a transfer, a pending one
// or a new one; void voids a pending transfer; reverse makes the new transfer that corrects a posted one.
const (
	OpPost    Operation = "post"
	OpVoid    Operation = "void"
	OpReverse Operation = "reverse"
)

// LedgerOperations is the set of the operations that a collection over a ledger may name (ST-DECL-INV27), in the
// order the declaration format lists them.
var LedgerOperations = []Operation{OpCreate, OpPost, OpVoid, OpReverse, OpRead, OpList}

// Ledger is the ledger settings of an entity of kind ledger (ST-DECL-EN14): its one currency, its transfer codes,
// its two-phase rule, the pending timeout, the snapshot interval and its accounts. The format holds no stored
// balance: a balance is derived from the entries after the latest snapshot (ST-DECL-INV27).
type Ledger struct {
	// Currency is the ISO 4217 code of the ledger. The format reads one code or a list, so that checking can name
	// each code of a ledger that gives more than one (ST-DECL-INV26).
	Currency Currencies `yaml:"currency,omitempty" json:"Currency,omitempty"`
	// TransferCodes are the codes of the transfers that the ledger accepts.
	TransferCodes []string `yaml:"transfer_codes,omitempty" json:"TransferCodes,omitempty"`
	// TwoPhase tells whether a transfer may be pended and then posted or voided. It has no default.
	TwoPhase *bool `yaml:"two_phase,omitempty" json:"TwoPhase,omitempty"`
	// PendingTimeout is how long a pending transfer may stay pending, as a Go duration such as 15m.
	PendingTimeout string `yaml:"pending_timeout,omitempty" json:"PendingTimeout,omitempty"`
	// SnapshotEvery is the number of entries per account between two balance snapshots.
	SnapshotEvery *int64 `yaml:"snapshot_every,omitempty" json:"SnapshotEvery,omitempty"`
	// Accounts are the account codes of the ledger (ST-DECL-EN15).
	Accounts []LedgerAccount `yaml:"accounts,omitempty" json:"Accounts,omitempty"`
}

// LedgerAccount is one account of a ledger (ST-DECL-EN15): its normal balance, its lowest balance, its owner and
// whether it is hot. It has no balance field, so no declaration and no slot can state a stored balance.
type LedgerAccount struct {
	// Name is the account code, unique in its ledger, such as customer_wallet.
	Name string `yaml:"name" json:"Name"`
	// Normal is debit or credit: the side on which the balance grows.
	Normal string `yaml:"normal,omitempty" json:"Normal,omitempty"`
	// MinBalance is the lowest balance the account may reach, as Money in the currency of the ledger.
	MinBalance *Money `yaml:"min_balance,omitempty" json:"MinBalance,omitempty"`
	// Owner names the entity that owns an account of this code, when one does.
	Owner string `yaml:"owner,omitempty" json:"Owner,omitempty"`
	// Hot marks an account that is never row-locked, such as a settlement account.
	Hot bool `yaml:"hot,omitempty" json:"Hot,omitempty"`
}

// Money is an amount as the declaration writes it (ST-VO61): integer minor units with an ISO 4217 currency.
type Money struct {
	Amount   MinorUnits `yaml:"amount" json:"Amount"`
	Currency string     `yaml:"currency,omitempty" json:"Currency,omitempty"`
}

// MinorUnits is an amount of minor units as the declaration writes it. It keeps the text, so that checking can
// refuse a value that is not a whole number, such as 10.5, and name it, instead of a reading fault that names no
// account (ST-DECL-INV26).
type MinorUnits struct {
	// Text is the value as written.
	Text string `json:"Text"`
	// Whole is true when Text is a whole number that fits in an int64.
	Whole bool `json:"Whole"`
	// Value is the whole number, when Whole is true.
	Value int64 `json:"Value"`
}

// UnmarshalYAML reads one scalar. A YAML integer that fits in an int64 is whole; any other scalar is kept as text.
func (m *MinorUnits) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: an amount of minor units must be one whole number", node.Line)
	}
	*m = MinorUnits{Text: node.Value}
	if node.Tag == "!!int" {
		if v, err := strconv.ParseInt(node.Value, 10, 64); err == nil {
			m.Whole, m.Value = true, v
		}
	}
	return nil
}

// MarshalYAML writes a whole amount as a number and any other amount as the text that was read.
func (m MinorUnits) MarshalYAML() (any, error) {
	if m.Whole {
		return m.Value, nil
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Value: m.Text}, nil
}

// Currencies are the currency codes that a ledger gives: one code as a scalar, or a list.
type Currencies []string

// UnmarshalYAML reads one code or a list of codes.
func (c *Currencies) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		*c = Currencies{node.Value}
		return nil
	case yaml.SequenceNode:
		var codes []string
		if err := node.Decode(&codes); err != nil {
			return err
		}
		*c = codes
		return nil
	}
	return fmt.Errorf("line %d: a ledger currency must be one ISO 4217 code", node.Line)
}

// MarshalYAML writes one code as a scalar and more than one as a list.
func (c Currencies) MarshalYAML() (any, error) {
	if len(c) == 1 {
		return c[0], nil
	}
	return []string(c), nil
}
