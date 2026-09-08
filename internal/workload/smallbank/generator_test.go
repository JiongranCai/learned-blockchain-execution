package smallbank

import (
	"context"
	"math"
	"reflect"
	"testing"

	engineapi "github.com/crypto-org-chain/go-block-stm/internal/engine"
	"github.com/crypto-org-chain/go-block-stm/internal/engine/serial"
	"github.com/crypto-org-chain/go-block-stm/internal/model"
	"github.com/crypto-org-chain/go-block-stm/internal/runtime/flat"
	"github.com/crypto-org-chain/go-block-stm/internal/state/memkv"
	"github.com/crypto-org-chain/go-block-stm/internal/workload"
)

func testConfig() Config {
	c := Config{Seed: 131, Accounts: 16, InitialChecking: BalanceRange{100, 199}, InitialSavings: BalanceRange{200, 400}, BlockCount: 2, TransactionsPerBlock: 64}
	for _, kind := range []string{Balance, DepositChecking, TransactSavings, SendPayment, Amalgamate, WriteCheck, CheckFunds} {
		tx := TransactionConfig{Type: kind, Weight: 1, Access: AccessConfig{Kind: "hotspot", HotAccounts: 4, HotProbability: 0.9}, Compute: workload.ComputeConfig{MinUnits: 16, MaxUnits: 64, PrefixFraction: 0.5}}
		if kind != Balance && kind != Amalgamate {
			tx.Amount = 150
		}
		c.Mix = append(c.Mix, tx)
	}
	return c
}

func generate(t *testing.T, c Config) workload.Artifact {
	t.Helper()
	a, err := Generate(c)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func execute(t *testing.T, a workload.Artifact) []model.BlockResult {
	t.Helper()
	storage, err := memkv.FromEntries(a.InitialState)
	if err != nil {
		t.Fatal(err)
	}
	var results []model.BlockResult
	for _, block := range a.OrderedBlocks {
		r, _, err := serial.New(nil).ExecuteBlock(context.Background(), block, storage, engineapi.RunConfig{})
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, r)
	}
	return results
}

func TestBankingSemantics(t *testing.T) {
	// Balances are [checking A, savings A, checking B, savings B]. Expected
	// values are independent business outcomes, not a second IR interpreter.
	for _, tc := range []struct {
		name, kind    string
		amount        int64
		want          [4]int64
		ret           int64
		reads, writes int
		failed        bool
	}{
		{"balance", Balance, 0, [4]int64{20, 30, 7, 11}, 50, 2, 0, false},
		{"deposit", DepositChecking, 3, [4]int64{23, 30, 7, 11}, 0, 1, 1, false},
		{"withdraw savings", TransactSavings, -5, [4]int64{20, 25, 7, 11}, 0, 1, 1, false},
		{"savings rejection", TransactSavings, -31, [4]int64{20, 30, 7, 11}, 0, 1, 0, true},
		{"payment", SendPayment, 12, [4]int64{8, 30, 19, 11}, 0, 2, 2, false},
		{"payment rejection", SendPayment, 21, [4]int64{20, 30, 7, 11}, 0, 1, 0, true},
		{"merge all source funds", Amalgamate, 0, [4]int64{0, 0, 57, 11}, 0, 3, 3, false},
		{"check backed by savings", WriteCheck, 45, [4]int64{-25, 30, 7, 11}, 0, 2, 1, false},
		{"check exact total", WriteCheck, 50, [4]int64{-30, 30, 7, 11}, 0, 2, 1, false},
		{"check penalty", WriteCheck, 51, [4]int64{-32, 30, 7, 11}, 0, 2, 1, false},
		{"selective skip savings", CheckFunds, 20, [4]int64{20, 30, 7, 11}, 1, 1, 0, false},
		{"selective use savings", CheckFunds, 50, [4]int64{20, 30, 7, 11}, 1, 2, 0, false},
		{"selective insufficient", CheckFunds, 51, [4]int64{20, 30, 7, 11}, 0, 2, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := oneTransaction(tc.kind, tc.amount, [4]int64{20, 30, 7, 11})
			r := execute(t, a)[0]
			tx := r.Transactions[0]
			status := model.TxStatusSuccess
			if tc.failed {
				status = model.TxStatusFailed
			}
			if tx.Status != status || len(tx.Reads) != tc.reads || len(tx.Writes) != tc.writes {
				t.Fatalf("unexpected outcome: %+v", tx)
			}
			if tc.failed {
				if tx.ErrorCode != "insufficient_funds" {
					t.Fatal(tx.ErrorCode)
				}
			} else if value, _ := flat.DecodeInt64(tx.ReturnValue); value != tc.ret {
				t.Fatalf("return %d, want %d", value, tc.ret)
			}
			if got := balances(r.FinalState); got != tc.want {
				t.Fatalf("balances %v, want %v", got, tc.want)
			}
		})
	}
}

func oneTransaction(kind string, amount int64, initial [4]int64) workload.Artifact {
	a := workload.Artifact{}
	for i, value := range initial {
		field := "checking"
		if i%2 == 1 {
			field = "savings"
		}
		a.InitialState = append(a.InitialState, model.StateEntry{Key: accountKey(field, i/2), Value: flat.EncodeInt64(value)})
	}
	p := transactionProgram(kind, []int{0, 1}, amount, 7, 11)
	a.OrderedBlocks = []model.Block{{ID: "bank", Transactions: []model.Transaction{{ID: kind, MaxUnits: 1000, Program: model.Program{Instructions: p}}}}}
	return a
}

