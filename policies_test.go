// Copyright (c) 2015 The gocql Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gocql

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/hailocab/go-hostpool"
)

// Tests of the round-robin host selection policy implementation
func TestHostPolicy_RoundRobin(t *testing.T) {
	policy := RoundRobinHostPolicy()

	hosts := [...]*HostInfo{
		{hostId: "0", connectAddress: net.IPv4(0, 0, 0, 1)},
		{hostId: "1", connectAddress: net.IPv4(0, 0, 0, 2)},
	}

	for _, host := range hosts {
		policy.AddHost(host)
	}

	// interleaved iteration should always increment the host
	iterA := policy.Pick(nil)
	if actual := iterA(); actual.Info() != hosts[0] {
		t.Errorf("Expected hosts[0] but was hosts[%s]", actual.Info().HostID())
	}
	iterB := policy.Pick(nil)
	if actual := iterB(); actual.Info() != hosts[1] {
		t.Errorf("Expected hosts[1] but was hosts[%s]", actual.Info().HostID())
	}
	if actual := iterB(); actual.Info() != hosts[0] {
		t.Errorf("Expected hosts[0] but was hosts[%s]", actual.Info().HostID())
	}
	if actual := iterA(); actual.Info() != hosts[1] {
		t.Errorf("Expected hosts[1] but was hosts[%s]", actual.Info().HostID())
	}

	iterC := policy.Pick(nil)
	if actual := iterC(); actual.Info() != hosts[0] {
		t.Errorf("Expected hosts[0] but was hosts[%s]", actual.Info().HostID())
	}
	if actual := iterC(); actual.Info() != hosts[1] {
		t.Errorf("Expected hosts[1] but was hosts[%s]", actual.Info().HostID())
	}
}

// Tests of the token-aware host selection policy implementation with a
// round-robin host selection policy fallback.
func TestHostPolicy_TokenAware(t *testing.T) {
	policy := TokenAwareHostPolicy(RoundRobinHostPolicy())

	query := &Query{}

	iter := policy.Pick(nil)
	if iter == nil {
		t.Fatal("host iterator was nil")
	}
	actual := iter()
	if actual != nil {
		t.Fatalf("expected nil from iterator, but was %v", actual)
	}

	// set the hosts
	hosts := [...]*HostInfo{
		{connectAddress: net.IPv4(10, 0, 0, 1), tokens: []string{"00"}},
		{connectAddress: net.IPv4(10, 0, 0, 2), tokens: []string{"25"}},
		{connectAddress: net.IPv4(10, 0, 0, 3), tokens: []string{"50"}},
		{connectAddress: net.IPv4(10, 0, 0, 4), tokens: []string{"75"}},
	}
	for _, host := range hosts {
		policy.AddHost(host)
	}

	// the token ring is not setup without the partitioner, but the fallback
	// should work
	if actual := policy.Pick(nil)(); !actual.Info().ConnectAddress().Equal(hosts[0].ConnectAddress()) {
		t.Errorf("Expected peer 0 but was %s", actual.Info().ConnectAddress())
	}

	query.RoutingKey([]byte("30"))
	if actual := policy.Pick(query)(); !actual.Info().ConnectAddress().Equal(hosts[1].ConnectAddress()) {
		t.Errorf("Expected peer 1 but was %s", actual.Info().ConnectAddress())
	}

	policy.SetPartitioner("OrderedPartitioner")

	// now the token ring is configured
	query.RoutingKey([]byte("20"))
	iter = policy.Pick(query)
	if actual := iter(); !actual.Info().ConnectAddress().Equal(hosts[1].ConnectAddress()) {
		t.Errorf("Expected peer 1 but was %s", actual.Info().ConnectAddress())
	}
	// rest are round robin
	if actual := iter(); !actual.Info().ConnectAddress().Equal(hosts[2].ConnectAddress()) {
		t.Errorf("Expected peer 2 but was %s", actual.Info().ConnectAddress())
	}
	if actual := iter(); !actual.Info().ConnectAddress().Equal(hosts[3].ConnectAddress()) {
		t.Errorf("Expected peer 3 but was %s", actual.Info().ConnectAddress())
	}
	if actual := iter(); !actual.Info().ConnectAddress().Equal(hosts[0].ConnectAddress()) {
		t.Errorf("Expected peer 0 but was %s", actual.Info().ConnectAddress())
	}
}

