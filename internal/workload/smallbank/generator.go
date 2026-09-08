// Package smallbank maps banking semantics to the flat runtime. See
// configs/README.md for the KV mapping and the selective CheckFunds extension.
package smallbank

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"sort"

	"github.com/crypto-org-chain/go-block-stm/internal/model"
	"github.com/crypto-org-chain/go-block-stm/internal/runtime/flat"
	"github.com/crypto-org-chain/go-block-stm/internal/workload"
)

const GeneratorVersion = "smallbank-v1"

const (
	Balance         = "balance"
	DepositChecking = "deposit_checking"
	TransactSavings = "transact_savings"
	SendPayment     = "send_payment"
	Amalgamate      = "amalgamate"
	WriteCheck      = "write_check"
	CheckFunds      = "check_funds" // Selective read-only extension.
)

type BalanceRange struct {
	Min int64 `json:"min"`
	Max int64 `json:"max"`
}

type Config struct {
	Seed                 int64               `json:"seed"`
	Accounts             int                 `json:"accounts"`
	InitialChecking      BalanceRange        `json:"initial_checking"`
	InitialSavings       BalanceRange        `json:"initial_savings"`
	BlockCount           int                 `json:"block_count"`
	TransactionsPerBlock int                 `json:"transactions_per_block"`
	Mix                  []TransactionConfig `json:"mix"`
}

// Entries may share a Type but differ in access, cost or amount. Weights are
// sampling weights, not exact per-block quotas.
type TransactionConfig struct {
	Weight  float64                `json:"weight"`
	Type    string                 `json:"type"`
	Amount  int64                  `json:"amount,omitempty"`
	Access  AccessConfig           `json:"access"`
	Compute workload.ComputeConfig `json:"compute"`
}

type AccessConfig struct {
	Kind           string  `json:"kind,omitempty"`
	HotAccounts    int     `json:"hot_accounts,omitempty"`
	HotProbability float64 `json:"hot_probability,omitempty"`
	Theta          float64 `json:"theta,omitempty"`
}

func (a AccessConfig) sampling() workload.AccessConfig {
	return workload.AccessConfig{Kind: a.Kind, HotKeys: a.HotAccounts,
		HotProbability: a.HotProbability, Theta: a.Theta}
}

func Generate(c Config) (workload.Artifact, error) {
	if err := c.validate(); err != nil {
		return workload.Artifact{}, err
	}
	descriptor, err := json.Marshal(c)
	if err != nil {
		return workload.Artifact{}, err
	}
	a := workload.Artifact{SchemaVersion: workload.ArtifactSchemaVersion,
		Generator: workload.GeneratorDescriptor{Name: "smallbank", Version: GeneratorVersion, Seed: c.Seed, Config: descriptor}}
	initialRNG := rand.New(rand.NewSource(c.Seed))
	accountRNG := rand.New(rand.NewSource(c.Seed + 1))
	costRNG := rand.New(rand.NewSource(c.Seed + 2))
	mixRNG := rand.New(rand.NewSource(c.Seed + 3))
	for account := 0; account < c.Accounts; account++ {
		for _, field := range []struct {
			name    string
			balance BalanceRange
		}{{"checking", c.InitialChecking}, {"savings", c.InitialSavings}} {
			value := field.balance.Min + initialRNG.Int63n(field.balance.Max-field.balance.Min+1)
			a.InitialState = append(a.InitialState, model.StateEntry{Key: accountKey(field.name, account), Value: flat.EncodeInt64(value)})
		}
	}
	sort.Slice(a.InitialState, func(i, j int) bool { return bytes.Compare(a.InitialState[i].Key, a.InitialState[j].Key) < 0 })
	var total float64
	samplers := make([]workload.KeySampler, len(c.Mix))
	for i, tx := range c.Mix {
		total += tx.Weight
		samplers[i] = workload.NewKeySampler(c.Accounts, tx.Access.sampling())
	}
	// Sampling continues across blocks; the executor carries committed balances
	// forward and resets initial state only for a new execution of this stream.
	global := 0
	for b := 0; b < c.BlockCount; b++ {
		block := model.Block{ID: fmt.Sprintf("block-%06d", b), Height: uint64(b)}
		for i := 0; i < c.TransactionsPerBlock; i++ {
			choice, selected := mixRNG.Float64()*total, len(c.Mix)-1
			for j, tx := range c.Mix {
				choice -= tx.Weight
				if choice < 0 {
					selected = j
					break
				}
			}
			tx := c.Mix[selected]
			accounts := samplers[selected].Keys(accountRNG, accountCount(tx.Type))
			prefix, suffix := tx.Compute.Sample(costRNG)
			program := transactionProgram(tx.Type, accounts, tx.Amount, prefix, suffix)
			id := fmt.Sprintf("tx-%06d-%s", global, tx.Type)
			var budget uint64
			for j := range program {
				program[j].ID = fmt.Sprintf("%s/op-%03d", id, j)
				budget += 1 + program[j].ComputeUnits
			}
			block.Transactions = append(block.Transactions, model.Transaction{ID: id, MaxUnits: budget, Program: model.Program{Instructions: program}})
			a.LogicalArrivalSchedule = append(a.LogicalArrivalSchedule, workload.LogicalArrival{Sequence: uint64(global), LogicalTime: uint64(global), BlockID: block.ID, TransactionID: id})
			global++
		}
		a.OrderedBlocks = append(a.OrderedBlocks, block)
	}
	return a, a.Validate()
}

