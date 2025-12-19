// Copyright (c) 2012 The gocql Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// +build all unit

package gocql

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/gocql/gocql/internal/streams"
)

// mockNetConn is a mock net.Conn for testing
type mockNetConn struct {
	net.Conn
	closed bool
}

func (m *mockNetConn) Close() error {
	m.closed = true
	return nil
}

func (m *mockNetConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9042}
}

// mockErrorHandler is a no-op error handler for testing
type mockErrorHandler struct{}

func (m *mockErrorHandler) HandleError(conn *Conn, err error, closed bool) {
	// No-op for testing
}

// mockConn creates a mock connection for testing
func mockConn(protocol int, createdAt time.Time, maxLifetime time.Duration) *Conn {
	return &Conn{
		conn:         &mockNetConn{},
		streams:      streams.New(protocol),
		cfg:          &ConnConfig{ConnMaxLifetime: maxLifetime},
		createdAt:    createdAt,
		closed:       0,
		calls:        make(map[int]*callReq),
		quit:         make(chan struct{}),
		errorHandler: &mockErrorHandler{},
	}
}

// TestConnectionExpiration_BasicExpiration tests that expired connections are moved to draining pool
func TestConnectionExpiration_BasicExpiration(t *testing.T) {
	session := createTestSession()
	host := &HostInfo{
		connectAddress: net.IPv4(127, 0, 0, 1),
		port:           9042,
	}

	pool := newHostConnPool(session, host, 9042, 5, "")

	// Add connections with different ages
	now := time.Now()
	expiredConn := mockConn(3, now.Add(-2*time.Hour), 1*time.Hour)
	freshConn := mockConn(3, now.Add(-30*time.Minute), 1*time.Hour)

	// Verify expiration check works
	if !expiredConn.IsExpired() {
		t.Fatalf("expiredConn should be expired")
	}
	if freshConn.IsExpired() {
		t.Fatalf("freshConn should not be expired")
	}

	pool.mu.Lock()
	pool.conns = []*Conn{freshConn, expiredConn}
	pool.mu.Unlock()

	// Pick a connection - this will start the maintenance goroutine
	// It may return either connection (expiration is handled asynchronously)
	_ = pool.Pick()

	// Wait for maintenance goroutine to move expired connection to draining pool
	time.Sleep(1500 * time.Millisecond)

	pool.mu.Lock()
	defer pool.mu.Unlock()

	t.Logf("Active connections: %d, Draining connections: %d", len(pool.conns), len(pool.expiredConns))
	t.Logf("Expired conn closed: %v", expiredConn.Closed())

	// Expired connection should be removed from active pool
	if len(pool.conns) != 1 {
		t.Errorf("Expected 1 connection in active pool, got %d", len(pool.conns))
	}

	// Expired connection should have been moved to draining and then closed
	// (since it has no active streams)
	if !expiredConn.Closed() {
		t.Errorf("Expected expired connection to be closed")
	}

	// The draining pool should be empty since the connection was closed immediately
	if len(pool.expiredConns) != 0 {
		t.Errorf("Expected 0 connections in draining pool (closed immediately), got %d", len(pool.expiredConns))
	}
}

// TestConnectionExpiration_ActiveQueries tests that connections with active queries are not closed
func TestConnectionExpiration_ActiveQueries(t *testing.T) {
	session := createTestSession()
	host := &HostInfo{
		connectAddress: net.IPv4(127, 0, 0, 1),
		port:           9042,
	}

	pool := newHostConnPool(session, host, 9042, 5, "")
	// Set fast ticker for testing
	pool.maintenanceTicker = 100 * time.Millisecond

	// Create an expired connection with active streams
	now := time.Now()
	expiredConn := mockConn(3, now.Add(-2*time.Hour), 1*time.Hour)

	// Simulate active streams by allocating some
	stream1, _ := expiredConn.streams.GetStream()
	stream2, _ := expiredConn.streams.GetStream()

	pool.mu.Lock()
	pool.conns = []*Conn{expiredConn}
	pool.mu.Unlock()

	// Wait for maintenance goroutine to move it to draining and check it
	time.Sleep(200 * time.Millisecond)

	pool.mu.Lock()
	// Connection should still be in draining pool (not closed)
	if len(pool.expiredConns) != 1 {
		t.Errorf("Expected connection to still be draining, got %d connections", len(pool.expiredConns))
	}
	pool.mu.Unlock()

	// Release streams
	expiredConn.streams.Clear(stream1)
	expiredConn.streams.Clear(stream2)

	// Wait for maintenance goroutine to close it
	time.Sleep(300 * time.Millisecond)

	pool.mu.Lock()
	defer pool.mu.Unlock()

	// Now it should be closed and removed from draining pool
	if len(pool.expiredConns) != 0 {
		t.Errorf("Expected connection to be closed and removed, got %d connections", len(pool.expiredConns))
	}
}

