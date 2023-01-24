package worker

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"go.sia.tech/core/types"
	rhpv3 "go.sia.tech/renterd/rhp/v3"
)

// priceTableValidityLeeway is the number of time before the actual expiry of a
// price table when we start considering it invalid.
const priceTableValidityLeeway = 30 * time.Second

type withTransportV3 func(ctx context.Context, hostIP string, hostKey types.PublicKey, fn func(*rhpv3.Transport) error) (err error)

type priceTables struct {
	withTransport withTransportV3
	mu            sync.Mutex
	priceTables   map[types.PublicKey]*priceTable
}

type priceTable struct {
	pt     *rhpv3.HostPriceTable
	hk     types.PublicKey
	expiry time.Time

	mu            sync.Mutex
	ongoingUpdate *priceTableUpdate
}

type priceTableUpdate struct {
	err  error
	done chan struct{}
	pt   *rhpv3.HostPriceTable
}

func newPriceTables(transportFn withTransportV3) *priceTables {
	return &priceTables{
		priceTables:   make(map[types.PublicKey]*priceTable),
		withTransport: transportFn,
	}
}

// PriceTable returns a price table for the given host and an bool to indicate
// whether it is valid or not.
func (pts *priceTables) PriceTable(hk types.PublicKey) (rhpv3.HostPriceTable, bool) {
	pt := pts.priceTable(hk)
	if pt.pt == nil {
		return rhpv3.HostPriceTable{}, false
	}
	return *pt.pt, time.Now().Before(pt.expiry.Add(priceTableValidityLeeway))
}

// Update updates a price table with the given host using the provided payment
// function to pay for it.
func (pts *priceTables) Update(ctx context.Context, payFn rhpv3.PriceTablePaymentFunc, hostIP string, hk types.PublicKey) (rhpv3.HostPriceTable, error) {
	// Fetch the price table to update.
	pt := pts.priceTable(hk)

	// Check if there is some update going on already. If not, create one.
	pt.mu.Lock()
	ongoing := pt.ongoingUpdate
	var performUpdate bool
	if ongoing == nil {
		ongoing = &priceTableUpdate{
			done: make(chan struct{}),
		}
		pt.ongoingUpdate = ongoing
		performUpdate = true
	}
	pt.mu.Unlock()

	// If this thread is not supposed to perform the update, just block and
	// return the result.
	if !performUpdate {
		select {
		case <-ctx.Done():
			return rhpv3.HostPriceTable{}, errors.New("timeout while blocking for pricetable update")
		case <-ongoing.done:
		}
		if ongoing.err != nil {
			return rhpv3.HostPriceTable{}, ongoing.err
		} else {
			return *ongoing.pt, nil
		}
	}

	// Update price table.
	var hpt rhpv3.HostPriceTable
	err := pts.withTransport(ctx, hostIP, hk, func(t *rhpv3.Transport) (err error) {
		hpt, err = rhpv3.RPCPriceTable(t, payFn)
		return err
	})

	pt.mu.Lock()
	defer pt.mu.Unlock()

	// On success we update the pt.
	if err == nil {
		pt.pt = &hpt
		pt.expiry = time.Now().Add(hpt.Validity)
	}

	// Signal that the update is over.
	ongoing.err = err
	close(ongoing.done)
	pt.ongoingUpdate = nil
	return hpt, err
}

// priceTable returns a priceTable from priceTables for the given host or
// creates a new one.
func (pts *priceTables) priceTable(hk types.PublicKey) *priceTable {
	pts.mu.Lock()
	defer pts.mu.Unlock()
	pt, exists := pts.priceTables[hk]
	if !exists {
		pt = &priceTable{
			hk: hk,
		}
		pts.priceTables[hk] = pt
	}
	return pt
}

// preparePriceTableContractPayment prepare a payment function to pay for a
// price table from the given host using the provided revision.
// This way of paying for a price table should only be used if payment by EA is
// not possible or if we already need a contract revision anyway. e.g. funding
// an EA.
func (w *worker) preparePriceTableContractPayment(hk types.PublicKey, revision *types.FileContractRevision) rhpv3.PriceTablePaymentFunc {
	return func(pt rhpv3.HostPriceTable) (rhpv3.PaymentMethod, error) {
		// TODO: gouging check on price table

		refundAccount := rhpv3.Account(w.accounts.deriveAccountKey(hk).PublicKey())
		rk := w.deriveRenterKey(hk)
		payment, ok := rhpv3.PayByContract(revision, pt.UpdatePriceTableCost, refundAccount, rk)
		if !ok {
			return nil, errors.New("insufficient funds")
		}
		return &payment, nil
	}
}

// preparePriceTableAccountPayment prepare a payment function to pay for a
// price table from the given host using the provided revision.
// This is the preferred way of paying for a price table since it is faster and
// doesn't require locking a contract.
func (w *worker) preparePriceTableAccountPayment(hk types.PublicKey) rhpv3.PriceTablePaymentFunc {
	return func(pt rhpv3.HostPriceTable) (rhpv3.PaymentMethod, error) {
		// TODO: gouging check on price table

		accountKey := w.accounts.deriveAccountKey(hk)
		account := rhpv3.Account(accountKey.PublicKey())
		payment := rhpv3.PayByEphemeralAccount(account, pt.UpdatePriceTableCost, math.MaxUint64, accountKey)
		return &payment, nil
	}
}