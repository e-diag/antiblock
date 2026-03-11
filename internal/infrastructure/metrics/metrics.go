package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	ActiveFreeProxies = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "antiblock_free_proxies_active",
		Help: "Number of active free proxies",
	})
	InactiveFreeProxies = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "antiblock_free_proxies_inactive",
		Help: "Number of inactive free proxies",
	})
	ActivePremiumProxies = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "antiblock_premium_proxies_active",
		Help: "Number of active premium proxies",
	})
	UnreachablePremiumProxies = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "antiblock_premium_proxies_unreachable",
		Help: "Number of unreachable premium proxies",
	})
	TotalUsers = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "antiblock_users_total",
		Help: "Total number of users",
	})
	PremiumUsers = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "antiblock_users_premium",
		Help: "Number of active premium users",
	})
	FreeProxyLoad = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "antiblock_free_proxy_load",
		Help: "Current user load per free proxy",
	}, []string{"ip", "port"})
)