// TestConnectionExpiration_DrainingLimit tests that no more than maxDrainingConns are drained at once
func TestConnectionExpiration_DrainingLimit(t *testing.T) {
	session := createTestSession()
	host := &HostInfo{
		connectAddress: net.IPv4(127, 0, 0, 1),
		port:           9042,
	}

	// Set pool size equal to number of expired connections to prevent fill() from being called
	numExpired := maxDrainingConns + 5
	pool := newHostConnPool(session, host, 9042, numExpired, "")

	// Create more expired connections than the limit, with active streams so they won't close immediately
	now := time.Now()

	pool.mu.Lock()
	for i := 0; i < numExpired; i++ {
		expiredConn := mockConn(3, now.Add(-2*time.Hour), 1*time.Hour)
		// Allocate a stream so the connection won't be closed immediately by maintenance goroutine
		expiredConn.streams.GetStream()
		pool.conns = append(pool.conns, expiredConn)
	}
	pool.mu.Unlock()

	// Wait for maintenance goroutine to move connections to draining
	time.Sleep(100 * time.Millisecond)

	pool.mu.Lock()
	drainingCount := len(pool.expiredConns)
	activeCount := len(pool.conns)
	pool.mu.Unlock()

	// The total should still be numExpired (some in draining, some in active)
	total := drainingCount + activeCount
	if total != numExpired {
		t.Errorf("Expected total of %d connections, got %d (draining: %d, active: %d)",
			numExpired, total, drainingCount, activeCount)
	}

	// Should have moved at most maxDrainingConns to draining pool
	if drainingCount > maxDrainingConns {
		t.Errorf("Expected at most %d connections in draining pool, got %d", maxDrainingConns, drainingCount)
	}

	// Should have at least some connections remaining in active pool
	if activeCount == 0 && numExpired > maxDrainingConns {
		t.Errorf("Expected some connections remaining in active pool when total > maxDrainingConns")
	}
}

// TestConnectionExpiration_ConcurrentPick tests concurrent access to Pick()
func TestConnectionExpiration_ConcurrentPick(t *testing.T) {
	session := createTestSession()
	host := &HostInfo{
		connectAddress: net.IPv4(127, 0, 0, 1),
		port:           9042,
	}

	pool := newHostConnPool(session, host, 9042, 10, "")

	// Add mix of expired and fresh connections
	now := time.Now()
	var expiredConns []*Conn
	pool.mu.Lock()
	for i := 0; i < 5; i++ {
		expiredConn := mockConn(3, now.Add(-2*time.Hour), 1*time.Hour)
		// Allocate a stream so they won't be closed immediately
		expiredConn.streams.GetStream()
		expiredConns = append(expiredConns, expiredConn)
		pool.conns = append(pool.conns, expiredConn)
	}
	for i := 0; i < 5; i++ {
		freshConn := mockConn(3, now.Add(-30*time.Minute), 1*time.Hour)
		pool.conns = append(pool.conns, freshConn)
	}
	pool.mu.Unlock()

	// Concurrent picks
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pool.Pick()
		}()
	}

	wg.Wait()

	// Wait for maintenance goroutine to move expired connections
	time.Sleep(1200 * time.Millisecond)

	pool.mu.Lock()
	defer pool.mu.Unlock()

	// All expired connections should be moved to draining (not closed due to active streams)
	if len(pool.expiredConns) != 5 {
		t.Errorf("Expected 5 connections in draining pool, got %d", len(pool.expiredConns))
	}

	// Only fresh connections should remain
	if len(pool.conns) != 5 {
		t.Errorf("Expected 5 connections in active pool, got %d", len(pool.conns))
	}
}

