// Copyright (c) 2026 The Monetarium developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package udb

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/monetarium/monetarium-node/chaincfg"
	"github.com/monetarium/monetarium-node/chaincfg/chainhash"
	"github.com/monetarium/monetarium-node/cointype"
	"github.com/monetarium/monetarium-node/dcrutil"
	"github.com/monetarium/monetarium-node/txscript/stdaddr"
	"github.com/monetarium/monetarium-node/wire"
	"github.com/monetarium/monetarium-wallet/wallet/walletdb"
)

// expiryMaturityCase is one coin type of the expiry-maturity tests.  SKA1
// credits sit in their own bucket and store an int64 amount of 0, so the VAR
// balance fields stay empty for them and the SKA fields are the ones that
// matter; both are asserted for both coin types.
type expiryMaturityCase struct {
	coinType cointype.CoinType
}

var expiryMaturityCases = []expiryMaturityCase{
	{coinType: cointype.CoinTypeVAR},
	{coinType: cointype.CoinType(1)},
}

func expiryTxRecord(t *testing.T, ct cointype.CoinType, params *chaincfg.Params) (*wire.MsgTx, *TxRecord) {
	t.Helper()
	addr, err := stdaddr.NewAddressPubKeyHashEcdsaSecp256k1V0(make([]byte, 20),
		params)
	if err != nil {
		t.Fatal(err)
	}
	_, pkScript := addr.PaymentScript()
	tx := wire.NewMsgTx()
	tx.Expiry = 100
	tx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(&chainhash.Hash{0x01}, 0,
		wire.TxTreeRegular), 0, nil))
	if ct.IsSKA() {
		tx.AddTxOut(wire.NewTxOutSKA(big.NewInt(1e8), ct, pkScript))
	} else {
		tx.AddTxOut(wire.NewTxOut(1e8, pkScript))
	}
	rec, err := NewTxRecordFromMsgTx(tx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return tx, rec
}

// TestExpiryCreditMaturityInCoinSelection checks that coin selection and the
// per-coin-type balance treat a credit of a transaction with an expiry as
// requiring coinbase maturity, as mond does: it rejects a transaction that
// spends such an output before CoinbaseMaturity (ErrExpiryTxSpentEarly).
// The full balance scan already applies the rule; this covers the input
// source behind sendtoaddress and the balance behind getbalance with a coin
// type, which did not read the expiry flag.
func TestExpiryCreditMaturityInCoinSelection(t *testing.T) {
	for _, tc := range expiryMaturityCases {
		t.Run(tc.coinType.String(), func(t *testing.T) {
			testExpiryCreditMaturityInCoinSelection(t, tc.coinType)
		})
	}
}