func balances(state []model.StateEntry) (values [4]int64) {
	for _, entry := range state {
		for i := range values {
			field := "checking"
			if i%2 == 1 {
				field = "savings"
			}
			if string(entry.Key) == string(accountKey(field, i/2)) {
				values[i], _ = flat.DecodeInt64(entry.Value)
			}
		}
	}
	return
}

func TestPaymentOverflowRollsBackDebit(t *testing.T) {
	initial := [4]int64{20, 30, math.MaxInt64, 11}
	r := execute(t, oneTransaction(SendPayment, 1, initial))[0]
	if r.Transactions[0].Status != model.TxStatusArithmeticError || len(r.Transactions[0].Writes) != 0 || balances(r.FinalState) != initial {
		t.Fatal("failed destination credit leaked source debit")
	}
}

func TestCheckFundsKeepsUnexecutedReadAndUsesCurrentState(t *testing.T) {
	a := oneTransaction(CheckFunds, 20, [4]int64{20, 30, 7, 11})
	staticReads := 0
	for _, op := range a.OrderedBlocks[0].Transactions[0].Program.Instructions {
		if op.Op == model.OpRead {
			staticReads++
		}
	}
	if staticReads != 2 {
		t.Fatal("selective program lost a candidate read")
	}
	skip := execute(t, a)[0].Transactions[0]
	// Reuse exactly the same program, changing only the runtime state.
	a.InitialState[0].Value = flat.EncodeInt64(15)
	read := execute(t, a)[0].Transactions[0]
	if len(skip.Reads) != 1 || len(read.Reads) != 2 || skip.ComputeDigest != read.ComputeDigest {
		t.Fatal("branch did not change reads while preserving CPU work")
	}
	a.InitialState[1].Value = flat.EncodeInt64(0)
	insufficient := execute(t, a)[0].Transactions[0]
	if value, _ := flat.DecodeInt64(insufficient.ReturnValue); value != 0 || insufficient.Status != model.TxStatusSuccess {
		t.Fatal("CheckFunds false was treated as a transaction failure")
	}
}

func flatten(a workload.Artifact) (txs []model.Transaction) {
	for _, b := range a.OrderedBlocks {
		txs = append(txs, b.Transactions...)
	}
	return
}

func TestStreamDeterminismCostIsolationAndReblocking(t *testing.T) {
	c := testConfig()
	base := generate(t, c)
	if !reflect.DeepEqual(base, generate(t, c)) {
		t.Fatal("same seed changed input")
	}
	want := execute(t, base)
	originalStream := flatten(base)
	c.BlockCount, c.TransactionsPerBlock = 4, 32
	reblocked := generate(t, c)
	if !reflect.DeepEqual(originalStream, flatten(reblocked)) {
		t.Fatal("block size changed the transaction stream")
	}
	got := execute(t, reblocked)
	if !reflect.DeepEqual(got[3].FinalState, want[1].FinalState) {
		t.Fatal("reblocking changed final balances")
	}
	c.BlockCount, c.TransactionsPerBlock = 2, 64
	for _, fraction := range []float64{0, 0.9, 1} {
		for i := range c.Mix {
			c.Mix[i].Compute = workload.ComputeConfig{MinUnits: 32, MaxUnits: 96, PrefixFraction: fraction}
		}
		a := generate(t, c)
		if !reflect.DeepEqual(base.InitialState, a.InitialState) || !reflect.DeepEqual(base.LogicalArrivalSchedule, a.LogicalArrivalSchedule) {
			t.Fatal("cost changed state/arrival order")
		}
		for i, tx := range flatten(a) {
			original := originalStream[i]
			for j, op := range tx.Program.Instructions {
				op.ComputeUnits = 0
				other := original.Program.Instructions[j]
				other.ComputeUnits = 0
				if !reflect.DeepEqual(op, other) {
					t.Fatal("cost changed business program")
				}
			}
		}
		results := execute(t, a)
		for b := range results {
			if !reflect.DeepEqual(results[b].FinalState, want[b].FinalState) {
				t.Fatal("cost changed balances")
			}
			for i, tx := range results[b].Transactions {
				ref := want[b].Transactions[i]
				if tx.Status != ref.Status || !reflect.DeepEqual(tx.Reads, ref.Reads) || !reflect.DeepEqual(tx.ReturnValue, ref.ReturnValue) {
					t.Fatal("cost changed business outcome")
				}
			}
		}
	}
	c.Seed++
	if reflect.DeepEqual(base.InitialState, generate(t, c).InitialState) {
		t.Fatal("seed did not affect state")
	}
}

func TestStreamCarriesBalancesAcrossBlocks(t *testing.T) {
	c := testConfig()
	c.Accounts, c.BlockCount, c.TransactionsPerBlock = 1, 2, 1
	c.InitialChecking = BalanceRange{20, 20}
	c.Mix = []TransactionConfig{{Type: DepositChecking, Amount: 3, Weight: 1}}
	r := execute(t, generate(t, c))
	if balances(r[0].FinalState)[0] != 23 || balances(r[1].FinalState)[0] != 26 {
		t.Fatal("state was reset between blocks")
	}
}

func TestInvalidBankParameters(t *testing.T) {
	for _, change := range []func(*Config){
		func(c *Config) { c.Mix = nil },
		func(c *Config) { c.InitialChecking = BalanceRange{5, 4} },
		func(c *Config) { c.Mix[0].Type = "unknown" },
		func(c *Config) { c.Mix[0].Weight = math.NaN() },
		func(c *Config) { c.Mix[1].Amount = -1 },
		func(c *Config) { c.Mix[3].Access = AccessConfig{Kind: "hotspot", HotAccounts: 1, HotProbability: 1} },
	} {
		c := testConfig()
		change(&c)
		if _, err := Generate(c); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
}
