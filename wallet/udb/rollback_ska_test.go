// Copyright (c) 2026 The Monetarium developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package udb

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/monetarium/monetarium-node/chaincfg/chainhash"
	"github.com/monetarium/monetarium-node/cointype"
	"github.com/monetarium/monetarium-node/dcrutil"
	"github.com/monetarium/monetarium-node/txscript/stdaddr"
	"github.com/monetarium/monetarium-node/wire"
	"github.com/monetarium/monetarium-wallet/wallet/walletdb"
)

// TestRollbackRestoresSpentSKACredit checks that rolling back a block that
// holds a wallet SKA spend puts the spent SKA credit back into the unspent
// set. The SKA companion of TestPruneUnminedRemovesDetachedSSFee: an SKA
// credit stores 0 in its credit value, so the amount-zero shortcut took
// every SKA credit as already removed, and the spent credit was never put
// back into the unspent set.
//
// Since c41bb4bb, main keeps the rolled-back spend itself: hasValidInputs
// counts an input as double-spent only when its credit carries the spent
// flag, and unspendRawCredit clears that flag for SKA credits too. The
// damage that remains is the credit missing from the unspent set:
//   - when the spend is mined again, existsUnspent finds no entry for the
//     credit, so no debit is recorded and the credit is never marked spent;
//   - when the spend instead leaves the wallet unmined (for example it is
//     abandoned), the credit never comes back, and its SKA amount drops out
//     of the balance and coin selection for good.
//
// The chain before the rollback:
//   - b1: skaFund pays the wallet a 9 SKA credit;
//   - b2: skaSpend spends it and pays an 8.5 SKA change to the wallet.
//
// After the rollback to keep only b1, skaSpend is unmined and spends
// skaFund:0. The unspent set then holds the fund credit, held by the
// unmined spend, next to the spend's change. Removing the unmined spend
// afterwards (for example, abandoning it) leaves the fund credit as the
// only unspent output, with its SKA amount and coin type intact.
func TestRollbackRestoresSpentSKACredit(t *testing.T) {
	ctx := context.Background()
	db, _, s, teardown, err := cloneDB(ctx, "rollback_ska_credit.kv")
	if err != nil {
		t.Fatal(err)
	}
	defer teardown()

	skaType := cointype.CoinType(1)
	addr, err := stdaddr.NewAddressPubKeyHashEcdsaSecp256k1V0(make([]byte, 20), s.chainParams)
	if err != nil {
		t.Fatal(err)
	}
	_, p2pkh := addr.PaymentScript()
	skaOut := func(v int64) *wire.TxOut {
		return wire.NewTxOutSKA(big.NewInt(v), skaType, p2pkh)
	}
	newRec := func(ins []wire.OutPoint, outs ...*wire.TxOut) *TxRecord {
		tx := wire.NewMsgTx()
		for i := range ins {
			tx.AddTxIn(wire.NewTxIn(&ins[i], 0, nil))
		}
		for _, out := range outs {
			tx.AddTxOut(out)
		}
		rec, err := NewTxRecordFromMsgTx(tx, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	op := func(rec *TxRecord, index uint32) wire.OutPoint {
		return wire.OutPoint{Hash: rec.Hash, Index: index, Tree: wire.TxTreeRegular}
	}

	skaFund := newRec([]wire.OutPoint{{Hash: chainhash.Hash{0x01}}}, skaOut(9e8))
	skaSpend := newRec([]wire.OutPoint{op(skaFund, 0)}, skaOut(85e7))

	g := makeBlockGenerator()
	headers := []*wire.BlockHeader{g.generate(dcrutil.BlockValid), g.generate(dcrutil.BlockValid)}

	var pruned []*chainhash.Hash
	err = walletdb.Update(ctx, db, func(dbtx walletdb.ReadWriteTx) error {
		err := insertMainChainHeaders(s, dbtx, makeHeaderDataSlice(headers...), emptyFilters(2))
		if err != nil {
			return err
		}
		mine := func(b *wire.BlockHeader, rec *TxRecord, credit uint32) error {
			h := b.BlockHash()
			if err := s.InsertMinedTx(dbtx, rec, &h); err != nil {
				return err
			}
			return s.AddCredit(dbtx, rec, makeBlockMeta(b), credit, false, 0)
		}
		if err := mine(headers[0], skaFund, 0); err != nil {
			return err
		}
		if err := mine(headers[1], skaSpend, 0); err != nil {
			return err
		}
		// A longer chain without b2 arrives: drop b2. skaSpend becomes
		// unmined, and the credit it spent must return to the unspent
		// set so PruneUnmined keeps seeing the spend's input.
		if err := s.Rollback(dbtx, 2); err != nil {
			return err
		}
		pruned, err = s.PruneUnmined(dbtx, 0, true)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, h := range pruned {
		if *h == skaSpend.Hash {
			t.Errorf("%v pruned as orphaned, want it kept unmined", &skaSpend.Hash)
		}
	}

	err = walletdb.View(ctx, db, func(dbtx walletdb.ReadTx) error {
		ns := dbtx.ReadBucket(wtxmgrBucketKey)
		if existsRawUnmined(ns, skaSpend.Hash[:]) == nil {
			t.Errorf("%v is no longer unmined", &skaSpend.Hash)
		}
		utxos, err := s.UnspentOutputs(dbtx, skaType)
		if err != nil {
			return err
		}
		if len(utxos) != 1 {
			t.Errorf("%d unspent outputs, want 1 (the unmined change)", len(utxos))
			return nil
		}
		if utxos[0].Hash != skaSpend.Hash || utxos[0].Index != 0 {
			t.Errorf("unspent output %v:%d, want the change of %v:0",
				&utxos[0].Hash, utxos[0].Index, &skaSpend.Hash)
		}
		if utxos[0].SKAAmount.Cmp(cointype.SKAAmountFromInt64(85e7)) != 0 {
			t.Errorf("change SKAAmount %v, want 8.5 SKA (85e7 atoms)",
				utxos[0].SKAAmount)
		}
		if utxos[0].CoinType != skaType {
			t.Errorf("change CoinType %v, want %v", utxos[0].CoinType, skaType)
		}
		// The spent credit is back in the unspent set, held by the
		// unmined spend: the spend's input must not look orphaned.
		credKey := keyCredit(&skaFund.Hash, 0, &makeBlockMeta(headers[0]).Block)
		if existsRawCredit(ns, credKey) == nil {
			t.Errorf("credit of %v is gone from the credits bucket", &skaFund.Hash)
		}
		fundOp := canonicalOutPoint(&skaFund.Hash, 0)
		if existsRawUnspent(ns, fundOp, s.chainParams) == nil {
			t.Errorf("credit of %v is not in the unspent set after the rollback", &skaFund.Hash)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// The user-visible consequence: once the unmined spend is removed —
	// for example when it is abandoned — the fund credit comes back as
	// the only unspent output, with its SKA amount and coin type intact.
	err = walletdb.Update(ctx, db, func(dbtx walletdb.ReadWriteTx) error {
		ns := dbtx.ReadWriteBucket(wtxmgrBucketKey)
		return s.RemoveUnconfirmed(ns, &skaSpend.MsgTx, &skaSpend.Hash)
	})
	if err != nil {
		t.Fatal(err)
	}

	err = walletdb.View(ctx, db, func(dbtx walletdb.ReadTx) error {
		utxos, err := s.UnspentOutputs(dbtx, skaType)
		if err != nil {
			return err
		}
		if len(utxos) != 1 || utxos[0].Hash != skaFund.Hash || utxos[0].Index != 0 {
			t.Errorf("after RemoveUnconfirmed: %d unspent outputs, want the fund "+
				"credit %v:0", len(utxos), &skaFund.Hash)
			return nil
		}
		if utxos[0].SKAAmount.Cmp(cointype.SKAAmountFromInt64(9e8)) != 0 {
			t.Errorf("fund credit SKAAmount %v, want 9 SKA (9e8 atoms)",
				utxos[0].SKAAmount)
		}
		if utxos[0].CoinType != skaType {
			t.Errorf("fund credit CoinType %v, want %v", utxos[0].CoinType, skaType)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
