// Copyright (c) 2026 The Monetarium developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"context"
	"testing"
	"time"

	"github.com/monetarium/monetarium-node/chaincfg/chainhash"
	"github.com/monetarium/monetarium-node/cointype"
	"github.com/monetarium/monetarium-node/dcrutil"
	gcs2 "github.com/monetarium/monetarium-node/gcs"
	"github.com/monetarium/monetarium-node/gcs/blockcf2"
	"github.com/monetarium/monetarium-node/wire"
	werrors "github.com/monetarium/monetarium-wallet/errors"
	"github.com/monetarium/monetarium-wallet/wallet/udb"
	"github.com/monetarium/monetarium-wallet/wallet/walletdb"
)

// TestExpiryCreditReserveAndListUnspent checks the wallet-level maturity rule
// for credits of transactions with an expiry behind ReserveOutputsForAmount
// (which funds ticket purchases and VSP fee payments) and ListUnspent: both
// exclude the credit until CoinbaseMaturity blocks have passed over it, and
// include it afterwards.
func TestExpiryCreditReserveAndListUnspent(t *testing.T) {
	ctx := context.Background()
	cfg := basicWalletConfig
	w, teardown := testWallet(ctx, t, &cfg, nil)
	defer teardown()

	params := w.chainParams
	addr, err := w.NewExternalAddress(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, pkScript := addr.PaymentScript()

	tx := wire.NewMsgTx()
	tx.Expiry = 100
	tx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(&chainhash.Hash{0x01}, 0,
		wire.TxTreeRegular), 0, nil))
	tx.AddTxOut(wire.NewTxOut(1e8, pkScript))
	rec, err := udb.NewTxRecordFromMsgTx(tx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	txHash := rec.Hash

	emptyFilter, _ := gcs2.FromBytesV2(blockcf2.B, blockcf2.M, nil)

	// Mine the credit: one block past genesis, the credit at height 1.
	// Blocks approve their parent (BlockValid), or stake invalidation
	// moves the credit out of the spendable set.
	b1 := &wire.BlockHeader{PrevBlock: params.GenesisHash, VoteBits: dcrutil.BlockValid, Height: 1}
	b1Hash := b1.BlockHash()
	err = walletdb.Update(ctx, w.db, func(dbtx walletdb.ReadWriteTx) error {
		if err := w.txStore.InsertMemPoolTx(dbtx, rec); err != nil {
			return err
		}
		if err := w.txStore.AddCredit(dbtx, rec, nil, 0, false, 0); err != nil {
			return err
		}
		ns := dbtx.ReadWriteBucket(wtxmgrNamespaceKey)
		if err := w.txStore.ExtendMainChain(ns, b1, &b1Hash, emptyFilter); err != nil {
			return err
		}
		return w.txStore.InsertMinedTx(dbtx, rec, &b1Hash)
	})
	if err != nil {
		t.Fatal(err)
	}

	extend := func(n int32) error {
		return walletdb.Update(ctx, w.db, func(dbtx walletdb.ReadWriteTx) error {
			ns := dbtx.ReadWriteBucket(wtxmgrNamespaceKey)
			prev, height := b1Hash, int32(1)
			for i := int32(0); i < n; i++ {
				height++
				h := &wire.BlockHeader{PrevBlock: prev, VoteBits: dcrutil.BlockValid, Height: uint32(height)}
				hHash := h.BlockHash()
				if err := w.txStore.ExtendMainChain(ns, h, &hHash, emptyFilter); err != nil {
					return err
				}
				prev = hHash
			}
			return nil
		})
	}

	ct := cointype.CoinTypeVAR

	// Immature: the ticket-purchase/VSP-fee reserve path cannot fund the
	// amount from the credit (insufficient balance), and listunspent does
	// not offer it.
	if _, err := w.ReserveOutputsForAmount(ctx, 0, 1e8, cointype.Zero(), 1, ct); !werrors.Is(err, werrors.InsufficientBalance) {
		t.Fatalf("immature: ReserveOutputsForAmount error = %v, want insufficient balance", err)
	}
	unspent, err := w.ListUnspent(ctx, 1, 9999999, nil, "default", &ct)
	if err != nil {
		t.Fatal(err)
	}
	if len(unspent) != 0 {
		t.Fatalf("immature: ListUnspent returned %d outputs, want 0", len(unspent))
	}

	// Extend the chain past coinbase maturity.
	maturity := int32(params.CoinbaseMaturity)
	if err := extend(maturity + 5); err != nil {
		t.Fatal(err)
	}

	// Matured: the same credit is listed and reservable.
	unspent, err = w.ListUnspent(ctx, 1, 9999999, nil, "default", &ct)
	if err != nil {
		t.Fatal(err)
	}
	if len(unspent) != 1 {
		t.Fatalf("matured: ListUnspent returned %d outputs, want 1", len(unspent))
	}
	reserved, err := w.ReserveOutputsForAmount(ctx, 0, 1e8, cointype.Zero(), 1, ct)
	if err != nil {
		t.Fatal(err)
	}
	if len(reserved) != 1 || reserved[0].OutPoint.Hash != txHash {
		t.Fatalf("matured: ReserveOutputsForAmount = %v, want the expiry credit %v",
			reserved, txHash)
	}
}
