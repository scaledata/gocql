package gocql

import (
	"log"
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
	pool       *policyConnPool
	policy     HostSelectionPolicy
	numRetries int
}

func (q *queryExecutor) attemptQuery(qry ExecutableQuery, conn *Conn) *Iter {
	start := time.Now()
	iter := qry.execute(conn)
	end := time.Now()

	qry.attempt(q.pool.keyspace, end, start, iter, conn.host)

	return iter
}

func (q *queryExecutor) executeQuery(qry ExecutableQuery) (iter *Iter, err error) {
	for i := 0; i <= q.numRetries; i++ {
		if i > 0 {
			log.Printf("Execute query error: %v, retry attempt %d, sleeping for %d seconds.", err, i, i)
		}
		time.Sleep(time.Duration(i) * time.Second)
		iter, err = q.executeQueryOnce(qry)
		if err == nil && iter.err == nil {
			return
		}
	}
	return
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
