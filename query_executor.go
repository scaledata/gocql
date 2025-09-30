package gocql

import (
	"log"
	"math"
	"math/rand"
	"time"
)

type ExecutableQuery interface {
	execute(conn *Conn) *Iter
	attempt(keyspace string, end, start time.Time, iter *Iter, host *HostInfo)
	retryPolicy() RetryPolicy
	GetRoutingKey() ([]byte, error)
	Keyspace() string
	RetryableQuery
}

type queryExecutor struct {
	pool              *policyConnPool
	policy            HostSelectionPolicy
	numRetries        int
	retryInitialDelay time.Duration
	retryMaxDelay     time.Duration
	maxRetryTime      time.Duration
}

func (q *queryExecutor) attemptQuery(qry ExecutableQuery, conn *Conn) *Iter {
	start := time.Now()
	iter := qry.execute(conn)
	end := time.Now()

	qry.attempt(q.pool.keyspace, end, start, iter, conn.host)

	return iter
}

func (q *queryExecutor) executeQuery(qry ExecutableQuery) (
	iter *Iter,
	err error,
) {
	start := time.Now()
	for i := 0; i <= q.numRetries && (q.maxRetryTime == 0 || time.Since(start) < q.maxRetryTime); i++ {
		if i > 0 {
			// Calculate exponential backoff with jitter
			backoffDelay := q.calculateBackoff(i)
			if iter != nil {
				log.Printf(
					"Execute query err: %v, iter.err: %v, retry attempt %d, sleeping for %v.",
					err,
					iter.err,
					i,
					backoffDelay,
				)
			} else {
				log.Printf(
					"Execute query err: %v, iter: <nil>, retry attempt %d, sleeping for %v.",
					err,
					i,
					backoffDelay,
				)
			}

			time.Sleep(backoffDelay)
		}
		iter, err = q.executeQueryOnce(qry)
		if err == nil && iter.err == nil {
			return
		}
	}
	return
}

// calculateBackoff computes exponential backoff with jitter
func (q *queryExecutor) calculateBackoff(attempt int) time.Duration {
	minDelay := q.retryInitialDelay
	maxDelay := q.retryMaxDelay

	// Set defaults if not configured
	if minDelay <= 0 {
		minDelay = 100 * time.Millisecond
	}
	if maxDelay <= 0 {
		maxDelay = 10 * time.Second
	}

	if attempt > 10 {
		return maxDelay
	}

	// Calculate exponential backoff: min * 2^(attempt-1)
	minFloat := float64(minDelay)
	backoff := minFloat * math.Pow(2, float64(attempt-1))

	// Add jitter: random value between -50% and +50% of minDelay
	jitter := rand.Float64()*minFloat - (minFloat / 2)
	backoff += jitter

	// Cap at max delay
	if backoff > float64(maxDelay) {
		return maxDelay
	}

	// Ensure non-negative
	if backoff < 0 {
		return minDelay
	}

	return time.Duration(backoff)
}

func (q *queryExecutor) executeQueryOnce(qry ExecutableQuery) (*Iter, error) {
	rt := qry.retryPolicy()
	hostIter := q.policy.Pick(qry)

	var iter *Iter
	for hostResponse := hostIter(); hostResponse != nil; hostResponse = hostIter() {
		host := hostResponse.Info()
		if host == nil || !host.IsUp() {
			continue
		}

		pool, ok := q.pool.getPool(host)
		if !ok {
			continue
		}

		conn := pool.Pick()
		if conn == nil {
			continue
		}

		iter = q.attemptQuery(qry, conn)
		// Update host
		hostResponse.Mark(iter.err)

		if rt == nil {
			iter.host = host
			break
		}

		switch rt.GetRetryType(iter.err) {
		case Retry:
			for rt.Attempt(qry) {
				iter = q.attemptQuery(qry, conn)
				hostResponse.Mark(iter.err)
				if iter.err == nil {
					iter.host = host
					return iter, nil
				}
				if rt.GetRetryType(iter.err) != Retry {
					break
				}
			}
		case Rethrow:
			return nil, iter.err
		case Ignore:
			return iter, nil
		case RetryNextHost:
		default:
		}

		// Exit for loop if the query was successful
		if iter.err == nil {
			iter.host = host
			return iter, nil
		}

		if !rt.Attempt(qry) {
			// What do here? Should we just return an error here?
			break
		}
	}

	if iter == nil {
		return nil, ErrNoConnections
	}

	return iter, nil
}