// Tests of the host pool host selection policy implementation
func TestHostPolicy_HostPool(t *testing.T) {
	policy := HostPoolHostPolicy(hostpool.New(nil))

	hosts := []*HostInfo{
		{hostId: "0", connectAddress: net.IPv4(10, 0, 0, 0)},
		{hostId: "1", connectAddress: net.IPv4(10, 0, 0, 1)},
	}

	// Using set host to control the ordering of the hosts as calling "AddHost" iterates the map
	// which will result in an unpredictable ordering
	policy.(*hostPoolHostPolicy).SetHosts(hosts)

	// the first host selected is actually at [1], but this is ok for RR
	// interleaved iteration should always increment the host
	iter := policy.Pick(nil)
	actualA := iter()
	if actualA.Info().HostID() != "0" {
		t.Errorf("Expected hosts[0] but was hosts[%s]", actualA.Info().HostID())
	}
	actualA.Mark(nil)

	actualB := iter()
	if actualB.Info().HostID() != "1" {
		t.Errorf("Expected hosts[1] but was hosts[%s]", actualB.Info().HostID())
	}
	actualB.Mark(fmt.Errorf("error"))

	actualC := iter()
	if actualC.Info().HostID() != "0" {
		t.Errorf("Expected hosts[0] but was hosts[%s]", actualC.Info().HostID())
	}
	actualC.Mark(nil)

	actualD := iter()
	if actualD.Info().HostID() != "0" {
		t.Errorf("Expected hosts[0] but was hosts[%s]", actualD.Info().HostID())
	}
	actualD.Mark(nil)
}

func TestHostPolicy_RoundRobin_NilHostInfo(t *testing.T) {
	policy := RoundRobinHostPolicy()

	host := &HostInfo{hostId: "host-1"}
	policy.AddHost(host)

	iter := policy.Pick(nil)
	next := iter()
	if next == nil {
		t.Fatal("got nil host")
	} else if v := next.Info(); v == nil {
		t.Fatal("got nil HostInfo")
	} else if v.HostID() != host.HostID() {
		t.Fatalf("expected host %v got %v", host, v)
	}

	next = iter()
	if next != nil {
		t.Errorf("expected to get nil host got %+v", next)
		if next.Info() == nil {
			t.Fatalf("HostInfo is nil")
		}
	}
}

func TestHostPolicy_TokenAware_NilHostInfo(t *testing.T) {
	policy := TokenAwareHostPolicy(RoundRobinHostPolicy())

	hosts := [...]*HostInfo{
		{connectAddress: net.IPv4(10, 0, 0, 0), tokens: []string{"00"}},
		{connectAddress: net.IPv4(10, 0, 0, 1), tokens: []string{"25"}},
		{connectAddress: net.IPv4(10, 0, 0, 2), tokens: []string{"50"}},
		{connectAddress: net.IPv4(10, 0, 0, 3), tokens: []string{"75"}},
	}
	for _, host := range hosts {
		policy.AddHost(host)
	}
	policy.SetPartitioner("OrderedPartitioner")

	query := &Query{}
	query.RoutingKey([]byte("20"))

	iter := policy.Pick(query)
	next := iter()
	if next == nil {
		t.Fatal("got nil host")
	} else if v := next.Info(); v == nil {
		t.Fatal("got nil HostInfo")
	} else if !v.ConnectAddress().Equal(hosts[1].ConnectAddress()) {
		t.Fatalf("expected peer 1 got %v", v.ConnectAddress())
	}

	// Empty the hosts to trigger the panic when using the fallback.
	for _, host := range hosts {
		policy.RemoveHost(host)
	}

	next = iter()
	if next != nil {
		t.Errorf("expected to get nil host got %+v", next)
		if next.Info() == nil {
			t.Fatalf("HostInfo is nil")
		}
	}
}

func TestCOWList_Add(t *testing.T) {
	var cow cowHostList

	toAdd := [...]net.IP{net.IPv4(10, 0, 0, 1), net.IPv4(10, 0, 0, 2), net.IPv4(10, 0, 0, 3)}

	for _, addr := range toAdd {
		if !cow.add(&HostInfo{connectAddress: addr}) {
			t.Fatal("did not add peer which was not in the set")
		}
	}

	hosts := cow.get()
	if len(hosts) != len(toAdd) {
		t.Fatalf("expected to have %d hosts got %d", len(toAdd), len(hosts))
	}

	set := make(map[string]bool)
	for _, host := range hosts {
		set[string(host.ConnectAddress())] = true
	}

	for _, addr := range toAdd {
		if !set[string(addr)] {
			t.Errorf("addr was not in the host list: %q", addr)
		}
	}
}

// TestSimpleRetryPolicy makes sure that we only allow 1 + numRetries attempts
func TestSimpleRetryPolicy(t *testing.T) {
	q := &Query{}

	// this should allow a total of 3 tries.
	rt := &SimpleRetryPolicy{NumRetries: 2}

	cases := []struct {
		attempts int
		allow    bool
	}{
		{0, true},
		{1, true},
		{2, true},
		{3, false},
		{4, false},
		{5, false},
	}

	for _, c := range cases {
		q.attempts = c.attempts
		if c.allow && !rt.Attempt(q) {
			t.Fatalf("should allow retry after %d attempts", c.attempts)
		}
		if !c.allow && rt.Attempt(q) {
			t.Fatalf("should not allow retry after %d attempts", c.attempts)
		}
	}
}

