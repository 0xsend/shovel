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
	}, []string{"src_name"})

	ConsensusFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "shovel_consensus_failures_total",
		Help: "Consensus attempts that failed to reach threshold",
	}, []string{"src_name"})

	ConsensusDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "shovel_consensus_duration_seconds",
		Help:    "Time taken to reach consensus for a block",
		Buckets: prometheus.DefBuckets,
	}, []string{"src_name"})

	ProviderErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "shovel_provider_error_total",
		Help: "RPC errors per provider",
	}, []string{"provider"})

	ReceiptMismatch = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "shovel_receipt_mismatch_total",
		Help: "Number of receipt validation mismatches",
	}, []string{"src_name"})

	AuditVerifications = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "shovel_audit_verifications_total",
		Help: "Confirmation audits executed",
	}, []string{"src_name", "status"})

	AuditQueueLength = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "shovel_audit_queue_length",
		Help: "Number of confirmed blocks pending audit",
	}, []string{"src_name"})

	AuditBacklogAge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "shovel_audit_backlog_age_seconds",
		Help: "Age in seconds of the oldest block pending audit for this source",
	}, []string{"src_name"})
)

type Metrics struct {
	start time.Time
	src   string
	once  sync.Once
}

func NewMetrics(src string) *Metrics {
	return &Metrics{src: src}
}

func (m *Metrics) Start() {
	m.start = time.Now()
	ConsensusAttempts.WithLabelValues(m.src).Inc()
}

func (m *Metrics) Failure() {
	ConsensusFailures.WithLabelValues(m.src).Inc()
}

func (m *Metrics) ProviderError(p string) {
	ProviderErrors.WithLabelValues(p).Inc()
	slog.Warn("provider error", "p", p)
}

func (m *Metrics) ReceiptMismatch() {
	ReceiptMismatch.WithLabelValues(m.src).Inc()
}

func (m *Metrics) Stop() {
	m.once.Do(func() {
		ConsensusDuration.WithLabelValues(m.src).Observe(time.Since(m.start).Seconds())
	})
}
