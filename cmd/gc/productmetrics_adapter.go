package main

import (
	"context"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/gchome"
	"github.com/gastownhall/gascity/internal/productmetrics"
)

const (
	privateProductMetricsFailureExitCode   = 1
	privateProductMetricsMarkerEnvironment = "GC_PRODUCT_METRICS_PRIVATE_UPLOADER"
	privateProductMetricsMarkerValue       = "1"
	productMetricsControlDeadline          = 12 * time.Second
)

type (
	productMetricsEffectiveState    = productmetrics.EffectiveState
	productMetricsStateReason       = productmetrics.StateReason
	productMetricsStatus            = productmetrics.Status
	productMetricsPolicyMetadata    = productmetrics.PolicyMetadata
	productMetricsInvocationContext = productmetrics.InvocationContext
	productMetricsPurgeResult       = productmetrics.PurgeResult
	productMetricsPurgeError        = productmetrics.PurgeError
	productMetricsPurgeClass        = productmetrics.PurgeErrorClass
)

const (
	productMetricsPurgeCompleted                    = productmetrics.PurgeCompleted
	productMetricsPurgeAlreadyDisabled              = productmetrics.PurgeAlreadyDisabled
	productMetricsPurgeErrorInvalidRequest          = productmetrics.PurgeErrorInvalidRequest
	productMetricsPurgeErrorDisableWrite            = productmetrics.PurgeErrorDisableWrite
	productMetricsPurgeErrorUploaderQuiescence      = productmetrics.PurgeErrorUploaderQuiescence
	productMetricsPurgeErrorCleanupIncomplete       = productmetrics.PurgeErrorCleanupIncomplete
	productMetricsPurgeErrorStateChanged            = productmetrics.PurgeErrorStateChanged
	productMetricsPurgeErrorStorage                 = productmetrics.PurgeErrorStorage
	productMetricsPurgeIncompleteDisableWrite       = productmetrics.PurgeIncompleteDisableWrite
	productMetricsPurgeIncompleteUploaderQuiescence = productmetrics.PurgeIncompleteUploaderQuiescence
	productMetricsPurgeIncompleteLocalCleanup       = productmetrics.PurgeIncompleteLocalCleanup
	productMetricsPurgeIncompleteFinalProof         = productmetrics.PurgeIncompleteFinalProof
	productMetricsPurgeManualUnsettledJournal       = productmetrics.PurgeManualCleanupUnsettledRootTempJournal
	productMetricsPurgeManualUnrecognizedEntry      = productmetrics.PurgeManualCleanupUnrecognizedRootEntry
)

type (
	privateProductMetricsRunFunc    func(context.Context, productmetrics.PrivateUploaderInvocation) error
	privateProductMetricsRunFactory func() privateProductMetricsRunFunc
)

type productMetricsControlService interface {
	Status(context.Context) productMetricsStatus
	PolicyMetadata() productMetricsPolicyMetadata
	InstallationIDForDisclosure(context.Context) (string, bool)
	Enable(context.Context, productMetricsInvocationContext, io.Writer) error
	DisableAndPurge(context.Context) (productMetricsPurgeResult, error)
	RecordingPermit(productmetrics.InvocationContext) productmetrics.RecordingPermit
	RecordOnce(productmetrics.RecordingPermit, productmetrics.CommandID) productmetrics.RecordResult
}

var privateProductMetricsRunnerFactory privateProductMetricsRunFactory = configuredPrivateProductMetricsRunner

var productMetricsControlServiceFactory = func() (productMetricsControlService, error) {
	return configuredProductMetricsControlService()
}

func productMetricsExplicitEnableInvocation() productMetricsInvocationContext {
	managed := false
	for _, key := range []string{
		"GC_SESSION_ID",
		"GC_SESSION_NAME",
		"GC_AGENT",
		"GC_TEMPLATE",
		"GC_MANAGED_SESSION_HOOK",
		"GC_HOOK_EVENT_NAME",
		"BEADS_ACTOR",
	} {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			managed = true
			break
		}
	}
	return productMetricsInvocationContext{
		DoNotTrack:          os.Getenv("DO_NOT_TRACK"),
		DisableUsageMetrics: os.Getenv("GC_DISABLE_USAGE_METRICS"),
		ManagedAutomation:   managed,
		NoticeEligible:      true,
	}
}

func productMetricsControlContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), productMetricsControlDeadline)
}

func encodeProductMetricsExampleBatch() ([]byte, error) {
	return productmetrics.EncodeBatch(productmetrics.ExampleBatch())
}

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
	service, err := configuredProductMetricsControlService()
	if err != nil {
		return err
	}
	return service.RunPrivateUploader(ctx, invocation)
}

func openProductionProductMetricsService() (*productmetrics.Service, error) {
	return productmetrics.OpenProduction(productmetrics.ProductionOptions{
		Home:    gchome.ResolveReadOnly(),
		Release: productmetrics.CurrentReleaseIdentity(),
	})
}
