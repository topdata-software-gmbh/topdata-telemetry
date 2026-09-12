package monitor

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var shopsTotal = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "topdata_telemetry_shops_total",
	Help: "Total number of Shopware shops currently monitored by the agent",
})
