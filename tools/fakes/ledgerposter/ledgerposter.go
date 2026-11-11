// Package ledgerposter fakes the ledger's posting API for payments'
// tests (payments' adapters.Ledger, task 4.4). A posting replays by its
// idempotency key, as the ledger's does, so a test can count credits
// exactly; Fail makes every posting error, as the ledger being down
// would, until Recover.
package ledgerposter

import (
	"context"
	"errors"
	"sync"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/adapters"
)

// ErrDown is what every posting returns after Fail.
var ErrDown = errors.New("ledgerposter: ledger unavailable")

type posted struct {
	txn uuid.UUID
	p   adapters.TopupPosting
}

// Fake records postings by key.
type Fake struct {
	mu    sync.Mutex
	byKey map[string]posted
	order []string
	calls int
	down  bool
}

// New returns an empty Fake.
func New() *Fake { return &Fake{byKey: map[string]posted{}} }

// Fail makes every posting error; Recover undoes it.
func (f *Fake) Fail() { f.set(true) }

// Recover lets postings through again.
func (f *Fake) Recover() { f.set(false) }

func (f *Fake) set(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

// PostTopup implements adapters.Ledger.
func (f *Fake) PostTopup(_ context.Context, p adapters.TopupPosting) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.down {
		return uuid.Nil, ErrDown
	}
	key := p.IdempotencyKey()
	if prev, ok := f.byKey[key]; ok {
		return prev.txn, nil
	}
	txn := uuid.New()
	f.byKey[key] = posted{txn: txn, p: p}
	f.order = append(f.order, key)
	return txn, nil
}

// Postings is every distinct posting, in order.
func (f *Fake) Postings() []adapters.TopupPosting {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]adapters.TopupPosting, len(f.order))
	for i, k := range f.order {
		out[i] = f.byKey[k].p
	}
	return out
}

// Credited is the total credited to tenant's main wallet, directly or
// out of suspense.
func (f *Fake) Credited(tenant uuid.UUID) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var sum int64
	for _, p := range f.byKey {
		if p.p.Kind != adapters.TopupToSuspense && p.p.TenantID == tenant {
			sum += p.p.AmountMinor
		}
	}
	return sum
}

// Suspense is the suspense account's balance: what went in less what
// staff matched out.
func (f *Fake) Suspense() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var sum int64
	for _, p := range f.byKey {
		switch p.p.Kind {
		case adapters.TopupToSuspense:
			sum += p.p.AmountMinor
		case adapters.TopupFromSuspense:
			sum -= p.p.AmountMinor
		}
	}
	return sum
}

// Clearing is provider clearing's total: every payment that arrived,
// matched or not.
func (f *Fake) Clearing() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var sum int64
	for _, p := range f.byKey {
		if p.p.Kind != adapters.TopupFromSuspense {
			sum += p.p.AmountMinor
		}
	}
	return sum
}

// Calls counts every PostTopup call, replays and failures included.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}