func TestExponentialBackoffPolicy(t *testing.T) {
	// test with defaults
	sut := &ExponentialBackoffRetryPolicy{NumRetries: 2}

	cases := []struct {
		attempts int
		delay    time.Duration
	}{

		{1, 100 * time.Millisecond},
		{2, (2) * 100 * time.Millisecond},
		{3, (2 * 2) * 100 * time.Millisecond},
		{4, (2 * 2 * 2) * 100 * time.Millisecond},
	}
	for _, c := range cases {
		// test 100 times for each case
		for i := 0; i < 100; i++ {
			d := sut.napTime(c.attempts)
			if d < c.delay-(100*time.Millisecond)/2 {
				t.Fatalf("Delay %d less than jitter min of %d", d, c.delay-100*time.Millisecond/2)
			}
			if d > c.delay+(100*time.Millisecond)/2 {
				t.Fatalf("Delay %d greater than jitter max of %d", d, c.delay+100*time.Millisecond/2)
			}
		}
	}
}

func TestDowngradingConsistencyRetryPolicy(t *testing.T) {

	q := &Query{cons: LocalQuorum}

	rewt0 := &RequestErrWriteTimeout{
		Received:  0,
		WriteType: "SIMPLE",
	}

	rewt1 := &RequestErrWriteTimeout{
		Received:  1,
		WriteType: "BATCH",
	}

	rewt2 := &RequestErrWriteTimeout{
		WriteType: "UNLOGGED_BATCH",
	}

	rert := &RequestErrReadTimeout{}

	reu0 := &RequestErrUnavailable{
		Alive: 0,
	}

	reu1 := &RequestErrUnavailable{
		Alive: 1,
	}

	// this should allow a total of 3 tries.
	consistencyLevels := []Consistency{Three, Two, One}
	rt := &DowngradingConsistencyRetryPolicy{ConsistencyLevelsToTry: consistencyLevels}
	cases := []struct {
		attempts  int
		allow     bool
		err       error
		retryType RetryType
	}{
		{0, true, rewt0, Rethrow},
		{3, true, rewt1, Ignore},
		{1, true, rewt2, Retry},
		{2, true, rert, Retry},
		{4, false, reu0, Rethrow},
		{16, false, reu1, Retry},
	}

	for _, c := range cases {
		q.attempts = c.attempts
		if c.retryType != rt.GetRetryType(c.err) {
			t.Fatalf("retry type should be %v", c.retryType)
		}
		if c.allow && !rt.Attempt(q) {
			t.Fatalf("should allow retry after %d attempts", c.attempts)
		}
		if !c.allow && rt.Attempt(q) {
			t.Fatalf("should not allow retry after %d attempts", c.attempts)
		}
	}
}

func TestHostPolicy_DCAwareRR(t *testing.T) {
	p := DCAwareRoundRobinPolicy("local")

	hosts := [...]*HostInfo{
		{hostId: "0", connectAddress: net.ParseIP("10.0.0.1"), dataCenter: "local"},
		{hostId: "1", connectAddress: net.ParseIP("10.0.0.2"), dataCenter: "local"},
		{hostId: "2", connectAddress: net.ParseIP("10.0.0.3"), dataCenter: "remote"},
		{hostId: "3", connectAddress: net.ParseIP("10.0.0.4"), dataCenter: "remote"},
	}

	for _, host := range hosts {
		p.AddHost(host)
	}

	// interleaved iteration should always increment the host
	iterA := p.Pick(nil)
	if actual := iterA(); actual.Info() != hosts[0] {
		t.Errorf("Expected hosts[0] but was hosts[%s]", actual.Info().HostID())
	}
	iterB := p.Pick(nil)
	if actual := iterB(); actual.Info() != hosts[1] {
		t.Errorf("Expected hosts[1] but was hosts[%s]", actual.Info().HostID())
	}
	if actual := iterB(); actual.Info() != hosts[0] {
		t.Errorf("Expected hosts[0] but was hosts[%s]", actual.Info().HostID())
	}
	if actual := iterA(); actual.Info() != hosts[1] {
		t.Errorf("Expected hosts[1] but was hosts[%s]", actual.Info().HostID())
	}
	iterC := p.Pick(nil)
	if actual := iterC(); actual.Info() != hosts[0] {
		t.Errorf("Expected hosts[0] but was hosts[%s]", actual.Info().HostID())
	}
	p.RemoveHost(hosts[0])
	if actual := iterC(); actual.Info() != hosts[1] {
		t.Errorf("Expected hosts[1] but was hosts[%s]", actual.Info().HostID())
	}
	p.RemoveHost(hosts[1])
	iterD := p.Pick(nil)
	if actual := iterD(); actual.Info() != hosts[2] {
		t.Errorf("Expected hosts[2] but was hosts[%s]", actual.Info().HostID())
	}
	if actual := iterD(); actual.Info() != hosts[3] {
		t.Errorf("Expected hosts[3] but was hosts[%s]", actual.Info().HostID())
	}

}

