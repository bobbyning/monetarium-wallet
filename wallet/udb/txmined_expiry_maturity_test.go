// Copyright (c) 2026 The Monetarium developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package udb

import (
	"context"
	"testing"
	"time"

	"github.com/monetarium/monetarium-node/chaincfg/chainhash"
	"github.com/monetarium/monetarium-node/cointype"
	"github.com/monetarium/monetarium-node/dcrutil"
	"github.com/monetarium/monetarium-node/txscript/stdaddr"
	"github.com/monetarium/monetarium-node/wire"
	"github.com/monetarium/monetarium-wallet/wallet/walletdb"
)

// TestExpiryCreditMaturityInCoinSelection checks that coin selection and the
// per-coin-type balance treat a credit of a transaction with an expiry as
// requiring coinbase maturity, as mond does: it rejects a transaction that
// spends such an output before CoinbaseMaturity (ErrExpiryTxSpentEarly).
// The full balance scan already applies the rule; this covers the input
// source behind sendtoaddress and the balance behind getbalance with a coin
// type, which did not read the expiry flag.
func TestExpiryCreditMaturityInCoinSelection(t *testing.T) {
	ctx := context.Background()
	db, _, s, teardown, err := cloneDB(ctx, "expiry_maturity_selection.kv")
	if err != nil {
		t.Fatal(err)
	}
	defer teardown()

	addr, err := stdaddr.NewAddressPubKeyHashEcdsaSecp256k1V0(make([]byte, 20), s.chainParams)
	if err != nil {
		t.Fatal(err)
	}
	_, pkScript := addr.PaymentScript()
	tx := wire.NewMsgTx()
	tx.Expiry = 100
	tx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(&chainhash.Hash{0x01}, 0, wire.TxTreeRegular), 0, nil))
	tx.AddTxOut(wire.NewTxOut(1e8, pkScript))
	rec, err := NewTxRecordFromMsgTx(tx, time.Now())
	if err != nil {
		t.Fatal(err)
	}

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
		src := s.MakeInputSourceWithCoinType(dbtx, 0, 1, syncHeight, nil, cointype.CoinTypeVAR)
		detail, err := src.SelectInputs(0, cointype.Zero())
		if err != nil {
			t.Fatal(err)
		}
		return len(detail.Inputs)
	}

	// Immature: the credit is mined at height 1 with one confirmation, but
	// the expiry makes it spendable only after coinbase maturity. A
	// transaction that selected it would be rejected by mond.
	err = walletdb.View(ctx, db, func(dbtx walletdb.ReadTx) error {
		if n := selectedInputs(dbtx, 1); n != 0 {
			t.Errorf("immature expiry credit: selected %d inputs, want 0", n)
		}
		bal, err := s.AccountBalanceByCoinType(dbtx, 1, 0, cointype.CoinTypeVAR)
		if err != nil {
			return err
		}
		if bal.Spendable != 0 {
			t.Errorf("immature expiry credit: spendable %v, want 0", bal.Spendable)
		}
		if bal.ImmatureCoinbaseRewards != dcrutil.Amount(1e8) {
			t.Errorf("immature expiry credit: immature %v, want 1e8 atoms",
				bal.ImmatureCoinbaseRewards)
		}
		return nil
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
		return insertMainChainHeaders(s, dbtx, makeHeaderDataSlice(blocks...), emptyFilters(int(nBlocks)))
	})
	if err != nil {
		t.Fatal(err)
	}

	// Matured: the same credit is selected and reported spendable.
	err = walletdb.View(ctx, db, func(dbtx walletdb.ReadTx) error {
		if n := selectedInputs(dbtx, tipHeight); n != 1 {
			t.Errorf("matured expiry credit: selected %d inputs, want 1", n)
		}
		bal, err := s.AccountBalanceByCoinType(dbtx, 1, 0, cointype.CoinTypeVAR)
		if err != nil {
			return err
		}
		if bal.Spendable != dcrutil.Amount(1e8) {
			t.Errorf("matured expiry credit: spendable %v, want 1e8 atoms", bal.Spendable)
		}
		if bal.ImmatureCoinbaseRewards != 0 {
			t.Errorf("matured expiry credit: immature %v, want 0", bal.ImmatureCoinbaseRewards)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