// TestConnectionExpiration_PoolClose tests that closing the pool closes all connections
func TestConnectionExpiration_PoolClose(t *testing.T) {
	session := createTestSession()
	host := &HostInfo{
		connectAddress: net.IPv4(127, 0, 0, 1),
		port:           9042,
	}

	pool := newHostConnPool(session, host, 9042, 5, "")

	// Add connections to both active and draining pools
	now := time.Now()
	activeConn := mockConn(3, now.Add(-30*time.Minute), 1*time.Hour)
	drainingConn := mockConn(3, now.Add(-2*time.Hour), 1*time.Hour)

	pool.mu.Lock()
	pool.conns = []*Conn{activeConn}
	pool.expiredConns = []*Conn{drainingConn}
	pool.mu.Unlock()

	// Close the pool
	pool.Close()

	pool.mu.Lock()
	defer pool.mu.Unlock()

	// Both pools should be empty
	if pool.conns != nil {
		t.Errorf("Expected active pool to be nil after close")
	}

	if pool.expiredConns != nil {
		t.Errorf("Expected draining pool to be nil after close")
	}

	// Pool should be marked as closed
	if !pool.closed {
		t.Errorf("Expected pool to be marked as closed")
	}
}

// TestConnectionExpiration_NoExpirationWhenDisabled tests that connections don't expire when ConnMaxLifetime is 0
func TestConnectionExpiration_NoExpirationWhenDisabled(t *testing.T) {
	session := createTestSession()
	host := &HostInfo{
		connectAddress: net.IPv4(127, 0, 0, 1),
		port:           9042,
	}

	pool := newHostConnPool(session, host, 9042, 5, "")

	// Create old connection but with ConnMaxLifetime disabled (0)
	now := time.Now()
	oldConn := mockConn(3, now.Add(-24*time.Hour), 0) // maxLifetime = 0 (disabled)

	pool.mu.Lock()
	pool.conns = []*Conn{oldConn}
	pool.mu.Unlock()

	// Pick a connection
	conn := pool.Pick()

	pool.mu.Lock()
	defer pool.mu.Unlock()

	// Should return the old connection (not expired)
	if conn != oldConn {
		t.Errorf("Expected to get old connection when expiration disabled")
	}

	// No connections should be in draining pool
	if len(pool.expiredConns) != 0 {
		t.Errorf("Expected 0 connections in draining pool, got %d", len(pool.expiredConns))
	}

	// Connection should still be in active pool
	if len(pool.conns) != 1 {
		t.Errorf("Expected 1 connection in active pool, got %d", len(pool.conns))
	}
}

// TestConnectionExpiration_PoolRefill tests that pool refills as connections expire
func TestConnectionExpiration_PoolRefill(t *testing.T) {
	// This test verifies that when connections are moved to draining,
	// the pool size drops below target and fill() is triggered
	session := createTestSession()
	host := &HostInfo{
		connectAddress: net.IPv4(127, 0, 0, 1),
		port:           9042,
	}

	poolSize := 3
	pool := newHostConnPool(session, host, 9042, poolSize, "")

	// Add expired connections with active streams so they stay in draining
	now := time.Now()
	pool.mu.Lock()
	for i := 0; i < poolSize; i++ {
		expiredConn := mockConn(3, now.Add(-2*time.Hour), 1*time.Hour)
		// Allocate a stream so they won't be closed immediately
		expiredConn.streams.GetStream()
		pool.conns = append(pool.conns, expiredConn)
	}
	pool.mu.Unlock()

	// Pick should start maintenance goroutine
	pool.Pick()

	// Wait for maintenance to move connections to draining
	time.Sleep(1200 * time.Millisecond)

	pool.mu.Lock()
	activeCount := len(pool.conns)
	drainingCount := len(pool.expiredConns)
	pool.mu.Unlock()

	// Active pool should be empty now (all moved to draining)
	if activeCount != 0 {
		t.Errorf("Expected active pool to be empty, got %d connections", activeCount)
	}

	// Draining pool should have all connections
	if drainingCount != poolSize {
		t.Errorf("Expected %d connections in draining pool, got %d", poolSize, drainingCount)
	}

	// Note: We can't easily test that fill() actually creates new connections
	// without a real server, but we can verify the pool size dropped
}

