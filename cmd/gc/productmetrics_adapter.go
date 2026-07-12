package main

import (
	"context"
	"os"
	"runtime"

	"github.com/gastownhall/gascity/internal/gchome"
	"github.com/gastownhall/gascity/internal/productmetrics"
)

const (
	privateProductMetricsFailureExitCode   = 1
	privateProductMetricsMarkerEnvironment = "GC_PRODUCT_METRICS_PRIVATE_UPLOADER"
	privateProductMetricsMarkerValue       = "1"
)

type (
	privateProductMetricsRunFunc    func(context.Context, productmetrics.PrivateUploaderInvocation) error
	privateProductMetricsRunFactory func() privateProductMetricsRunFunc
)

var privateProductMetricsRunnerFactory privateProductMetricsRunFactory = configuredPrivateProductMetricsRunner

func privateProductMetricsEntrypoint(args []string) (handled bool, code int) {
	return privateProductMetricsEntrypointForPlatform(args, runtime.GOOS)
}

func privateProductMetricsEntrypointForPlatform(args []string, goos string) (handled bool, code int) {
	invocation, detected, err := productmetrics.ParsePrivateUploaderInvocation(args)
	if !detected {
		return false, 0
	}
	if err != nil {
		return true, privateProductMetricsFailureExitCode
	}
	// Gate before the selected runner: tagged runners may open test trust files
	// while constructing their service. RunPrivateUploader repeats this exact
	// marker check as defense in depth before touching storage or the network.
	if os.Getenv(privateProductMetricsMarkerEnvironment) != privateProductMetricsMarkerValue {
		return true, privateProductMetricsFailureExitCode
	}
	if !privateProductMetricsPlatformSupported(goos) {
		return true, privateProductMetricsFailureExitCode
	}
	if privateProductMetricsRunnerFactory == nil {
		return true, privateProductMetricsFailureExitCode
	}
	runner := privateProductMetricsRunnerFactory()
	if runner == nil {
		return true, privateProductMetricsFailureExitCode
	}
	if err := runner(context.Background(), invocation); err != nil {
		return true, privateProductMetricsFailureExitCode
	}
	return true, 0
}

func privateProductMetricsPlatformSupported(goos string) bool {
	return goos == "linux" || goos == "darwin"
}

func runProductionProductMetricsChild(ctx context.Context, invocation productmetrics.PrivateUploaderInvocation) error {
	service, err := productmetrics.OpenProduction(productmetrics.ProductionOptions{
		Home:    gchome.ResolveReadOnly(),
		Release: productmetrics.CurrentReleaseIdentity(),
	})
	if err != nil {
		return err
	}
	return service.RunPrivateUploader(ctx, invocation)
}
