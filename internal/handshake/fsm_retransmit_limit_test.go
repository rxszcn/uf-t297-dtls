// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package dtlshandshake

import (
	"context"
	"testing"
	"time"

	dtlsconfig "github.com/pion/dtls/v4/internal/config"
	dtlsflight "github.com/pion/dtls/v4/internal/flight"
	dtlsflight12 "github.com/pion/dtls/v4/internal/flight/flight12"
	dtlsflight13 "github.com/pion/dtls/v4/internal/flight/flight13"
	dtlsstate "github.com/pion/dtls/v4/internal/state"
	"github.com/pion/dtls/v4/pkg/protocol"
	"github.com/pion/dtls/v4/pkg/protocol/handshake"
	"github.com/pion/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// instantTimerFactory fires every handshake timer immediately, so retransmit
// timeouts can be driven without waiting for the real backoff schedule.
func instantTimerFactory(time.Duration) dtlsconfig.Timer {
	channel := make(chan time.Time, 1)
	channel <- time.Now()

	return &handshakeTestTimer{channel: channel}
}

func neverTimerFactory(time.Duration) dtlsconfig.Timer {
	return &handshakeTestTimer{channel: make(chan time.Time)}
}

func retransmitLimitTestConfig(t *testing.T, attempts int) *dtlsconfig.HandshakeConfig {
	t.Helper()

	return &dtlsconfig.HandshakeConfig{
		InitialRetransmitInterval: time.Second,
		MaxRetransmitAttempts:     attempts,
		TimerFactory:              instantTimerFactory,
		Log:                       logging.NewDefaultLoggerFactory().NewLogger("dtls"),
	}
}

type retransmitLimitFSM struct {
	wait      func(ctx context.Context, conn Conn) (State, error)
	interval  func() time.Duration
	attempts  func() int
	setAttemt func(int)
}

func newRetransmitLimitFSM12(cfg *dtlsconfig.HandshakeConfig) retransmitLimitFSM {
	state := dtlsstate.NewState12(true)
	fsm := &fsm12{
		state: &state, cache: dtlsflight.NewCache(), cfg: cfg,
		currentFlight: dtlsflight12.Flight1, retransmit: true,
		retransmitInterval: cfg.InitialRetransmitInterval,
	}

	return retransmitLimitFSM{
		wait:      fsm.wait,
		interval:  func() time.Duration { return fsm.retransmitInterval },
		attempts:  func() int { return fsm.retransmitAttempts },
		setAttemt: func(n int) { fsm.retransmitAttempts = n },
	}
}

func newRetransmitLimitFSM13(cfg *dtlsconfig.HandshakeConfig) retransmitLimitFSM {
	state := dtlsstate.NewState13(true)
	fsm := &fsm13{
		currentFlight: dtlsflight13.Flight1, retransmit: true,
		retransmitInterval: cfg.InitialRetransmitInterval,
		handshakeContext:   handshakeContext{state: &state, cache: dtlsflight.NewCache(), cfg: cfg},
		closed:             make(chan struct{}),
	}

	return retransmitLimitFSM{
		wait:      fsm.wait,
		interval:  func() time.Duration { return fsm.retransmitInterval },
		attempts:  func() int { return fsm.retransmitAttempts },
		setAttemt: func(n int) { fsm.retransmitAttempts = n },
	}
}

func retransmitLimitFSMs() map[string]func(*dtlsconfig.HandshakeConfig) retransmitLimitFSM {
	return map[string]func(*dtlsconfig.HandshakeConfig) retransmitLimitFSM{
		"1.2": newRetransmitLimitFSM12,
		"1.3": newRetransmitLimitFSM13,
	}
}

// TestHandshakeFSMRetransmitLimitExceeded drives a silent peer through the
// default retransmission budget: the backoff curve keeps its existing shape
// and the handshake ends with a classifiable terminal error once the budget
// is spent.
func TestHandshakeFSMRetransmitLimitExceeded(t *testing.T) {
	for name, newFSM := range retransmitLimitFSMs() {
		t.Run(name, func(t *testing.T) {
			cfg := retransmitLimitTestConfig(t, dtlsconfig.DefaultMaxRetransmitAttempts)
			fsm := newFSM(cfg)
			conn := &flightTestConn{}

			interval := cfg.InitialRetransmitInterval
			for attempt := 1; attempt <= dtlsconfig.DefaultMaxRetransmitAttempts; attempt++ {
				next, err := fsm.wait(context.Background(), conn)
				require.NoError(t, err)
				require.Equal(t, StateSending, next)
				interval = min(interval, 30*time.Second) * 2
				assert.Equal(t, interval, fsm.interval(), "backoff curve must keep its existing pace")
				assert.Equal(t, attempt, fsm.attempts())
			}

			next, err := fsm.wait(context.Background(), conn)
			require.ErrorIs(t, err, ErrRetransmitLimitExceeded)
			require.Equal(t, StateErrored, next)
			assert.NotErrorIs(t, err, context.Canceled)
			assert.NotErrorIs(t, err, context.DeadlineExceeded)
		})
	}
}

// TestHandshakeFSMRetransmitLimitDisabled restores the previous unbounded
// retransmission behavior with an explicitly zero budget.
func TestHandshakeFSMRetransmitLimitDisabled(t *testing.T) {
	for name, newFSM := range retransmitLimitFSMs() {
		t.Run(name, func(t *testing.T) {
			cfg := retransmitLimitTestConfig(t, 0)
			fsm := newFSM(cfg)
			conn := &flightTestConn{}

			for range dtlsconfig.DefaultMaxRetransmitAttempts + 8 {
				next, err := fsm.wait(context.Background(), conn)
				require.NoError(t, err)
				require.Equal(t, StateSending, next)
			}
			assert.Equal(t, 0, fsm.attempts())
		})
	}
}

// TestHandshakeFSMRetransmitLimitWaitCancellation verifies that a configured
// timeout or cancellation still ends the wait before the retransmission
// budget is involved.
func TestHandshakeFSMRetransmitLimitWaitCancellation(t *testing.T) {
	for name, newFSM := range retransmitLimitFSMs() {
		t.Run(name, func(t *testing.T) {
			cfg := retransmitLimitTestConfig(t, 2)
			cfg.TimerFactory = neverTimerFactory
			fsm := newFSM(cfg)
			conn := &flightTestConn{}

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			next, err := fsm.wait(ctx, conn)
			require.ErrorIs(t, err, context.Canceled)
			require.NotErrorIs(t, err, ErrRetransmitLimitExceeded)
			assert.Equal(t, StateErrored, next)
			assert.Equal(t, cfg.InitialRetransmitInterval, fsm.interval())
			assert.Equal(t, 0, fsm.attempts())

			deadlineCtx, deadlineCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer deadlineCancel()
			next, err = fsm.wait(deadlineCtx, conn)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.NotErrorIs(t, err, ErrRetransmitLimitExceeded)
			assert.Equal(t, StateErrored, next)
		})
	}
}

// TestHandshakeFSM12RetransmitLimitResetsOnPeerRecovery verifies that a peer
// recovering from a silent stretch resets the budget: after a HelloVerifyRequest
// arrives, the full retransmission budget is available again.
func TestHandshakeFSM12RetransmitLimitResetsOnPeerRecovery(t *testing.T) {
	cfg := retransmitLimitTestConfig(t, 3)
	cache := dtlsflight.NewCache()
	state := dtlsstate.NewState12(true)
	fsm := &fsm12{
		state: &state, cache: cache, cfg: cfg,
		currentFlight: dtlsflight12.Flight1, retransmit: true,
		retransmitInterval: cfg.InitialRetransmitInterval,
	}
	conn := &flightTestConn{recvHandshake: make(chan RecvHandshakeState, 1)}

	// The peer is silent: two retransmissions are charged against the budget.
	for range 2 {
		next, err := fsm.wait(context.Background(), conn)
		require.NoError(t, err)
		require.Equal(t, StateSending, next)
	}
	require.Equal(t, 2, fsm.retransmitAttempts)

	// The peer recovers and answers with a HelloVerifyRequest. Hold the
	// retransmit timer so the receive branch wins the select.
	cfg.TimerFactory = neverTimerFactory
	raw, err := (&handshake.Handshake{
		Header:  handshake.Header{MessageSequence: 0},
		Message: &handshake.MessageHelloVerifyRequest{Version: protocol.Version1_2, Cookie: []byte{0x01}},
	}).Marshal()
	require.NoError(t, err)
	cache.Push(raw, 0, 0, handshake.TypeHelloVerifyRequest, false)
	received := RecvHandshakeState{Done: make(chan struct{})}
	conn.recvHandshake <- received
	next, err := fsm.wait(context.Background(), conn)
	require.NoError(t, err)
	require.Equal(t, StatePreparing, next)
	require.Equal(t, dtlsflight12.Flight3, fsm.currentFlight)
	assert.Zero(t, fsm.retransmitAttempts, "peer progress must reset the retransmission budget")
	fsm.received.release()
	cfg.TimerFactory = instantTimerFactory

	// The full budget is available again before the terminal error.
	for attempt := 1; attempt <= 3; attempt++ {
		next, err = fsm.wait(context.Background(), conn)
		require.NoError(t, err)
		require.Equal(t, StateSending, next)
		assert.Equal(t, attempt, fsm.retransmitAttempts)
	}
	next, err = fsm.wait(context.Background(), conn)
	require.ErrorIs(t, err, ErrRetransmitLimitExceeded)
	require.Equal(t, StateErrored, next)
}

// TestHandshakeFSM13RetransmitLimitResetsOnPeerRecovery verifies that
// preparing the next flight after a peer response resets the budget.
func TestHandshakeFSM13RetransmitLimitResetsOnPeerRecovery(t *testing.T) {
	cfg := testHandshakeConfig13(t)
	cfg.MaxRetransmitAttempts = 3
	state := newTestState13(t, true)
	fsm, err := newFSM13(state, dtlsflight.NewCache(), cfg, dtlsflight13.Flight1, nil, nil)
	require.NoError(t, err)

	fsm.retransmitAttempts = 2
	next, err := fsm.prepare(context.Background(), &flightTestConn{})
	require.NoError(t, err)
	require.Equal(t, StateSending, next)
	assert.Zero(t, fsm.retransmitAttempts, "peer progress must reset the retransmission budget")
}
