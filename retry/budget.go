package retry

import "sync"

// Budget caps retries as a proportion of traffic rather than per request.
//
// A per-request limit of "3 attempts" sounds conservative but is not: when a
// target starts failing *every* request, every caller uses its full allowance
// at once and the target receives three times the load exactly when it can
// least afford it. A budget makes retries a scarce shared resource — each
// request deposits a fraction of a token, each retry spends a whole one — so
// occasional failures are retried freely while a broad outage stops being
// retried almost entirely.
//
// This is how Envoy and Finagle do it, and it is the single most effective
// guard against a retry storm.
type Budget struct {
	mu sync.Mutex
	// Accounting is in fixed-point units rather than float64 because the
	// deposits are small and numerous: adding 0.1 a hundred times gives
	// 9.999…, which silently loses a token the caller had earned.
	tokens int64
	perReq int64
	max    int64
}

const (
	// tokenUnit is how many fixed-point units make one whole retry.
	tokenUnit = 1_000_000
	// burstAllowance is how many retries may be saved up. Without a ceiling,
	// a long healthy period would bank enough tokens to fund a storm later.
	burstAllowance = 10
)

// NewBudget returns a budget allowing `ratio` retries per request — 0.1 means
// retries may not exceed 10% of traffic. A ratio of 0 or less disables
// retrying entirely.
func NewBudget(ratio float64) *Budget {
	if ratio < 0 {
		ratio = 0
	}
	return &Budget{
		perReq: int64(ratio * tokenUnit),
		max:    burstAllowance * tokenUnit,
	}
}

// Deposit credits the budget for one request the caller made. Call it once per
// logical request, regardless of how many attempts follow.
func (b *Budget) Deposit() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens += b.perReq
	if b.tokens > b.max {
		b.tokens = b.max
	}
}

// Withdraw spends one token and reports whether a retry is allowed.
func (b *Budget) Withdraw() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tokens < tokenUnit {
		return false
	}
	b.tokens -= tokenUnit
	return true
}

// Available is the number of retries currently affordable, for reporting.
func (b *Budget) Available() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return float64(b.tokens) / tokenUnit
}
