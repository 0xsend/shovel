package shovel

import (
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	ConsensusAttempts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "shovel_consensus_attempts_total",
		Help: "Number of consensus evaluations per block range",
	}, []string{"src_name", "ig_name"})

	ConsensusFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "shovel_consensus_failures_total",
		Help: "Consensus attempts that failed to reach threshold",
	}, []string{"src_name", "ig_name"})

	ConsensusDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "shovel_consensus_duration_seconds",
		Help:    "Time taken to reach consensus for a block",
		Buckets: prometheus.DefBuckets,
	}, []string{"src_name", "ig_name"})

	ProviderErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "shovel_provider_error_total",
		Help: "RPC errors per provider",
	}, []string{"provider"})

	CircuitBreakerState = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "shovel_circuit_breaker_state",
		Help: "Circuit breaker state per provider (0=closed, 1=open, 2=half_open)",
	}, []string{"src_name", "ig_name", "provider"})

	CircuitBreakerTransitions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "shovel_circuit_breaker_transitions_total",
		Help: "Circuit breaker state transitions",
	}, []string{"src_name", "ig_name", "provider", "from_state", "to_state"})
)

type Metrics struct {
	start time.Time
	src   string
	ig    string
	once  sync.Once
}

func NewMetrics(src, ig string) *Metrics {
	return &Metrics{src: src, ig: ig}
}

func (m *Metrics) Start() {
	m.start = time.Now()
	ConsensusAttempts.WithLabelValues(m.src, m.ig).Inc()
}

func (m *Metrics) Failure() {
	ConsensusFailures.WithLabelValues(m.src, m.ig).Inc()
}

func (m *Metrics) ProviderError(p string) {
	ProviderErrors.WithLabelValues(p).Inc()
	slog.Warn("provider error", "p", p)
}

func (m *Metrics) Stop() {
	m.once.Do(func() {
		ConsensusDuration.WithLabelValues(m.src, m.ig).Observe(time.Since(m.start).Seconds())
	})
}

func (m *Metrics) CircuitBreakerTransition(provider, fromState, toState string) {
	CircuitBreakerTransitions.WithLabelValues(m.src, m.ig, provider, fromState, toState).Inc()
}

func (m *Metrics) CircuitBreakerStateChange(provider string, state float64) {
	CircuitBreakerState.WithLabelValues(m.src, m.ig, provider).Set(state)
}