// TestConnectionExpiration_MultiplePickCycles tests repeated Pick() calls over time
func TestConnectionExpiration_MultiplePickCycles(t *testing.T) {
	session := createTestSession()
	host := &HostInfo{
		connectAddress: net.IPv4(127, 0, 0, 1),
		port:           9042,
	}

	pool := newHostConnPool(session, host, 9042, 10, "")
	// Set fast ticker for testing
	pool.maintenanceTicker = 100 * time.Millisecond

	// Add fresh connections
	now := time.Now()
	pool.mu.Lock()
	for i := 0; i < 5; i++ {
		freshConn := mockConn(3, now.Add(-30*time.Minute), 1*time.Hour)
		pool.conns = append(pool.conns, freshConn)
	}
	pool.mu.Unlock()

	// First pick - no expiration
	conn1 := pool.Pick()
	if conn1 == nil {
		t.Errorf("Expected to get a connection")
	}

	pool.mu.Lock()
	if len(pool.expiredConns) != 0 {
		t.Errorf("Expected no expired connections yet")
	}
	pool.mu.Unlock()

	// Manually expire some connections by changing their createdAt time
	pool.mu.Lock()
	for i := 0; i < 2; i++ {
		pool.conns[i].createdAt = now.Add(-2 * time.Hour)
		// Allocate a stream so they won't be closed immediately
		pool.conns[i].streams.GetStream()
	}
	pool.mu.Unlock()

	// Second pick - should start maintenance which will expire 2 connections
	conn2 := pool.Pick()
	if conn2 == nil {
		t.Errorf("Expected to get a connection")
	}

	// Wait for maintenance to move expired connections
	time.Sleep(200 * time.Millisecond)

	pool.mu.Lock()
	defer pool.mu.Unlock()

	if len(pool.expiredConns) != 2 {
		t.Errorf("Expected 2 expired connections, got %d", len(pool.expiredConns))
	}

	if len(pool.conns) != 3 {
		t.Errorf("Expected 3 active connections, got %d", len(pool.conns))
	}
}

// TestConnectionExpiration_StreamAvailabilityCheck tests the correct stream availability calculation
func TestConnectionExpiration_StreamAvailabilityCheck(t *testing.T) {
	// Test for protocol v3 (32768 streams)
	conn := mockConn(3, time.Now().Add(-2*time.Hour), 1*time.Hour)

	// All streams should be available initially (minus reserved stream 0)
	available := conn.AvailableStreams()
	expected := conn.streams.NumStreams - 1

	if available != expected {
		t.Errorf("Expected %d available streams, got %d", expected, available)
	}

	// Allocate some streams
	for i := 0; i < 10; i++ {
		conn.streams.GetStream()
	}

	// Should have 10 fewer available
	available = conn.AvailableStreams()
	expected = conn.streams.NumStreams - 1 - 10

	if available != expected {
		t.Errorf("Expected %d available streams after allocation, got %d", expected, available)
	}
}


// TestConnectionExpiration_RaceCondition tests for race conditions using Go's race detector
func TestConnectionExpiration_RaceCondition(t *testing.T) {
	session := createTestSession()
	host := &HostInfo{
		connectAddress: net.IPv4(127, 0, 0, 1),
		port:           9042,
	}

	pool := newHostConnPool(session, host, 9042, 10, "")

	// Add connections
	now := time.Now()
	pool.mu.Lock()
	for i := 0; i < 10; i++ {
		var conn *Conn
		if i%2 == 0 {
			conn = mockConn(3, now.Add(-2*time.Hour), 1*time.Hour)
		} else {
			conn = mockConn(3, now.Add(-30*time.Minute), 1*time.Hour)
		}
		pool.conns = append(pool.conns, conn)
	}
	pool.mu.Unlock()

	// Hammer the pool with concurrent operations
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Concurrent Pick() calls
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					pool.Pick()
					time.Sleep(10 * time.Millisecond)
				}
			}
		}()
	}

	// Concurrent Size() calls
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					pool.Size()
					time.Sleep(5 * time.Millisecond)
				}
			}
		}()
	}

	// Let it run for a bit
	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()

	// If we get here without race detector errors, we're good
}

// TestConnectionExpiration_DrainingGoroutineRestart tests that draining goroutine can restart
func TestConnectionExpiration_DrainingGoroutineRestart(t *testing.T) {
	session := createTestSession()
	host := &HostInfo{
		connectAddress: net.IPv4(127, 0, 0, 1),
		port:           9042,
	}

	pool := newHostConnPool(session, host, 9042, 5, "")

	pool.maintenanceMu.Lock()
	if !pool.maintenanceStarted {
		t.Error("Expected maintenance to be started")
	}
}

