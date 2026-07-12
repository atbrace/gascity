//go:build !productmetrics_testhook

package main

func configuredPrivateProductMetricsRunner() privateProductMetricsRunFunc {
	return runProductionProductMetricsChild
}