func testExpiryCreditMaturityInCoinSelection(t *testing.T, ct cointype.CoinType) {
	ctx := context.Background()
	db, _, s, teardown, err := cloneDB(ctx, "expiry_maturity_selection_"+ct.String()+".kv")
	if err != nil {
		t.Fatal(err)
	}
	defer teardown()

	_, rec := expiryTxRecord(t, ct, s.chainParams)

	g := makeBlockGenerator()
	b1 := g.generate(dcrutil.BlockValid)
	b1Hash := b1.BlockHash()
	err = walletdb.Update(ctx, db, func(dbtx walletdb.ReadWriteTx) error {
		if err := s.InsertMemPoolTx(dbtx, rec); err != nil {
			return err
		}
		if err := s.AddCredit(dbtx, rec, nil, 0, false, 0); err != nil {
			return err
		}
		err := insertMainChainHeaders(s, dbtx, makeHeaderDataSlice(b1), emptyFilters(1))
		if err != nil {
			return err
		}
		return s.InsertMinedTx(dbtx, rec, &b1Hash)
	})
	if err != nil {
		t.Fatal(err)
	}

	selectedInputs := func(dbtx walletdb.ReadTx, syncHeight int32) int {
		src := s.MakeInputSourceWithCoinType(dbtx, 0, 1, syncHeight, nil, ct)
		detail, err := src.SelectInputs(0, cointype.Zero())
		if err != nil {
			t.Fatal(err)
		}
		return len(detail.Inputs)
	}
	checkBalances := func(stage string, immature bool) func(walletdb.ReadTx) error {
		return func(dbtx walletdb.ReadTx) error {
			bal, err := s.AccountBalanceByCoinType(dbtx, 1, 0, ct)
			if err != nil {
				return err
			}
			// VAR credits carry their amount in the int64 fields, SKA
			// credits in the big.Int fields and 0 in the int64 fields;
			// assert exactly the pair that belongs to the coin type.
			var wantSpend, wantImmature dcrutil.Amount
			var wantSKASpend, wantSKAImmature = cointype.Zero(), cointype.Zero()
			if ct.IsSKA() {
				if !immature {
					wantSKASpend = cointype.SKAAmountFromInt64(1e8)
				} else {
					wantSKAImmature = cointype.SKAAmountFromInt64(1e8)
				}
			} else {
				if !immature {
					wantSpend = dcrutil.Amount(1e8)
				} else {
					wantImmature = dcrutil.Amount(1e8)
				}
			}
			if bal.Spendable != wantSpend {
				t.Errorf("%s expiry credit (%s): spendable %v, want %v",
					stage, ct, bal.Spendable, wantSpend)
			}
			if bal.ImmatureCoinbaseRewards != wantImmature {
				t.Errorf("%s expiry credit (%s): immature %v, want %v",
					stage, ct, bal.ImmatureCoinbaseRewards, wantImmature)
			}
			if bal.SKASpendable.Cmp(wantSKASpend) != 0 {
				t.Errorf("%s expiry credit (%s): SKA spendable %v, want %v",
					stage, ct, bal.SKASpendable, wantSKASpend)
			}
			if bal.SKAImmatureCoinbaseRewards.Cmp(wantSKAImmature) != 0 {
				t.Errorf("%s expiry credit (%s): SKA immature %v, want %v",
					stage, ct, bal.SKAImmatureCoinbaseRewards, wantSKAImmature)
			}
			return nil
		}
	}

	// Immature: the credit is mined at height 1 with one confirmation, but
	// the expiry makes it spendable only after coinbase maturity. A
	// transaction that selected it would be rejected by mond.
	err = walletdb.View(ctx, db, func(dbtx walletdb.ReadTx) error {
		if n := selectedInputs(dbtx, 1); n != 0 {
			t.Errorf("immature expiry credit (%s): selected %d inputs, want 0",
				ct, n)
		}
		return checkBalances("immature", true)(dbtx)
	})
	if err != nil {
		t.Fatal(err)
	}

	// Extend the main chain past coinbase maturity.
	maturity := int32(s.chainParams.CoinbaseMaturity)
	tipHeight := 1 + maturity + 5
	nBlocks := tipHeight - 1
	blocks := make([]*wire.BlockHeader, 0, nBlocks)
	for i := int32(0); i < nBlocks; i++ {
		blocks = append(blocks, g.generate(dcrutil.BlockValid))
	}
	err = walletdb.Update(ctx, db, func(dbtx walletdb.ReadWriteTx) error {
		return insertMainChainHeaders(s, dbtx, makeHeaderDataSlice(blocks...),
			emptyFilters(int(nBlocks)))
	})
	if err != nil {
		t.Fatal(err)
	}

	// Matured: the same credit is selected and reported spendable.
	err = walletdb.View(ctx, db, func(dbtx walletdb.ReadTx) error {
		if n := selectedInputs(dbtx, tipHeight); n != 1 {
			t.Errorf("matured expiry credit (%s): selected %d inputs, want 1", ct, n)
		}
		return checkBalances("matured", false)(dbtx)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestUnminedExpiryCreditNotSelectable checks that an unmined credit of a
// transaction with an expiry is returned by no selection path: while the
// parent transaction is unmined, the mempool runs CheckTransactionInputs at
// nextBlockHeight with the parent at mining.UnminedHeight, so blocksSincePrev
// is negative and a spending transaction fails with ErrExpiryTxSpentEarly.
// With minConf 0 the input source was the one path that still selected such
// credits, and the minConf-0 balance counted them spendable.
func TestUnminedExpiryCreditNotSelectable(t *testing.T) {
	for _, tc := range expiryMaturityCases {
		t.Run(tc.coinType.String(), func(t *testing.T) {
			testUnminedExpiryCreditNotSelectable(t, tc.coinType)
		})
	}
}

func testUnminedExpiryCreditNotSelectable(t *testing.T, ct cointype.CoinType) {
	ctx := context.Background()
	db, _, s, teardown, err := cloneDB(ctx, "unmined_expiry_selection_"+ct.String()+".kv")
	if err != nil {
		t.Fatal(err)
	}
	defer teardown()

	_, rec := expiryTxRecord(t, ct, s.chainParams)

	err = walletdb.Update(ctx, db, func(dbtx walletdb.ReadWriteTx) error {
		if err := s.InsertMemPoolTx(dbtx, rec); err != nil {
			return err
		}
		return s.AddCredit(dbtx, rec, nil, 0, false, 0)
	})
	if err != nil {
		t.Fatal(err)
	}

	err = walletdb.View(ctx, db, func(dbtx walletdb.ReadTx) error {
		_, syncHeight := s.MainChainTip(dbtx)
		src := s.MakeInputSourceWithCoinType(dbtx, 0, 0, syncHeight, nil, ct)
		detail, err := src.SelectInputs(0, cointype.Zero())
		if err != nil {
			return err
		}
		if len(detail.Inputs) != 0 {
			t.Errorf("unmined expiry credit (%s): input source selected %d inputs "+
				"at minconf 0, want 0", ct, len(detail.Inputs))
		}
		bal, err := s.AccountBalanceByCoinType(dbtx, 0, 0, ct)
		if err != nil {
			return err
		}
		if bal.Spendable != 0 {
			t.Errorf("unmined expiry credit (%s): spendable %v at minconf 0, want 0",
				ct, bal.Spendable)
		}
		if bal.SKASpendable.Cmp(cointype.Zero()) != 0 {
			t.Errorf("unmined expiry credit (%s): SKA spendable %v at minconf 0, "+
				"want 0", ct, bal.SKASpendable)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