func (c Config) validate() error {
	bad := func(message string) error { return fmt.Errorf("invalid smallbank workload: %s", message) }
	if c.Accounts <= 0 || c.BlockCount <= 0 || c.TransactionsPerBlock <= 0 || len(c.Mix) == 0 {
		return bad("positive account/block/transaction counts and a nonempty mix are required")
	}
	for _, r := range []BalanceRange{c.InitialChecking, c.InitialSavings} {
		if r.Min < 0 || r.Max < r.Min || r.Max-r.Min == math.MaxInt64 {
			return bad("initial balances require a nonnegative, sampleable range")
		}
	}
	var total float64
	for _, tx := range c.Mix {
		if tx.Weight <= 0 || math.IsNaN(tx.Weight) || math.IsInf(tx.Weight, 0) {
			return bad("weights must be positive and finite")
		}
		total += tx.Weight
		switch tx.Type {
		case Balance, Amalgamate:
			if tx.Amount != 0 {
				return bad("balance/amalgamate do not take an amount")
			}
		case DepositChecking, SendPayment, WriteCheck, CheckFunds:
			if tx.Amount <= 0 {
				return bad("deposit/payment/check amounts must be positive")
			}
		case TransactSavings: // Signed deposit/withdrawal.
		default:
			return bad("unknown transaction type " + tx.Type)
		}
		support, err := tx.Access.sampling().Support(c.Accounts)
		if err != nil {
			return bad(err.Error())
		}
		if support < accountCount(tx.Type) {
			return bad("not enough distinct accounts in the access distribution")
		}
		if err := tx.Compute.Validate(); err != nil {
			return bad(err.Error())
		}
	}
	if math.IsInf(total, 0) {
		return bad("total mixture weight must be finite")
	}
	return nil
}

func accountCount(kind string) int {
	if kind == SendPayment || kind == Amalgamate {
		return 2
	}
	return 1
}

func accountKey(field string, account int) []byte {
	return []byte(fmt.Sprintf("%s/%08d", field, account))
}

func transactionProgram(kind string, accounts []int, amount int64, prefix, suffix uint64) []model.Instruction {
	p := []model.Instruction{{Op: model.OpCompute, ComputeUnits: prefix}}
	read := func(field string, account int, register string) {
		p = append(p, model.Instruction{Op: model.OpRead, Key: accountKey(field, account), Register: register})
	}
	expr := func(register string, delta int64) model.Expression {
		return model.Expression{Base: model.Register(register), Delta: delta}
	}
	sum := func(left, right string) model.Expression {
		addend := model.Register(right)
		return model.Expression{Base: model.Register(left), Addend: &addend}
	}
	literal := func(value int64) model.Expression { return model.Expression{Base: model.Literal(value)} }
	assign := func(register string, value model.Expression) {
		p = append(p, model.Instruction{Op: model.OpAssign, Register: register, Expression: value})
	}
	write := func(field string, account int, value model.Expression) {
		p = append(p, model.Instruction{Op: model.OpWrite, Key: accountKey(field, account), Expression: value})
	}
	condition := func(kind model.ConditionKind, register string, value int64) model.Condition {
		return model.Condition{Kind: kind, Left: model.Register(register), Right: model.Literal(value)}
	}
	fail := func(c model.Condition) {
		p = append(p, model.Instruction{Op: model.OpFailIf, Condition: c, ErrorCode: "insufficient_funds"})
	}
	jump := func(c model.Condition) int {
		index := len(p)
		p = append(p, model.Instruction{Op: model.OpJumpIf, Condition: c})
		return index
	}
	compute := func() { p = append(p, model.Instruction{Op: model.OpCompute, ComputeUnits: suffix}) }
	result := literal(0)
	a := accounts[0]
	switch kind {
	case Balance:
		read("checking", a, "c")
		read("savings", a, "s")
		compute()
		result = sum("c", "s")
	case DepositChecking:
		read("checking", a, "c")
		compute()
		write("checking", a, expr("c", amount))
	case TransactSavings:
		read("savings", a, "s")
		compute()
		assign("next", expr("s", amount))
		fail(condition(model.ConditionLess, "next", 0))
		write("savings", a, expr("next", 0))
	case SendPayment:
		b := accounts[1]
		read("checking", a, "c")
		fail(condition(model.ConditionLess, "c", amount))
		read("checking", b, "dest")
		compute()
		write("checking", a, expr("c", -amount))
		write("checking", b, expr("dest", amount))
	case Amalgamate:
		b := accounts[1]
		read("savings", a, "s")
		read("checking", a, "c")
		read("checking", b, "dest")
		compute()
		assign("total", sum("s", "c"))
		assign("next", sum("dest", "total"))
		write("savings", a, literal(0))
		write("checking", a, literal(0))
		write("checking", b, expr("next", 0))
	case WriteCheck:
		read("checking", a, "c")
		read("savings", a, "s")
		compute()
		assign("total", sum("c", "s"))
		assign("next", expr("c", -amount))
		paid := jump(condition(model.ConditionGreaterEq, "total", amount))
		assign("next", expr("next", -1)) // One monetary unit overdraft penalty.
		p[paid].Target = len(p)
		write("checking", a, expr("next", 0))
	case CheckFunds:
		read("checking", a, "c")
		assign("ok", literal(1))
		checkingEnough := jump(condition(model.ConditionGreaterEq, "c", amount))
		read("savings", a, "s")
		assign("total", sum("c", "s"))
		assign("ok", literal(0))
		notEnough := jump(condition(model.ConditionLess, "total", amount))
		assign("ok", literal(1))
		p[checkingEnough].Target, p[notEnough].Target = len(p), len(p)
		compute() // Both paths pay the same configured CPU work.
		result = expr("ok", 0)
	}
	return append(p, model.Instruction{Op: model.OpReturn, Expression: result})
}