// TestConnectionExpiration_ClosedPoolStopsDraining tests that closing pool stops draining goroutine
func TestConnectionExpiration_ClosedPoolStopsDraining(t *testing.T) {
	session := createTestSession()
	host := &HostInfo{
		connectAddress: net.IPv4(127, 0, 0, 1),
		port:           9042,
	}

	pool := newHostConnPool(session, host, 9042, 5, "")

	// Add expired connection with active streams
	now := time.Now()
	expiredConn := mockConn(3, now.Add(-2*time.Hour), 1*time.Hour)
	expiredConn.streams.GetStream() // Allocate a stream so it won't close immediately

	pool.mu.Lock()
	pool.conns = []*Conn{expiredConn}
	pool.mu.Unlock()

	// Verify maintenance started
	pool.maintenanceMu.Lock()
	if !pool.maintenanceStarted {
		t.Errorf("Expected maintenance goroutine to be started")
	}
	pool.maintenanceMu.Unlock()

	// Close the pool
	pool.Close()

	// Wait a bit for goroutine to exit
	time.Sleep(1500 * time.Millisecond)

	// Maintenance goroutine should have stopped
	pool.maintenanceMu.Lock()
	defer pool.maintenanceMu.Unlock()

	if pool.maintenanceStarted {
		t.Errorf("Expected maintenance goroutine to stop when pool is closed")
	}
}

// BenchmarkPick benchmarks the Pick() method
func BenchmarkPick(b *testing.B) {
	session := createTestSession()
	host := &HostInfo{
		connectAddress: net.IPv4(127, 0, 0, 1),
		port:           9042,
	}

	pool := newHostConnPool(session, host, 9042, 10, "")

	// Add fresh connections
	now := time.Now()
	pool.mu.Lock()
	for i := 0; i < 10; i++ {
		freshConn := mockConn(3, now.Add(-30*time.Minute), 1*time.Hour)
		pool.conns = append(pool.conns, freshConn)
	}
	pool.mu.Unlock()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			pool.Pick()
		}
	})
}

// BenchmarkPickWithExpiration benchmarks Pick() with some expired connections
func BenchmarkPickWithExpiration(b *testing.B) {
	session := createTestSession()
	host := &HostInfo{
		connectAddress: net.IPv4(127, 0, 0, 1),
		port:           9042,
	}

	pool := newHostConnPool(session, host, 9042, 10, "")

	// Add mix of fresh and expired connections
	now := time.Now()
	pool.mu.Lock()
	for i := 0; i < 7; i++ {
		freshConn := mockConn(3, now.Add(-30*time.Minute), 1*time.Hour)
		pool.conns = append(pool.conns, freshConn)
	}
	for i := 0; i < 3; i++ {
		expiredConn := mockConn(3, now.Add(-2*time.Hour), 1*time.Hour)
		pool.conns = append(pool.conns, expiredConn)
	}
	pool.mu.Unlock()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pool.Pick()
	}
}

// TestMaintenanceGoroutine_ConfigurableInterval demonstrates using a custom ticker interval for testing
func TestMaintenanceGoroutine_ConfigurableInterval(t *testing.T) {
	session := createTestSession()
	host := &HostInfo{
		connectAddress: net.IPv4(127, 0, 0, 1),
		port:           9042,
	}

	// Create pool but don't start maintenance goroutine yet
	pool := &hostConnPool{
		session:            session,
		host:               host,
		port:               9042,
		size:               5,
		conns:              make([]*Conn, 0, 5),
		expiredConns:       make([]*Conn, 0, maxDrainingConns),
		filling:            false,
		closed:             false,
		maintenanceStarted: false,
		maintenanceTicker:  100 * time.Millisecond, // Fast ticker for testing (100ms instead of 30s)
	}

	// Add an expired connection
	now := time.Now()
	expiredConn := mockConn(3, now.Add(-2*time.Hour), 1*time.Hour)

	pool.mu.Lock()
	pool.conns = []*Conn{expiredConn}
	pool.mu.Unlock()

	// Now start the maintenance goroutine with the fast ticker
	pool.startMaintenanceGoroutine()

	// Wait for the fast ticker to run (much shorter than 30s!)
	// First run is immediate, then wait for one more tick to ensure it processes
	time.Sleep(200 * time.Millisecond)

	pool.mu.Lock()
	defer pool.mu.Unlock()

	// Expired connection should be closed
	if !expiredConn.Closed() {
		t.Errorf("Expected expired connection to be closed")
	}

	// Active pool should be empty
	if len(pool.conns) != 0 {
		t.Errorf(
			"Expected 0 connections in active pool, got %d",
			len(pool.conns),
		)
	}
}