func TestExponentialReconnectionPolicy(t *testing.T) {
	policy := &ExponentialReconnectionPolicy{
		MaxRetries:      5,
		InitialInterval: 100 * time.Millisecond,
	}

	// Test that MaxRetries is returned correctly
	if policy.GetMaxRetries() != 5 {
		t.Fatalf("Expected MaxRetries to be 5, got %d", policy.GetMaxRetries())
	}

	// Test exponential growth of intervals
	cases := []struct {
		currentRetry int
		minExpected  time.Duration
		maxExpected  time.Duration
	}{
		{0, 0 * time.Millisecond, 100 * time.Millisecond},     // 2^(-1) * 100ms = 50ms with jitter (0-100ms range)
		{1, 50 * time.Millisecond, 150 * time.Millisecond},    // 2^0 * 100ms = 100ms with jitter (50-150ms range)
		{2, 150 * time.Millisecond, 250 * time.Millisecond},   // 2^1 * 100ms = 200ms with jitter (150-250ms range)
		{3, 350 * time.Millisecond, 450 * time.Millisecond},   // 2^2 * 100ms = 400ms with jitter (350-450ms range)
		{4, 750 * time.Millisecond, 850 * time.Millisecond},   // 2^3 * 100ms = 800ms with jitter (750-850ms range)
		{5, 1550 * time.Millisecond, 1650 * time.Millisecond}, // 2^4 * 100ms = 1600ms with jitter (1550-1650ms range)
	}

	for _, c := range cases {
		// Test multiple times due to jitter
		for i := 0; i < 10; i++ {
			interval := policy.GetInterval(c.currentRetry)
			if interval < c.minExpected || interval > c.maxExpected {
				t.Errorf("currentRetry=%d: expected interval between %v and %v, got %v",
					c.currentRetry, c.minExpected, c.maxExpected, interval)
			}
		}
	}

	// Test that intervals increase with retry attempts
	for retry := 1; retry <= 3; retry++ {
		// Due to jitter, we can't guarantee strict ordering, but the average should increase
		// Test multiple times and check that current is generally larger
		largerCount := 0
		for i := 0; i < 20; i++ {
			if policy.GetInterval(retry) > policy.GetInterval(retry-1) {
				largerCount++
			}
		}
		if largerCount < 10 { // At least half should be larger due to exponential growth
			t.Errorf("Expected interval for retry %d to generally be larger than retry %d", retry, retry-1)
		}
	}

	// Test with different InitialInterval
	policy2 := &ExponentialReconnectionPolicy{
		MaxRetries:      3,
		InitialInterval: 500 * time.Millisecond,
	}

	interval0 := policy2.GetInterval(0)

	// First interval (retry 0) should be around 0.5 * InitialInterval (with jitter)
	// 2^(-1) * 500ms = 250ms, with jitter range approximately 125ms to 375ms
	if interval0 < 125*time.Millisecond || interval0 > 375*time.Millisecond {
		t.Errorf("Expected first interval around 250ms (0.5 * 500ms), got %v", interval0)
	}

	// Test that the policy uses currentRetry parameter correctly
	// by ensuring different retry values produce different results
	intervals := make(map[int][]time.Duration)
	for retry := 0; retry < 4; retry++ {
		for i := 0; i < 5; i++ {
			intervals[retry] = append(intervals[retry], policy.GetInterval(retry))
		}
	}

	// Calculate averages to verify exponential growth despite jitter
	avgInterval := func(durations []time.Duration) time.Duration {
		var sum time.Duration
		for _, d := range durations {
			sum += d
		}
		return sum / time.Duration(len(durations))
	}

	avg0 := avgInterval(intervals[0])
	avg1 := avgInterval(intervals[1])
	avg2 := avgInterval(intervals[2])
	avg3 := avgInterval(intervals[3])

	// Verify exponential growth pattern (each should be roughly double the previous)
	if avg1 < avg0 {
		t.Errorf("Average interval for retry 1 (%v) should be >= retry 0 (%v)", avg1, avg0)
	}
	if avg2 < time.Duration(float64(avg1)*1.5) { // Allow some tolerance due to jitter
		t.Errorf("Average interval for retry 2 (%v) should be significantly larger than retry 1 (%v)", avg2, avg1)
	}
	if avg3 < time.Duration(float64(avg2)*1.5) {
		t.Errorf("Average interval for retry 3 (%v) should be significantly larger than retry 2 (%v)", avg3, avg2)
	}
}
