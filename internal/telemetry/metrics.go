package telemetry

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	signalsTotal     *prometheus.CounterVec
	ordersTotal      *prometheus.CounterVec
	positionChanges  *prometheus.CounterVec
	stateTransitions *prometheus.CounterVec
	dataErrors       *prometheus.CounterVec

	positionQty     prometheus.Gauge
	unrealizedPnLPct prometheus.Gauge
	btcCorr1m       prometheus.Gauge
	basisZScore     prometheus.Gauge
	currentState    prometheus.Gauge

	signalToOrderLatency prometheus.Histogram
	orderFillLatency     prometheus.Histogram
	slippageBps          prometheus.Histogram
}

func NewMetrics() *Metrics {
	m := &Metrics{
		signalsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "eth_signals_generated_total",
		}, []string{"engine", "direction"}),
		ordersTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "eth_orders_submitted_total",
		}, []string{"type", "result"}),
		positionChanges: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "eth_position_changes_total",
		}, []string{"action"}),
		stateTransitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "eth_state_transitions_total",
		}, []string{"from", "to"}),
		dataErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "eth_data_errors_total",
		}, []string{"source", "type"}),

		positionQty: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "eth_position_quantity",
		}),
		unrealizedPnLPct: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "eth_unrealized_pnl_pct",
		}),
		btcCorr1m: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "eth_btc_correlation_1m",
		}),
		basisZScore: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "eth_basis_zscore",
		}),
		currentState: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "eth_current_state",
		}),

		signalToOrderLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "eth_signal_to_order_latency_ms",
			Buckets: []float64{1, 5, 10, 25, 50, 100, 200, 500},
		}),
		orderFillLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "eth_order_fill_latency_ms",
			Buckets: []float64{10, 50, 100, 250, 500, 1000, 3000},
		}),
		slippageBps: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "eth_slippage_bps_actual",
			Buckets: []float64{0.5, 1, 2, 5, 10, 20, 50},
		}),
	}

	prometheus.MustRegister(
		m.signalsTotal, m.ordersTotal, m.positionChanges,
		m.stateTransitions, m.dataErrors,
		m.positionQty, m.unrealizedPnLPct, m.btcCorr1m, m.basisZScore, m.currentState,
		m.signalToOrderLatency, m.orderFillLatency, m.slippageBps,
	)
	return m
}

func (m *Metrics) SignalGenerated(engineName, direction string) {
	m.signalsTotal.WithLabelValues(engineName, direction).Inc()
}

func (m *Metrics) OrderSubmitted(orderType, result string) {
	m.ordersTotal.WithLabelValues(orderType, result).Inc()
}

func (m *Metrics) PositionChanged(action string) {
	m.positionChanges.WithLabelValues(action).Inc()
}

func (m *Metrics) StateTransition(from, to string) {
	m.stateTransitions.WithLabelValues(from, to).Inc()
}

func (m *Metrics) DataError(source, errType string) {
	m.dataErrors.WithLabelValues(source, errType).Inc()
}

func (m *Metrics) SetPositionQty(qty float64)       { m.positionQty.Set(qty) }
func (m *Metrics) SetPnLPct(pct float64)            { m.unrealizedPnLPct.Set(pct) }
func (m *Metrics) SetBTCCorr(corr float64)          { m.btcCorr1m.Set(corr) }
func (m *Metrics) SetBasisZScore(z float64)         { m.basisZScore.Set(z) }
func (m *Metrics) SetState(state float64)           { m.currentState.Set(state) }
func (m *Metrics) ObserveSignalLatency(ms float64)  { m.signalToOrderLatency.Observe(ms) }
func (m *Metrics) ObserveFillLatency(ms float64)    { m.orderFillLatency.Observe(ms) }
func (m *Metrics) ObserveSlippage(bps float64)      { m.slippageBps.Observe(bps) }

func (m *Metrics) ServeHTTP(addr string) {
	http.Handle("/metrics", promhttp.Handler())
	go http.ListenAndServe(addr, nil) //nolint:errcheck
}
