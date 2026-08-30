package sync

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"net/http"
	"time"

	"google.golang.org/api/googleapi"
)

// RetryPolicy controls how transient Calendar API failures are retried.
//
// Retries use exponential backoff with full jitter: the nth retry (0-indexed)
// waits a random duration in [0, min(MaxBackoff, BaseBackoff*2^n)). Full
// jitter is deliberate — it spreads retries from many concurrent accounts so
// they don't synchronize into a thundering herd against the same API after a
// shared 429/5xx.
type RetryPolicy struct {
	// MaxAttempts is the total number of tries (not counting-only-retries):
	// 1 means no retry, 3 means the initial call plus up to 2 retries. Values
	// < 1 are treated as 1.
	MaxAttempts int

	// BaseBackoff is the backoff ceiling for the first retry before jitter.
	BaseBackoff time.Duration

	// MaxBackoff caps the per-attempt backoff ceiling regardless of how many
	// attempts have elapsed.
	MaxBackoff time.Duration
}

// DefaultRetryPolicy is a conservative policy suitable for the poll loop:
// a few attempts, sub-second base, capped at 30s so a single pass can't
// stall for minutes on one wedged account.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts: 4,
		BaseBackoff: 500 * time.Millisecond,
		MaxBackoff:  30 * time.Second,
	}
}

// isTransient reports whether err is worth retrying. It retries on:
//   - HTTP 429 (rate limited) and 5xx (server-side) from the Calendar API,
//   - transient network errors (timeouts, connection resets),
//
// and does NOT retry on 4xx client errors other than 429 (e.g. 401/403/404),
// which will not succeed on retry and usually indicate an auth/config problem
// the operator must fix.
func isTransient(err error) bool {
	if err == nil {
		return false
	}

	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		if apiErr.Code == http.StatusTooManyRequests {
			return true
		}
		return apiErr.Code >= 500 && apiErr.Code <= 599
	}

	// DNS-level errors: a temporary resolution failure (e.g. a resolver
	// timeout or SERVFAIL) doesn't always report Timeout() == true, so check
	// IsTemporary explicitly instead of relying solely on the net.Error
	// check below.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsTemporary {
		return true
	}

	// Network-level errors: timeouts and other temporary conditions.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	return false
}

// isAmbiguous reports whether err leaves it unknown whether the request's
// side effect (e.g. an insert) reached the server before the failure. Only a
// network-level timeout or temporary DNS failure is ambiguous in this sense:
// no response was received, so the server may have processed the request and
// failed only to reply. A googleapi.Error means the server did respond, so
// its outcome is known and is never ambiguous.
func isAmbiguous(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsTemporary {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

// backoffFor returns the jittered wait before the given retry attempt.
// attempt is 0-indexed: attempt 0 is the wait before the first retry.
func (p RetryPolicy) backoffFor(attempt int) time.Duration {
	base := p.BaseBackoff
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	maxb := p.MaxBackoff
	if maxb <= 0 {
		maxb = 30 * time.Second
	}

	// ceiling = base * 2^attempt, saturating at maxb. Compute in float to
	// avoid overflow on large attempt counts, then clamp.
	ceiling := float64(base) * pow2(attempt)
	if ceiling > float64(maxb) {
		ceiling = float64(maxb)
	}
	if ceiling <= 0 {
		return 0
	}
	// Full jitter: uniform in [0, ceiling).
	// #nosec G404 -- this randomness only spreads retry backoff to avoid a
	// thundering herd; it is not security-sensitive. math/rand/v2 is the
	// correct, non-erroring choice here — crypto/rand would be wasteful and
	// pointless for jitter.
	return time.Duration(rand.Float64() * ceiling)
}

// pow2 returns 2^n as a float64 for n >= 0, saturating at a large value to
// avoid +Inf for absurd attempt counts.
func pow2(n int) float64 {
	if n <= 0 {
		return 1
	}
	if n > 62 {
		// 2^62 is already astronomically larger than any sane MaxBackoff;
		// the caller clamps to MaxBackoff anyway.
		return 1 << 62
	}
	return float64(uint64(1) << uint(n))
}

// retry runs fn under the policy, retrying only transient errors, honoring
// ctx cancellation between attempts. It returns the last error if all
// attempts fail, or the first non-transient error immediately.
//
// onRetry, if non-nil, is called before each backoff sleep with the attempt
// number (1-indexed retry count) and the error being retried — used for
// logging.
func retry(ctx context.Context, p RetryPolicy, onRetry func(attempt int, err error, wait time.Duration), fn func() error) error {
	attempts := p.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for i := 0; i < attempts; i++ {
		// Respect cancellation before doing any work.
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return errors.Join(lastErr, err)
			}
			return err
		}

		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if !isTransient(lastErr) {
			return lastErr
		}
		// No point sleeping after the final attempt.
		if i == attempts-1 {
			break
		}

		wait := p.backoffFor(i)
		if onRetry != nil {
			onRetry(i+1, lastErr, wait)
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(lastErr, ctx.Err())
		case <-timer.C:
		}
	}
	return lastErr
}
