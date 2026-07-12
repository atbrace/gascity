package productmetrics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	queueDirectoryName    = "queue"
	inflightDirectoryName = "inflight"
	eventFileSuffix       = ".json"

	maximumEnumerationEvents        = uint64(5001)
	maximumCleanupEntries           = uint64(6000)
	maximumCleanupDirectories       = uint64(512)
	maximumCleanupReadBytes         = uint64(5 * 1024 * 1024)
	maximumCleanupNameBytes         = uint64(1024 * 1024)
	maximumFilesystemName           = uint64(255)
	spoolTraversalDirectoryEnvelope = uint64(2)
	spoolFixedDirectoryReserve      = uint64(1)
	spoolFileDescriptorHeadroom     = uint64(4)
	spoolFallbackDirectoryLimit     = uint64(32)
	// Two traversal envelopes let a bounded pass reach one nested child and
	// mutate it; the fixed slot preserves fail-closed control work.
	spoolMinimumDirectoryProgress = spoolFixedDirectoryReserve + 2*spoolTraversalDirectoryEnvelope
	// Bound every named operation in one fixed workflow conservatively: four
	// name operations per possible temp attempt, 32 per relocation candidate,
	// and 64 for control/quota/cursor/replay/retirement/cleanup bookkeeping.
	// This is intentionally larger than today's exact call count so a storage
	// implementation detail cannot silently escape the shared cap.
	spoolFixedEntryEnvelope = uint64(4*maximumStorageTempAttempts + 32*maximumRelocationSlots + 64)
	spoolFixedNameEnvelope  = spoolFixedEntryEnvelope * maximumStorageNameBytes
	// The relocation envelope covers quota read + conflict replay + quota
	// stage, followed by the control and root-fallback cursor read/stage pairs.
	// Writes share the same byte dimension as reads so neither direction can
	// escape the cap.
	spoolFixedReadEnvelope = uint64(3*maximumQuotaBytes + 4*maximumRelocationBytes + 4)

	defaultRecordDecisionBudget = 50 * time.Millisecond
	canonicalHourLayout         = "2006-01-02T15:04:05Z"
)

// RecordResult is the deliberately small outcome of a best-effort recording
// attempt. Metrics failures never surface as command failures.
type RecordResult uint8

const (
	// RecordDropped means the first attempt was ineligible or could not be
	// made durable within the fixed bounds.
	RecordDropped RecordResult = iota
	// RecordStored means exactly one immutable event file is durable.
	RecordStored
)

type recordDecisionWindow struct {
	started time.Time
	now     func() time.Time
	limit   time.Duration
}

type recordOperation string

const (
	recordOperationQuotaRead      recordOperation = "quota-read"
	recordOperationControlOpen    recordOperation = "control-open"
	recordOperationQuotaStage     recordOperation = "quota-stage"
	recordOperationQuotaInstall   recordOperation = "quota-install"
	recordOperationQuotaReplay    recordOperation = "quota-replay-read"
	recordOperationQuotaSync      recordOperation = "quota-replay-sync"
	recordOperationStageCleanup   recordOperation = "quota-stage-cleanup"
	recordOperationControlClean   recordOperation = "control-cleanup"
	recordOperationControlRemove  recordOperation = "control-remove"
	recordOperationQueueOpen      recordOperation = "queue-open"
	recordOperationGenerationOpen recordOperation = "generation-open"
	recordOperationEventWrite     recordOperation = "event-write"
)

func recordLookupOperation(name string) recordOperation {
	return recordOperation("lookup:" + name)
}

func (window recordDecisionWindow) remaining() (time.Duration, bool) {
	current := window.now()
	if current.Before(window.started) {
		return 0, false
	}
	elapsed := current.Sub(window.started)
	if elapsed < 0 || elapsed >= window.limit {
		return 0, false
	}
	return window.limit - elapsed, true
}

func depsHourUTC(value time.Time) string {
	return value.UTC().Truncate(time.Hour).Format(canonicalHourLayout)
}

func parseCanonicalHourUTC(value string) (time.Time, error) {
	parsed, err := time.Parse(canonicalHourLayout, value)
	if err != nil || parsed.Format(canonicalHourLayout) != value || parsed.Minute() != 0 || parsed.Second() != 0 || parsed.Nanosecond() != 0 {
		return time.Time{}, errors.New("productmetrics: occurrence is not a canonical UTC hour")
	}
	return parsed, nil
}

func operatingSystemForRuntime() OperatingSystem {
	switch runtime.GOOS {
	case "linux":
		return OSLinux
	case "darwin":
		return OSDarwin
	default:
		return ""
	}
}

// RecordOnce consumes the invocation's first recording attempt regardless of
// whether it succeeds. It revalidates the exact config record under state.lock,
// durably reserves root-global quota, and then installs one immutable file.
func (service *Service) RecordOnce(permit RecordingPermit, commandID CommandID) RecordResult {
	if service == nil || !service.recordAttempt.CompareAndSwap(false, true) {
		return RecordDropped
	}
	if !permit.Valid() || permit.releaseVersion != service.deps.release.releaseVersion ||
		permit.metricsEpoch != service.deps.release.metricsEpoch ||
		permit.operatingSystem == "" || permit.operatingSystem != operatingSystemForRuntime() {
		return RecordDropped
	}
	if _, err := commandIDWire(commandID, productionCommandIDCatalog); err != nil {
		return RecordDropped
	}
	occurred, err := parseCanonicalHourUTC(permit.occurredHourUTC)
	if err != nil {
		return RecordDropped
	}

	started := service.deps.now()
	if !eventWithinRetention(occurred, started) {
		return RecordDropped
	}
	window := recordDecisionWindow{started: started, now: service.deps.now, limit: defaultRecordDecisionBudget}
	eventID, err := service.deps.newUUID()
	if err != nil || !validCanonicalUUIDv4(eventID) {
		return RecordDropped
	}
	event := Event{
		EventID:         eventID,
		InstallationID:  permit.installationID,
		App:             AppGasCity,
		ReleaseVersion:  permit.releaseVersion,
		OS:              permit.operatingSystem,
		OccurredHourUTC: permit.occurredHourUTC,
		CommandID:       commandID,
	}
	encoded, err := EncodeEvent(event)
	if err != nil || len(encoded) == 0 || uint64(len(encoded)) > maximumEventBytes {
		return RecordDropped
	}
	if _, ok := window.remaining(); !ok {
		return RecordDropped
	}

	storageHooks := service.deps.storageHooks
	existingStorageGate := storageHooks.decisionGate
	storageHooks.decisionGate = func() bool {
		if existingStorageGate != nil && !existingStorageGate() {
			return false
		}
		_, ok := window.remaining()
		return ok
	}
	root, err := openStorageRootMutableWithHooks(service.deps.home, storageHooks)
	if err != nil {
		return RecordDropped
	}
	defer func() { _ = root.Close() }()
	remaining, ok := window.remaining()
	if !ok {
		return RecordDropped
	}
	lockContext, cancel := context.WithTimeout(context.Background(), remaining)
	defer cancel()
	lock, err := root.acquireLock(lockContext, stateLockName)
	if err != nil {
		return RecordDropped
	}
	defer func() { _ = lock.Release() }()
	if _, ok := window.remaining(); !ok {
		return RecordDropped
	}

	loaded := loadStateFromDirectory(root)
	defer func() { _ = loaded.Close() }()
	if loaded.err != nil || !loaded.present || loaded.lease == nil ||
		!permit.recordLease.Matches(loaded.lease) || !stateMatchesPermit(loaded.state, permit) ||
		service.project(InvocationContext{}, loaded).state != StateEnabled {
		return RecordDropped
	}
	if _, ok := window.remaining(); !ok {
		return RecordDropped
	}
	canStart := func(operation recordOperation) bool {
		if service.deps.beforeRecordOperation != nil {
			service.deps.beforeRecordOperation(operation)
		}
		_, ok := window.remaining()
		return ok
	}
	quota, present, err := loadForegroundSpoolQuota(root, canStart)
	if err != nil {
		return RecordDropped
	}
	reserved, err := quota.reserve(uint64(len(encoded)))
	if err != nil {
		return RecordDropped
	}
	if _, ok := window.remaining(); !ok {
		return RecordDropped
	}
	if err := persistForegroundSpoolQuota(root, reserved, !present, canStart); err != nil {
		return RecordDropped
	}
	if !canStart(recordOperationQueueOpen) {
		return RecordDropped
	}
	queueRoot, err := root.openDir([]string{queueDirectoryName}, true)
	if err != nil {
		return RecordDropped
	}
	defer func() { _ = queueRoot.Close() }()
	if !canStart(recordOperationGenerationOpen) {
		return RecordDropped
	}
	queue, err := queueRoot.openDir([]string{permit.spoolGeneration}, true)
	if err != nil {
		return RecordDropped
	}
	defer func() { _ = queue.Close() }()
	if !canStart(recordOperationEventWrite) {
		return RecordDropped
	}
	if err := queue.writeFileAtomicNoReplace(eventFileName(eventID), encoded); err != nil {
		// The durable reservation deliberately remains. This crash/failure
		// window can overcount but can never admit an event past the cap.
		return RecordDropped
	}
	return RecordStored
}

func loadForegroundSpoolQuota(root *storageRoot, canStart func(recordOperation) bool) (spoolQuota, bool, error) {
	quota, present, err := loadSpoolQuotaWithGate(root, canStart)
	if err != nil {
		return spoolQuota{}, false, err
	}
	names := []string{spoolControlDirectoryName, retiredControlDirectoryName, fallbackRelocationCursorName}
	if !present {
		names = []string{queueDirectoryName, inflightDirectoryName, spoolControlDirectoryName, retiredControlDirectoryName, fallbackRelocationCursorName}
	}
	for _, name := range names {
		if !recordOperationCanStart(canStart, recordLookupOperation(name)) {
			return spoolQuota{}, false, errRecordDecisionWindowExpired
		}
		_, err := root.lookupEntry(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return spoolQuota{}, false, err
		}
		return spoolQuota{}, false, fmt.Errorf("productmetrics: conservative spool evidence %q is present", name)
	}
	return quota, present, nil
}

func eventFileName(eventID string) string {
	return eventID + eventFileSuffix
}

func eventIDFromFileName(name string) (string, bool) {
	if len(name) != 36+len(eventFileSuffix) || !strings.HasSuffix(name, eventFileSuffix) {
		return "", false
	}
	id := strings.TrimSuffix(name, eventFileSuffix)
	return id, validCanonicalUUIDv4(id)
}

type spoolWorkBudget struct {
	maxEntries     uint64
	maxDirectories uint64
	maxReadBytes   uint64
	maxNameBytes   uint64
}

func defaultSpoolWorkBudget() spoolWorkBudget {
	return spoolWorkBudget{
		maxEntries:     maximumCleanupEntries,
		maxDirectories: maximumCleanupDirectories,
		maxReadBytes:   maximumCleanupReadBytes,
		maxNameBytes:   maximumCleanupNameBytes,
	}
}

type spoolWorkUsage struct {
	entries     uint64
	directories uint64
	readBytes   uint64
	nameBytes   uint64
}

type spoolWorkMeter struct {
	budget                  spoolWorkBudget
	usage                   spoolWorkUsage
	eventEntries            uint64
	exhausted               bool
	traversalError          error
	physicalDirectories     bool
	fixedDirectoryPermits   uint64
	cleanupDirectoryPermits uint64
	fixedEnvelopeClaimed    bool
}

func newSpoolWorkMeter(budget spoolWorkBudget) *spoolWorkMeter {
	if budget.maxEntries == 0 || budget.maxDirectories == 0 || budget.maxNameBytes == 0 {
		return &spoolWorkMeter{budget: budget, exhausted: true}
	}
	return &spoolWorkMeter{budget: budget}
}

func (meter *spoolWorkMeter) chargeDirectory() bool {
	if meter.physicalDirectories {
		ordinaryLimit := meter.ordinaryDirectoryLimit()
		if meter.exhausted || ordinaryLimit < meter.usage.directories ||
			ordinaryLimit-meter.usage.directories < spoolTraversalDirectoryEnvelope {
			meter.exhausted = true
			return false
		}
		return true
	}
	if meter.exhausted || meter.budget.maxDirectories == 0 ||
		meter.usage.directories >= meter.budget.maxDirectories-1 {
		meter.exhausted = true
		return false
	}
	meter.usage.directories++
	return true
}

// chargeFixedDirectory spends the one directory slot reserved for bounded
// control recovery after ordinary traversal has stopped at the directory cap.
func (meter *spoolWorkMeter) chargeFixedDirectory() bool {
	if meter.physicalDirectories {
		if meter.fixedDirectoryPermits > 0 {
			return true
		}
		if meter.usage.directories >= meter.budget.maxDirectories {
			meter.exhausted = true
			return false
		}
		meter.fixedDirectoryPermits++
		return true
	}
	if meter.usage.directories >= meter.budget.maxDirectories {
		// Legacy logical-only meters do not observe the actual fixed open.
		// Physical sweeps above remain strictly capped; direct state-machine
		// tests may reuse the already-reserved fixed slot at the logical cap.
		return true
	}
	meter.usage.directories++
	return true
}

func (meter *spoolWorkMeter) chargeCleanupDirectory() bool {
	if meter != nil && meter.physicalDirectories {
		ordinaryLimit := meter.ordinaryDirectoryLimit()
		if ordinaryLimit < meter.usage.directories ||
			ordinaryLimit-meter.usage.directories < spoolTraversalDirectoryEnvelope {
			meter.exhausted = true
			return false
		}
		meter.cleanupDirectoryPermits += spoolTraversalDirectoryEnvelope
		return true
	}
	return meter != nil && meter.chargeDirectory()
}

func (meter *spoolWorkMeter) ordinaryDirectoryLimit() uint64 {
	if meter == nil || meter.budget.maxDirectories <= spoolFixedDirectoryReserve {
		return 0
	}
	return meter.budget.maxDirectories - spoolFixedDirectoryReserve
}

func (meter *spoolWorkMeter) beforePhysicalDirectoryOpen(string) error {
	if meter == nil {
		return errStorageClosed
	}
	limit := meter.ordinaryDirectoryLimit()
	if meter.fixedDirectoryPermits > 0 {
		limit = meter.budget.maxDirectories
	}
	if meter.usage.directories >= limit {
		meter.exhausted = true
		return errors.New("productmetrics: physical directory-open budget is exhausted")
	}
	return nil
}

func (meter *spoolWorkMeter) afterPhysicalDirectoryOpen(string) {
	if meter == nil {
		return
	}
	if meter.usage.directories < math.MaxUint64 {
		meter.usage.directories++
	}
	if meter.fixedDirectoryPermits > 0 {
		meter.fixedDirectoryPermits--
	} else if meter.cleanupDirectoryPermits > 0 {
		meter.cleanupDirectoryPermits--
	}
}

func (meter *spoolWorkMeter) refundLogicalDirectoryCharge() {
	if meter != nil && !meter.physicalDirectories && meter.usage.directories > 0 {
		meter.usage.directories--
	}
}

func (meter *spoolWorkMeter) next(iterator *storageIterator) (storageEntry, bool) {
	entryLimit := meter.ordinaryEntryLimit()
	nameLimit := meter.ordinaryNameLimit()
	if meter.exhausted || meter.usage.entries >= entryLimit ||
		nameLimit < maximumFilesystemName || meter.usage.nameBytes > nameLimit-maximumFilesystemName {
		meter.exhausted = true
		return storageEntry{}, false
	}
	entry, err := iterator.Next()
	if errors.Is(err, io.EOF) {
		return storageEntry{}, false
	}
	if err != nil {
		meter.traversalError = errors.Join(meter.traversalError, err)
		return storageEntry{}, false
	}
	nameBytes := uint64(entry.nameBytes)
	if nameBytes > nameLimit-meter.usage.nameBytes {
		meter.exhausted = true
		return storageEntry{}, false
	}
	meter.usage.entries++
	meter.usage.nameBytes += nameBytes
	return entry, true
}

func (meter *spoolWorkMeter) chargeEventEntry() bool {
	if meter.eventEntries >= maximumEnumerationEvents {
		meter.exhausted = true
		return false
	}
	meter.eventEntries++
	return true
}

func (meter *spoolWorkMeter) chargeRead(bytes uint64) bool {
	limit := meter.ordinaryReadLimit()
	if meter.exhausted || meter.usage.readBytes > limit || bytes > limit-meter.usage.readBytes {
		meter.exhausted = true
		return false
	}
	meter.usage.readBytes += bytes
	return true
}

func (meter *spoolWorkMeter) refundRead(reserved, used uint64) {
	if meter == nil || used > reserved || meter.usage.readBytes < reserved-used {
		return
	}
	meter.usage.readBytes -= reserved - used
}

func (meter *spoolWorkMeter) chargeNamedEntry(name string) bool {
	nameBytes := uint64(len(name))
	entryLimit := meter.ordinaryEntryLimit()
	nameLimit := meter.ordinaryNameLimit()
	if meter.exhausted || meter.usage.entries >= entryLimit || meter.usage.nameBytes > nameLimit ||
		nameBytes > nameLimit-meter.usage.nameBytes {
		meter.exhausted = true
		return false
	}
	meter.usage.entries++
	meter.usage.nameBytes += nameBytes
	return true
}

// chargeFixedEntry charges one exact fd-relative lookup after traversal has
// stopped on the directory cap. It never relaxes the entry or name budgets.
func (meter *spoolWorkMeter) chargeFixedEntry(name string) bool {
	if meter.physicalDirectories {
		return meter.claimFixedWorkEnvelope()
	}
	nameBytes := uint64(len(name))
	if meter.usage.entries >= meter.budget.maxEntries || nameBytes > meter.budget.maxNameBytes ||
		meter.usage.nameBytes > meter.budget.maxNameBytes-nameBytes {
		meter.exhausted = true
		return false
	}
	meter.usage.entries++
	meter.usage.nameBytes += nameBytes
	return true
}

func (meter *spoolWorkMeter) availableFixedSlots(nameBytes uint64, maximum int) int {
	if meter != nil && meter.physicalDirectories {
		if meter.claimFixedWorkEnvelope() {
			return maximum
		}
		return 0
	}
	if meter == nil || maximum <= 0 || nameBytes == 0 || meter.usage.entries >= meter.budget.maxEntries ||
		meter.usage.nameBytes >= meter.budget.maxNameBytes {
		return 0
	}
	byEntries := meter.budget.maxEntries - meter.usage.entries
	byNames := (meter.budget.maxNameBytes - meter.usage.nameBytes) / nameBytes
	available := min(byEntries, byNames, uint64(maximum))
	return int(available)
}

func (meter *spoolWorkMeter) chargeFixedRead(bytes uint64) bool {
	if meter != nil && meter.physicalDirectories {
		return meter.claimFixedWorkEnvelope()
	}
	if meter == nil || meter.usage.readBytes > meter.budget.maxReadBytes ||
		bytes > meter.budget.maxReadBytes-meter.usage.readBytes {
		if meter != nil {
			meter.exhausted = true
		}
		return false
	}
	meter.usage.readBytes += bytes
	return true
}

func (meter *spoolWorkMeter) ordinaryEntryLimit() uint64 {
	if meter == nil || !meter.physicalDirectories {
		if meter == nil {
			return 0
		}
		return meter.budget.maxEntries
	}
	if meter.budget.maxEntries <= spoolFixedEntryEnvelope {
		return 0
	}
	if meter.fixedEnvelopeClaimed {
		return meter.budget.maxEntries
	}
	return meter.budget.maxEntries - spoolFixedEntryEnvelope
}

func (meter *spoolWorkMeter) ordinaryNameLimit() uint64 {
	if meter == nil || !meter.physicalDirectories {
		if meter == nil {
			return 0
		}
		return meter.budget.maxNameBytes
	}
	if meter.budget.maxNameBytes <= spoolFixedNameEnvelope {
		return 0
	}
	if meter.fixedEnvelopeClaimed {
		return meter.budget.maxNameBytes
	}
	return meter.budget.maxNameBytes - spoolFixedNameEnvelope
}

func (meter *spoolWorkMeter) ordinaryReadLimit() uint64 {
	if meter == nil || !meter.physicalDirectories {
		if meter == nil {
			return 0
		}
		return meter.budget.maxReadBytes
	}
	if meter.budget.maxReadBytes <= spoolFixedReadEnvelope {
		return 0
	}
	if meter.fixedEnvelopeClaimed {
		return meter.budget.maxReadBytes
	}
	return meter.budget.maxReadBytes - spoolFixedReadEnvelope
}

func (meter *spoolWorkMeter) claimFixedWorkEnvelope() bool {
	if meter == nil {
		return false
	}
	if meter.fixedEnvelopeClaimed {
		return true
	}
	if meter.usage.entries > meter.budget.maxEntries ||
		spoolFixedEntryEnvelope > meter.budget.maxEntries-meter.usage.entries ||
		meter.usage.nameBytes > meter.budget.maxNameBytes ||
		spoolFixedNameEnvelope > meter.budget.maxNameBytes-meter.usage.nameBytes ||
		meter.usage.readBytes > meter.budget.maxReadBytes ||
		spoolFixedReadEnvelope > meter.budget.maxReadBytes-meter.usage.readBytes {
		meter.exhausted = true
		return false
	}
	meter.usage.entries += spoolFixedEntryEnvelope
	meter.usage.nameBytes += spoolFixedNameEnvelope
	meter.usage.readBytes += spoolFixedReadEnvelope
	meter.fixedEnvelopeClaimed = true
	return true
}

type spoolPolicy struct {
	generation     string
	installationID string
}

func policyFromPermit(permit RecordingPermit) spoolPolicy {
	return spoolPolicy{generation: permit.spoolGeneration, installationID: permit.installationID}
}

type spoolRecord struct {
	tree             string
	generation       string
	name             string
	event            Event
	bytes            uint64
	mtimeSeconds     int64
	mtimeNanoseconds int64
}

type spoolClaim struct {
	generation string
	records    []spoolRecord
	authority  *spoolClaimAuthority
}

type spoolClaimAuthority struct {
	mu      sync.Mutex
	settled bool
}

func (claim spoolClaim) beginSettlement() (func(), error) {
	if len(claim.records) == 0 {
		return func() {}, nil
	}
	if claim.authority == nil {
		return nil, errors.New("productmetrics: spool claim has no settlement authority")
	}
	claim.authority.mu.Lock()
	if claim.authority.settled {
		claim.authority.mu.Unlock()
		return nil, errors.New("productmetrics: spool claim is already settled")
	}
	claim.authority.settled = true
	return claim.authority.mu.Unlock, nil
}

func (claim spoolClaim) events() []Event {
	events := make([]Event, len(claim.records))
	for index := range claim.records {
		events[index] = claim.records[index].event
	}
	return events
}

type spoolSweepResult struct {
	complete      bool
	usage         spoolWorkUsage
	quota         spoolQuota
	removedEvents uint64
	removedBytes  uint64
}

type spoolSweepState struct {
	root                       *storageRoot
	policy                     spoolPolicy
	now                        time.Time
	purgeAll                   bool
	meter                      *spoolWorkMeter
	quota                      spoolQuota
	records                    []spoolRecord
	seen                       map[string]struct{}
	pruneDirs                  map[string]*storageDir
	removedEvents              uint64
	removedBytes               uint64
	operation                  error
	traversed                  bool
	mutated                    bool
	afterRelocationReservation func() error
	relocationQuotaMarked      bool
	restoreDirectoryOpenHooks  func()
	retainedControl            *storageDir
	retainedRetiredControl     *storageDir
	failClosedArmed            bool
	durableQuotaMarker         bool
}

// reconcileSpool is a caller-held-state.lock primitive. It may lower durable
// quota only after one bounded traversal has accounted for the complete tree;
// otherwise it installs overflow markers so foreground recording stays closed.
func reconcileSpool(root *storageRoot, policy spoolPolicy, now time.Time, budget spoolWorkBudget) (spoolSweepResult, error) {
	state := runSpoolSweep(root, policy, now, budget, false)
	return state.finish()
}

// purgeSpool is a caller-held-state.lock primitive. Disable/pause callers also
// hold uploader.lock first. A complete result is a durable proof that every
// queue/inflight generation is empty and quota.toml is durably zero.
func purgeSpool(root *storageRoot, budget spoolWorkBudget) (spoolSweepResult, error) {
	state := runSpoolSweep(root, spoolPolicy{}, time.Time{}, budget, true)
	return state.finish()
}

func runSpoolSweep(root *storageRoot, policy spoolPolicy, now time.Time, budget spoolWorkBudget, purgeAll bool) *spoolSweepState {
	budget = constrainSpoolDirectoryBudget(root, budget)
	state := &spoolSweepState{
		root: root, policy: policy, now: now.UTC().Truncate(time.Hour), purgeAll: purgeAll,
		meter: newSpoolWorkMeter(budget), seen: make(map[string]struct{}), pruneDirs: make(map[string]*storageDir),
	}
	if root == nil || root.storageDir == nil || root.backend == nil {
		state.operation = errStorageClosed
		return state
	}
	state.meter.physicalDirectories = true
	state.restoreDirectoryOpenHooks = root.installDirectoryOpenHooks(
		state.meter.beforePhysicalDirectoryOpen, state.meter.afterPhysicalDirectoryOpen,
	)
	state.cleanupUnsafeQuota()
	if state.mutated || state.operation != nil || state.meter.exhausted {
		return state
	}
	state.cleanupUnsafeFallbackCursor()
	if state.mutated || state.operation != nil || state.meter.exhausted {
		return state
	}
	state.cleanupDualControlPriority()
	if state.mutated || state.operation != nil || state.meter.exhausted {
		return state
	}
	for _, tree := range []string{queueDirectoryName, inflightDirectoryName} {
		if state.meter.exhausted {
			break
		}
		state.walkTree(tree)
	}
	if !state.purgeAll && state.operation == nil && state.meter.traversalError == nil {
		// Every expired or malformed event reached within this invocation's
		// global budget has already been removed. Only then prune oldest valid
		// records, so expiry always wins within the bounded working set.
		state.pruneOldestToQuota()
	}
	if !state.mutated && state.operation == nil && state.meter.traversalError == nil && !state.meter.exhausted {
		state.cleanupRetiredControlDirectory()
	}
	if !state.mutated && state.operation == nil && state.meter.traversalError == nil && !state.meter.exhausted {
		state.cleanupSpoolControlDirectory()
	}
	if !state.mutated && state.operation == nil && state.meter.traversalError == nil && !state.meter.exhausted {
		state.cleanupFallbackCursor()
	}
	state.traversed = !state.meter.exhausted && state.meter.traversalError == nil && state.operation == nil
	return state
}

func constrainSpoolDirectoryBudget(root *storageRoot, budget spoolWorkBudget) spoolWorkBudget {
	if root == nil || root.storageDir == nil || root.backend == nil || budget.maxDirectories == 0 {
		return budget
	}
	limiter, ok := root.backend.(storageFileDescriptorLimitBackend)
	if !ok {
		return budget
	}
	softLimit, err := limiter.fileDescriptorSoftLimit()
	effective := spoolFallbackDirectoryLimit
	if err == nil {
		effective = spoolDirectoryBudgetForSoftLimit(budget.maxDirectories, softLimit)
	}
	if effective < budget.maxDirectories {
		budget.maxDirectories = effective
	}
	return budget
}

func spoolDirectoryBudgetForSoftLimit(requested, softLimit uint64) uint64 {
	effective := softLimit / spoolFileDescriptorHeadroom
	if effective < spoolMinimumDirectoryProgress {
		effective = spoolMinimumDirectoryProgress
	}
	if requested < effective {
		return requested
	}
	return effective
}

func (state *spoolSweepState) cleanupUnsafeQuota() {
	if state == nil || state.root == nil || state.meter == nil || !state.meter.chargeNamedEntry(quotaFileName) {
		return
	}
	entry, err := state.root.lookupEntry(quotaFileName)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	safeRegular := entry.metadata.kind == storageEntryRegular && entry.metadata.nlink == 1 &&
		entry.metadata.uid == uint32(os.Geteuid()) && entry.metadata.ownerOnly
	if safeRegular {
		return
	}
	if !state.ensureFailClosedControl() {
		return
	}
	if entry.metadata.kind != storageEntryDirectory {
		if err := state.root.unlinkEnumeratedEntry(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
			state.operation = errors.Join(state.operation, err)
		} else if err == nil {
			state.mutated = true
		}
		return
	}
	if !state.meter.chargeCleanupDirectory() {
		return
	}
	directory, err := state.root.openEnumeratedCleanupDirectory(entry)
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	state.purgeDirectory(directory, directory, false)
	state.operation = errors.Join(state.operation, directory.Close())
	if state.meter.exhausted || state.operation != nil {
		return
	}
	if err := state.root.removeEnumeratedCleanupDirectory(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
		state.operation = errors.Join(state.operation, err)
	} else if err == nil {
		state.mutated = true
	}
}

func (state *spoolSweepState) cleanupUnsafeFallbackCursor() {
	if state == nil || state.root == nil || state.meter == nil || !state.meter.chargeNamedEntry(fallbackRelocationCursorName) {
		return
	}
	entry, err := state.root.lookupEntry(fallbackRelocationCursorName)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	if safeFallbackRelocationCursor(entry) {
		return
	}
	if !state.ensureFailClosedControl() {
		return
	}
	if entry.metadata.kind != storageEntryDirectory {
		if err := state.root.unlinkEnumeratedEntry(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
			state.operation = errors.Join(state.operation, err)
		} else if err == nil {
			state.mutated = true
		}
		return
	}
	if !state.meter.chargeCleanupDirectory() {
		return
	}
	directory, err := state.root.openEnumeratedCleanupDirectory(entry)
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	state.purgeDirectory(directory, directory, false)
	state.operation = errors.Join(state.operation, directory.Close())
	if state.meter.exhausted || state.operation != nil {
		return
	}
	if err := state.root.removeEnumeratedCleanupDirectory(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
		state.operation = errors.Join(state.operation, err)
	} else if err == nil {
		state.mutated = true
	}
}

func (state *spoolSweepState) cleanupFallbackCursor() {
	if state == nil || state.root == nil || state.meter == nil || !state.meter.chargeNamedEntry(fallbackRelocationCursorName) {
		return
	}
	entry, err := state.root.lookupEntry(fallbackRelocationCursorName)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	if !safeFallbackRelocationCursor(entry) {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: fallback relocation cursor changed before cleanup"))
		return
	}
	if !state.ensureAlternateControlBarrier(spoolControlDirectoryName) {
		return
	}
	if err := state.root.unlinkEnumeratedEntry(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
		state.operation = errors.Join(state.operation, err)
	} else if err == nil {
		state.mutated = true
	}
}

func safeFallbackRelocationCursor(entry storageEntry) bool {
	return entry.metadata.kind == storageEntryRegular && entry.metadata.nlink == 1 &&
		entry.metadata.uid == uint32(os.Geteuid()) && entry.metadata.ownerOnly &&
		entry.metadata.size >= 0 && entry.metadata.size <= maximumRelocationBytes
}

func (state *spoolSweepState) cleanupDualControlPriority() {
	if state == nil || state.root == nil || state.meter == nil {
		return
	}
	if !state.meter.chargeNamedEntry(spoolControlDirectoryName) {
		return
	}
	_, activeErr := state.root.lookupEntry(spoolControlDirectoryName)
	if activeErr != nil && !errors.Is(activeErr, fs.ErrNotExist) {
		state.operation = errors.Join(state.operation, activeErr)
		return
	}
	if !state.meter.chargeNamedEntry(retiredControlDirectoryName) {
		return
	}
	_, retiredErr := state.root.lookupEntry(retiredControlDirectoryName)
	if retiredErr != nil && !errors.Is(retiredErr, fs.ErrNotExist) {
		state.operation = errors.Join(state.operation, retiredErr)
		return
	}
	if retiredErr == nil {
		if activeErr == nil {
			state.failClosedArmed = true
		}
		state.cleanupRetiredControlDirectory()
	}
}

func (state *spoolSweepState) ensureFailClosedControl() bool {
	if state == nil || state.root == nil || state.meter == nil {
		return false
	}
	if state.failClosedArmed {
		return true
	}
	if !state.meter.chargeFixedEntry(spoolControlDirectoryName) {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: cleanup budget cannot inspect fail-closed control"))
		return false
	}
	_, activeErr := state.root.lookupEntry(spoolControlDirectoryName)
	if activeErr == nil {
		state.failClosedArmed = true
		if !state.meter.chargeFixedEntry(retiredControlDirectoryName) {
			return true
		}
		if _, retiredErr := state.root.lookupEntry(retiredControlDirectoryName); retiredErr == nil {
			// Active evidence remains named while the one fixed directory slot
			// is spent making retired cleanup progress first.
			return true
		} else if !errors.Is(retiredErr, fs.ErrNotExist) {
			state.operation = errors.Join(state.operation, retiredErr)
			return false
		}
		// Presence alone is durable fail-closed evidence. Defer opening an
		// existing namespace until the caller knows whether it needs cursor or
		// quota contents, preserving the one fixed physical slot.
		return true
	}
	if !errors.Is(activeErr, fs.ErrNotExist) {
		state.operation = errors.Join(state.operation, activeErr)
		return false
	}
	if !state.meter.chargeFixedEntry(retiredControlDirectoryName) {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: cleanup budget cannot inspect retired fail-closed control"))
		return false
	}
	_, retiredErr := state.root.lookupEntry(retiredControlDirectoryName)
	if retiredErr == nil {
		state.failClosedArmed = true
		return true
	}
	if !errors.Is(retiredErr, fs.ErrNotExist) {
		state.operation = errors.Join(state.operation, retiredErr)
		return false
	}
	if !state.meter.chargeFixedDirectory() {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: cleanup budget cannot open fail-closed control"))
		return false
	}
	control, err := state.root.openDir([]string{spoolControlDirectoryName}, true)
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return false
	}
	state.retainedControl = control
	state.failClosedArmed = true
	state.mutated = true
	return true
}

func (state *spoolSweepState) ensureDurableQuotaMarker() bool {
	if state.durableQuotaMarker {
		return true
	}
	if !state.meter.chargeFixedEntry(quotaFileName) || !state.meter.chargeFixedRead(maximumQuotaBytes+1) {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: cleanup budget cannot inspect fail-closed quota marker"))
		return false
	}
	markers := spoolQuota{Events: maximumQuotaEventMarker, Bytes: maximumQuotaByteMarker}
	quota, present, err := loadSpoolQuota(state.root)
	if err == nil && present && quota == markers {
		state.durableQuotaMarker = true
		return true
	}
	data, encodeErr := encodeSpoolQuota(markers)
	if encodeErr != nil || !state.meter.chargeFixedRead(uint64(len(data))) {
		state.operation = errors.Join(state.operation, err, encodeErr, errors.New("productmetrics: cleanup budget cannot write fail-closed quota marker"))
		return false
	}
	if persistErr := persistSpoolQuotaDirect(state.root, markers); persistErr != nil {
		state.operation = errors.Join(state.operation, err, persistErr)
		return false
	}
	state.durableQuotaMarker = true
	state.mutated = true
	return true
}

func (state *spoolSweepState) ensureAlternateControlBarrier(alternateName string) bool {
	if state == nil || state.root == nil || state.meter == nil {
		return false
	}
	if !state.meter.chargeFixedEntry(alternateName) {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: cleanup budget cannot inspect alternate fail-closed control"))
		return false
	}
	_, err := state.root.lookupEntry(alternateName)
	if err == nil {
		return true
	}
	if !errors.Is(err, fs.ErrNotExist) {
		state.operation = errors.Join(state.operation, err)
		return false
	}
	return state.ensureDurableQuotaMarker()
}

func (state *spoolSweepState) cleanupSpoolControlDirectory() {
	if !state.meter.chargeNamedEntry(spoolControlDirectoryName) {
		return
	}
	entry, err := state.root.lookupEntry(spoolControlDirectoryName)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	if !state.meter.chargeFixedEntry(retiredControlDirectoryName) {
		return
	}
	_, retiredErr := state.root.lookupEntry(retiredControlDirectoryName)
	if errors.Is(retiredErr, fs.ErrNotExist) {
		if !state.ensureDurableQuotaMarker() {
			return
		}
	} else if retiredErr != nil {
		state.operation = errors.Join(state.operation, retiredErr)
		return
	}
	unlinkErr := state.root.unlinkEnumeratedEntry(entry)
	if unlinkErr == nil {
		state.mutated = true
		return
	}
	if errors.Is(unlinkErr, fs.ErrNotExist) {
		return
	}
	if !errors.Is(unlinkErr, errStorageEntryIsDirectory) {
		state.operation = errors.Join(state.operation, unlinkErr)
		return
	}
	removeErr := state.root.removeEnumeratedCleanupDirectory(entry)
	if removeErr == nil {
		state.mutated = true
		return
	}
	if !errors.Is(removeErr, errStorageDirectoryNotEmpty) {
		if !errors.Is(removeErr, fs.ErrNotExist) {
			state.operation = errors.Join(state.operation, removeErr)
		}
		return
	}
	result, renameErr := state.root.renameEnumeratedDirectory(entry, state.root.storageDir, retiredControlDirectoryName)
	if result.state != storageRenameNotApplied {
		state.mutated = true
	}
	if renameErr != nil {
		state.operation = errors.Join(state.operation, renameErr)
	} else if result.state != storageRenameAppliedDurable {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: control retirement was not durable"))
	}
}

func (state *spoolSweepState) cleanupRetiredControlDirectory() {
	if !state.meter.chargeNamedEntry(retiredControlDirectoryName) {
		return
	}
	entry, err := state.root.lookupEntry(retiredControlDirectoryName)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	if !state.meter.chargeFixedEntry(spoolControlDirectoryName) {
		return
	}
	_, activeErr := state.root.lookupEntry(spoolControlDirectoryName)
	if errors.Is(activeErr, fs.ErrNotExist) {
		if !state.ensureDurableQuotaMarker() {
			return
		}
	} else if activeErr != nil {
		state.operation = errors.Join(state.operation, activeErr)
		return
	}
	unlinkErr := state.root.unlinkEnumeratedEntry(entry)
	if unlinkErr == nil {
		state.mutated = true
		return
	}
	if errors.Is(unlinkErr, fs.ErrNotExist) {
		return
	}
	if !errors.Is(unlinkErr, errStorageEntryIsDirectory) {
		state.operation = errors.Join(state.operation, unlinkErr)
		return
	}
	if !state.meter.chargeCleanupDirectory() {
		return
	}
	retired, openErr := state.root.openEnumeratedCleanupDirectory(entry)
	if openErr != nil {
		state.operation = errors.Join(state.operation, openErr)
		return
	}
	state.retainedRetiredControl = retired
	state.purgeDirectory(retired, retired, false)
	state.retainedRetiredControl = nil
	state.operation = errors.Join(state.operation, retired.Close())
	if state.meter.exhausted {
		return
	}
	if err := state.root.removeEnumeratedCleanupDirectory(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
		state.operation = errors.Join(state.operation, err)
	} else if err == nil {
		state.mutated = true
	}
}

func (state *spoolSweepState) walkTree(treeName string) {
	if !state.meter.chargeNamedEntry(treeName) {
		return
	}
	entry, lookupErr := state.root.lookupEntry(treeName)
	if errors.Is(lookupErr, fs.ErrNotExist) {
		return
	}
	if lookupErr != nil {
		state.operation = errors.Join(state.operation, lookupErr)
		return
	}
	if !state.meter.chargeDirectory() {
		return
	}
	tree, err := state.root.openEnumeratedCleanupDirectory(entry)
	if err != nil {
		state.operation = errors.Join(state.operation, directoryDescriptorExhaustion(err))
		if !state.meter.chargeEventEntry() {
			return
		}
		state.deleteLeaf(state.root.storageDir, entry, true)
		return
	}
	cleanupMalformedTree := tree.cleanupOnly()
	defer func() {
		if !cleanupMalformedTree || state.meter.exhausted {
			return
		}
		if !state.ensureFailClosedControl() {
			return
		}
		removeErr := state.root.removeEnumeratedCleanupDirectory(entry)
		if removeErr == nil {
			state.mutated = true
			return
		}
		if !errors.Is(removeErr, fs.ErrNotExist) && !errors.Is(removeErr, errStorageDirectoryNotEmpty) {
			state.operation = errors.Join(state.operation, removeErr)
		}
	}()
	defer func() { state.operation = errors.Join(state.operation, tree.Close()) }()
	iterator, err := tree.iterateEntries()
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	defer func() { state.operation = errors.Join(state.operation, iterator.Close()) }()
	for {
		entry, ok := state.meter.next(iterator)
		if !ok {
			return
		}
		validGeneration := validCanonicalUUIDv4(entry.name)
		deleteGeneration := state.purgeAll || tree.cleanupOnly() || !validGeneration || entry.name != state.policy.generation
		if entry.metadata.kind != storageEntryDirectory {
			if !state.meter.chargeEventEntry() {
				return
			}
			state.deleteLeaf(tree, entry, true)
			continue
		}
		var canOpen bool
		if deleteGeneration {
			canOpen = state.meter.chargeCleanupDirectory()
		} else {
			canOpen = state.meter.chargeDirectory()
		}
		if !canOpen {
			state.quarantineDirectory(tree, tree, entry, true, true)
			return
		}
		generation, openErr := openEnumeratedStorageDirectory(tree, entry)
		if openErr != nil {
			state.operation = errors.Join(state.operation, directoryDescriptorExhaustion(openErr))
			// The declared layout permits only generation directories here.
			state.meter.refundLogicalDirectoryCharge()
			if !state.meter.chargeEventEntry() {
				return
			}
			state.deleteLeaf(tree, entry, true)
			continue
		}
		cleanupGeneration := deleteGeneration || generation.cleanupOnly()
		if cleanupGeneration {
			state.purgeDirectory(generation, tree, true)
		} else {
			state.scanCurrentGeneration(treeName, entry.name, generation, tree)
		}
		state.operation = errors.Join(state.operation, generation.Close())
		if cleanupGeneration && !state.meter.exhausted {
			if !state.ensureFailClosedControl() {
				return
			}
			if err := tree.removeEnumeratedCleanupDirectory(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
				state.operation = errors.Join(state.operation, err)
			} else if err == nil {
				state.mutated = true
			}
		}
		if state.mutated {
			return
		}
	}
}

func openEnumeratedStorageDirectory(parent *storageDir, entry storageEntry) (*storageDir, error) {
	if parent == nil || parent.backend == nil {
		return nil, errStorageClosed
	}
	if err := validateEnumeratedEntry(entry); err != nil {
		return nil, err
	}
	return parent.openEnumeratedCleanupDirectory(entry)
}

func (state *spoolSweepState) purgeDirectory(directory, quarantineRoot *storageDir, eventTree bool) {
	iterator, err := directory.iterateEntries()
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	defer func() { state.operation = errors.Join(state.operation, iterator.Close()) }()
	for {
		entry, ok := state.meter.next(iterator)
		if !ok {
			return
		}
		if entry.metadata.kind != storageEntryDirectory {
			if eventTree && !state.meter.chargeEventEntry() {
				return
			}
			state.deleteLeaf(directory, entry, eventTree)
			continue
		}
		if !state.meter.chargeCleanupDirectory() {
			state.quarantineDirectory(directory, quarantineRoot, entry, true, eventTree)
			return
		}
		child, openErr := openEnumeratedStorageDirectory(directory, entry)
		if openErr == nil {
			state.purgeDirectory(child, quarantineRoot, eventTree)
			state.operation = errors.Join(state.operation, child.Close())
			if !state.meter.exhausted {
				if !state.ensureFailClosedControl() {
					return
				}
				if err := directory.removeEnumeratedCleanupDirectory(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
					state.operation = errors.Join(state.operation, err)
				} else if err == nil {
					state.mutated = true
				}
			}
			continue
		}
		state.operation = errors.Join(state.operation, directoryDescriptorExhaustion(openErr))
		state.meter.refundLogicalDirectoryCharge()
		if eventTree && !state.meter.chargeEventEntry() {
			return
		}
		state.deleteLeaf(directory, entry, eventTree)
	}
}

func (state *spoolSweepState) quarantineDirectory(parent, quarantineRoot *storageDir, entry storageEntry, deleteLeaf, eventTree bool) {
	if !state.ensureFailClosedControl() {
		return
	}
	name := fmt.Sprintf(".orphan-%x-%x", entry.metadata.dev, entry.metadata.ino)
	if !state.meter.chargeFixedEntry(name) {
		return
	}
	result, err := parent.renameEnumeratedDirectory(entry, quarantineRoot, name)
	if result.state != storageRenameNotApplied {
		state.mutated = true
	}
	if errors.Is(err, errStorageDestinationExists) {
		state.resolveQuarantineCollision(parent, quarantineRoot, entry, name, eventTree)
		return
	}
	if err != nil {
		if deleteLeaf {
			unlinkErr := parent.unlinkEnumeratedEntry(entry)
			if unlinkErr == nil || errors.Is(unlinkErr, fs.ErrNotExist) {
				if unlinkErr == nil {
					state.mutated = true
				}
				if eventTree {
					state.noteRemoved(entry.metadata.size)
				}
				return
			}
			err = errors.Join(err, unlinkErr)
		}
		state.operation = errors.Join(state.operation, err)
		return
	}
	if result.state != storageRenameAppliedDurable {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: malformed subtree quarantine was not durable"))
	}
}

func (state *spoolSweepState) resolveQuarantineCollision(parent, quarantineRoot *storageDir, source storageEntry, targetName string, eventTree bool) {
	target, err := quarantineRoot.lookupEntry(targetName)
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	if err := quarantineRoot.unlinkEnumeratedEntry(target); err == nil {
		state.mutated = true
		if eventTree {
			state.noteRemoved(target.metadata.size)
		}
		return
	} else if !errors.Is(err, errStorageEntryIsDirectory) {
		state.operation = errors.Join(state.operation, err)
		return
	}
	if err := quarantineRoot.removeEnumeratedCleanupDirectory(target); err == nil {
		state.mutated = true
		return
	} else if !errors.Is(err, errStorageDirectoryNotEmpty) {
		state.operation = errors.Join(state.operation, err)
		return
	}
	result, err := parent.exchangeEnumeratedEntries(source, quarantineRoot, target)
	if errors.Is(err, errStorageExchangeAncestor) {
		state.relocateQuarantineBlocker(quarantineRoot, target, eventTree)
		return
	}
	if errors.Is(err, errStorageExchangeUnsupported) || errors.Is(err, errStorageExchangeSameEntry) {
		state.relocateUnsupportedExchangeBlocker(quarantineRoot, target)
		return
	}
	if result.state != storageRenameNotApplied {
		state.mutated = true
	}
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	if result.state != storageRenameAppliedDurable {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: malformed subtree exchange was not durable"))
	}
}

func (state *spoolSweepState) relocateUnsupportedExchangeBlocker(quarantineRoot *storageDir, blocker storageEntry) {
	start, slots, err := state.reserveRelocationSlots()
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	for offset := 0; offset < slots; offset++ {
		sequence := start + uint64(offset)
		name := relocationCandidateName(sequence)
		if !state.meter.chargeFixedEntry(name) {
			state.operation = errors.Join(state.operation, errors.New("productmetrics: reserved relocation slot exceeded cleanup budget"))
			return
		}
		_, lookupErr := quarantineRoot.lookupEntry(name)
		if lookupErr == nil {
			continue
		}
		if !errors.Is(lookupErr, fs.ErrNotExist) {
			state.operation = errors.Join(state.operation, lookupErr)
			return
		}
		result, renameErr := quarantineRoot.renameEnumeratedDirectory(blocker, quarantineRoot, name)
		if result.state != storageRenameNotApplied {
			state.mutated = true
		}
		if errors.Is(renameErr, errStorageDestinationExists) {
			continue
		}
		if renameErr != nil {
			state.operation = errors.Join(state.operation, renameErr)
			return
		}
		if result.state != storageRenameAppliedDurable {
			state.operation = errors.Join(state.operation, errors.New("productmetrics: fallback blocker relocation was not durable"))
		}
		return
	}
}

func relocationCandidateName(sequence uint64) string {
	return fmt.Sprintf(".pm-relocated-%016x", sequence)
}

func (state *spoolSweepState) reserveRelocationSlots() (uint64, int, error) {
	if state == nil || state.root == nil || state.meter == nil {
		return 0, 0, errStorageClosed
	}
	_, retiredPresent, progressErr := state.cleanupOneRetiredControlEntryFixed()
	if progressErr != nil || retiredPresent {
		return 0, 0, progressErr
	}
	created := false
	control := state.retainedControl
	if control == nil {
		if !state.meter.chargeFixedDirectory() {
			return 0, 0, errors.New("productmetrics: cleanup budget cannot open relocation control directory")
		}
		if !state.meter.chargeFixedEntry(spoolControlDirectoryName) {
			return 0, 0, errors.New("productmetrics: cleanup budget cannot inspect relocation control directory")
		}
		_, lookupErr := state.root.lookupEntry(spoolControlDirectoryName)
		var err error
		switch {
		case errors.Is(lookupErr, fs.ErrNotExist):
			control, err = state.root.openDir([]string{spoolControlDirectoryName}, true)
			created = err == nil
		case lookupErr != nil:
			return 0, 0, lookupErr
		default:
			control, err = state.root.openDir([]string{spoolControlDirectoryName}, false)
		}
		if err != nil {
			if lookupErr == nil {
				retireErr := state.retireActiveControlNamespace(nil, err)
				if retireErr == nil {
					return 0, 0, nil
				}
				return 0, 0, errors.Join(err, retireErr)
			}
			return 0, 0, err
		}
		state.retainedControl = control
		state.failClosedArmed = true
		if created {
			state.mutated = true
		}
	}
	if err := state.ensureConservativeRelocationQuota(control); err != nil {
		retireErr := state.retireActiveControlNamespace(control, err)
		if retireErr == nil {
			return 0, 0, nil
		}
		return 0, 0, errors.Join(err, retireErr)
	}

	if !state.meter.chargeFixedEntry(relocationCursorFileName) {
		return 0, 0, errors.New("productmetrics: cleanup budget cannot inspect relocation cursor")
	}
	cursor := relocationCursor{}
	cursorEntry, cursorLookupErr := control.lookupEntry(relocationCursorFileName)
	if cursorLookupErr == nil {
		if !state.meter.chargeFixedRead(maximumRelocationBytes + 1) {
			return 0, 0, errors.New("productmetrics: cleanup budget cannot read relocation cursor")
		}
		data, readErr := control.readFile(relocationCursorFileName, maximumRelocationBytes)
		if readErr != nil {
			if err := state.retireRelocationCursor(control, cursorEntry, readErr); err != nil {
				return 0, 0, err
			}
			return 0, 0, nil
		}
		decoded, decodeErr := decodeRelocationCursor(data)
		if decodeErr != nil {
			if err := state.retireRelocationCursor(control, cursorEntry, decodeErr); err != nil {
				return 0, 0, err
			}
			return 0, 0, nil
		}
		cursor = decoded
	} else if !errors.Is(cursorLookupErr, fs.ErrNotExist) {
		return 0, 0, cursorLookupErr
	}

	candidateBytes := uint64(len(relocationCandidateName(maximumRelocationSequence)))
	slots := state.meter.availableFixedSlots(candidateBytes, maximumRelocationSlots)
	if slots != maximumRelocationSlots {
		return 0, 0, errors.New("productmetrics: cleanup budget cannot reserve a complete relocation block")
	}
	if cursor.Next > maximumRelocationSequence-uint64(slots) {
		if err := state.retireRelocationCursor(control, cursorEntry, errors.New("productmetrics: relocation cursor is exhausted")); err != nil {
			return 0, 0, err
		}
		return 0, 0, nil
	}
	reserved := relocationCursor{Next: cursor.Next + uint64(slots)}
	data, err := encodeRelocationCursor(reserved)
	if err != nil {
		return 0, 0, err
	}
	if !state.meter.chargeFixedRead(uint64(len(data))) {
		return 0, 0, errors.New("productmetrics: cleanup budget cannot write relocation cursor")
	}
	result, writeErr := control.writeFileAtomicOutcome(relocationCursorFileName, data)
	if result.state != storageWriteNotApplied {
		state.mutated = true
	}
	if writeErr != nil {
		return 0, 0, writeErr
	}
	if result.state != storageWriteAppliedDurable {
		return 0, 0, errors.New("productmetrics: relocation cursor reservation was not durable")
	}
	state.mutated = true
	if state.afterRelocationReservation != nil {
		if err := state.afterRelocationReservation(); err != nil {
			return 0, 0, fmt.Errorf("productmetrics: injected post-reservation failure: %w", err)
		}
	}
	return cursor.Next, slots, nil
}

func (state *spoolSweepState) cleanupOneRetiredControlEntryFixed() (bool, bool, error) {
	if !state.meter.chargeFixedEntry(retiredControlDirectoryName) {
		return false, false, errors.New("productmetrics: cleanup budget cannot inspect retired control")
	}
	entry, err := state.root.lookupEntry(retiredControlDirectoryName)
	if errors.Is(err, fs.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if !state.ensureAlternateControlBarrier(spoolControlDirectoryName) {
		return false, true, errors.New("productmetrics: cannot remove retired control without replacement fail-closed evidence")
	}
	if err := state.root.unlinkEnumeratedEntry(entry); err == nil {
		state.mutated = true
		return true, true, nil
	} else if !errors.Is(err, errStorageEntryIsDirectory) {
		return false, true, err
	}
	if err := state.root.removeEnumeratedCleanupDirectory(entry); err == nil {
		state.mutated = true
		return true, true, nil
	} else if !errors.Is(err, errStorageDirectoryNotEmpty) {
		return false, true, err
	}
	retired := state.retainedRetiredControl
	closeRetired := false
	if retired == nil {
		if !state.meter.chargeFixedDirectory() {
			return false, true, nil
		}
		retired, err = state.root.openEnumeratedCleanupDirectory(entry)
		if err != nil {
			return false, true, err
		}
		closeRetired = true
	}
	if closeRetired {
		defer func() { state.operation = errors.Join(state.operation, retired.Close()) }()
	}
	child, err := retired.firstEntryFromRetainedHandle()
	if errors.Is(err, io.EOF) {
		return false, true, nil
	}
	if err != nil {
		return false, true, err
	}
	if !state.meter.chargeFixedEntry(child.name) {
		return false, true, errors.New("productmetrics: cleanup budget cannot inspect retired-control child")
	}
	if child.metadata.kind != storageEntryDirectory {
		if err := retired.unlinkEnumeratedEntry(child); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, true, err
		}
		state.mutated = true
		return true, true, nil
	}
	if err := retired.removeEnumeratedCleanupDirectory(child); err == nil {
		state.mutated = true
		return true, true, nil
	} else if !errors.Is(err, errStorageDirectoryNotEmpty) {
		return false, true, err
	}
	if !state.meter.chargeFixedDirectory() {
		return false, true, nil
	}
	childDirectory, err := retired.openEnumeratedCleanupDirectory(child)
	if err != nil {
		return false, true, err
	}
	defer func() { state.operation = errors.Join(state.operation, childDirectory.Close()) }()
	grandchild, err := childDirectory.firstEntryFromRetainedHandle()
	if errors.Is(err, io.EOF) {
		return false, true, nil
	}
	if err != nil {
		return false, true, err
	}
	if !state.meter.chargeFixedEntry(grandchild.name) {
		return false, true, errors.New("productmetrics: cleanup budget cannot inspect nested retired-control child")
	}
	if grandchild.metadata.kind != storageEntryDirectory {
		if err := childDirectory.unlinkEnumeratedEntry(grandchild); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, true, err
		}
		state.mutated = true
		return true, true, nil
	}
	if err := childDirectory.removeEnumeratedCleanupDirectory(grandchild); err == nil {
		state.mutated = true
		return true, true, nil
	} else if !errors.Is(err, errStorageDirectoryNotEmpty) {
		return false, true, err
	}
	targetName := fmt.Sprintf(".orphan-%x-%x", grandchild.metadata.dev, grandchild.metadata.ino)
	progressed, err := state.liftNestedRetiredControlDirectory(childDirectory, retired, grandchild, targetName)
	return progressed, true, err
}

func (state *spoolSweepState) liftNestedRetiredControlDirectory(parent, retired *storageDir, source storageEntry, targetName string) (bool, error) {
	if !state.meter.chargeFixedEntry(targetName) {
		return false, errors.New("productmetrics: cleanup budget cannot relocate nested retired-control child")
	}
	result, renameErr := parent.renameEnumeratedDirectory(source, retired, targetName)
	if result.state != storageRenameNotApplied {
		state.mutated = true
	}
	if !errors.Is(renameErr, errStorageDestinationExists) {
		return validateRetiredControlRename(result, renameErr, "nested retired-control relocation")
	}
	if result.state != storageRenameNotApplied {
		return true, renameErr
	}

	blocker, lookupErr := retired.lookupEntry(targetName)
	if lookupErr != nil {
		return false, lookupErr
	}
	if unlinkErr := retired.unlinkEnumeratedEntry(blocker); unlinkErr == nil {
		state.mutated = true
		return true, nil
	} else if !errors.Is(unlinkErr, errStorageEntryIsDirectory) {
		return false, unlinkErr
	}
	if removeErr := retired.removeEnumeratedCleanupDirectory(blocker); removeErr == nil {
		state.mutated = true
		return true, nil
	} else if !errors.Is(removeErr, errStorageDirectoryNotEmpty) {
		return false, removeErr
	}

	exchangeResult, exchangeErr := parent.exchangeEnumeratedEntries(source, retired, blocker)
	if exchangeResult.state != storageRenameNotApplied {
		state.mutated = true
	}
	unsupported := errors.Is(exchangeErr, errStorageExchangeUnsupported)
	ancestor := errors.Is(exchangeErr, errStorageExchangeAncestor)
	if !unsupported && !ancestor {
		return validateRetiredControlRename(exchangeResult, exchangeErr, "nested retired-control exchange")
	}
	if exchangeResult.state != storageRenameNotApplied {
		return true, exchangeErr
	}
	return state.rotateRetiredControlCollision(retired, blocker)
}

func (state *spoolSweepState) rotateRetiredControlCollision(retired *storageDir, blocker storageEntry) (bool, error) {
	current := blocker
	seen := make(map[[2]uint64]struct{}, maximumRelocationSlots)
	for attempts := 0; attempts < maximumRelocationSlots; attempts++ {
		identity := [2]uint64{current.metadata.dev, current.metadata.ino}
		if _, duplicate := seen[identity]; duplicate {
			return state.breakRetiredControlCanonicalGraph(retired, current)
		}
		seen[identity] = struct{}{}
		candidateName := fmt.Sprintf(".orphan-%x-%x", current.metadata.dev, current.metadata.ino)
		if candidateName == current.name {
			return state.breakRetiredControlCanonicalGraph(retired, current)
		}
		progressed, occupant, collision, err := state.rotateRetiredControlEntry(retired, current, candidateName)
		if progressed || err != nil {
			return progressed, err
		}
		if !collision {
			return false, errors.New("productmetrics: retired-control canonical rotation made no progress")
		}
		current = occupant
	}
	return state.breakRetiredControlCanonicalGraph(retired, current)
}

func (state *spoolSweepState) breakRetiredControlCanonicalGraph(retired *storageDir, current storageEntry) (bool, error) {
	span := maximumRelocationSequence - uint64(maximumRelocationSlots)
	start := (current.metadata.dev ^ current.metadata.ino*0x9e3779b97f4a7c15) % (span + 1)
	for offset := 0; offset < maximumRelocationSlots; offset++ {
		candidateName := relocationCandidateName(start + uint64(offset))
		if candidateName == current.name {
			continue
		}
		progressed, _, collision, err := state.rotateRetiredControlEntry(retired, current, candidateName)
		if progressed || err != nil {
			return progressed, err
		}
		if !collision {
			return false, errors.New("productmetrics: retired-control graph breaker made no progress")
		}
	}
	return state.promoteRetiredControlBlocker(retired, current)
}

func (state *spoolSweepState) promoteRetiredControlBlocker(retired *storageDir, blocker storageEntry) (bool, error) {
	if state == nil || state.root == nil || state.root.storageDir == nil || state.meter == nil {
		return false, errStorageClosed
	}
	if !state.meter.chargeFixedEntry(spoolControlDirectoryName) {
		return false, errors.New("productmetrics: cleanup budget cannot inspect graph-breaker promotion target")
	}
	active, activeErr := state.root.lookupEntry(spoolControlDirectoryName)
	if activeErr == nil {
		state.failClosedArmed = true
		exchangeResult, exchangeErr := retired.exchangeEnumeratedEntries(blocker, state.root.storageDir, active)
		if exchangeResult.state != storageRenameNotApplied {
			state.mutated = true
		}
		if !errors.Is(exchangeErr, errStorageExchangeUnsupported) || exchangeResult.state != storageRenameNotApplied {
			return validateRetiredControlRename(exchangeResult, exchangeErr, "active/retired graph-breaker exchange")
		}
		return state.parkActiveControlInRetired(retired, active)
	}
	if !errors.Is(activeErr, fs.ErrNotExist) {
		return false, activeErr
	}
	result, renameErr := retired.renameEnumeratedDirectory(blocker, state.root.storageDir, spoolControlDirectoryName)
	if result.state != storageRenameNotApplied {
		state.mutated = true
		state.failClosedArmed = true
	}
	return validateRetiredControlRename(result, renameErr, "retired-control graph-breaker promotion")
}

func (state *spoolSweepState) parkActiveControlInRetired(retired *storageDir, active storageEntry) (bool, error) {
	reservation, err := state.reserveFallbackRelocationBlock()
	if err != nil {
		return state.mutated, err
	}
	for offset := 0; offset < reservation.slots; offset++ {
		candidateName := fallbackRelocationCandidateName(reservation.cursor, reservation.start+uint64(offset))
		if !state.meter.chargeFixedEntry(candidateName) {
			return state.mutated, errors.New("productmetrics: cleanup budget cannot park active-control collision blocker")
		}
		result, renameErr := state.root.renameEnumeratedEntry(active, retired, candidateName)
		if result.state != storageRenameNotApplied {
			state.mutated = true
			state.failClosedArmed = true
		}
		if errors.Is(renameErr, errStorageDestinationExists) && result.state == storageRenameNotApplied {
			continue
		}
		return validateRetiredControlRename(result, renameErr, "active-control collision parking")
	}
	// Reserving and persisting a fresh cursor incarnation is itself durable
	// progress. A pass whose entire block was occupied therefore stops cleanly;
	// the next pass reserves a disjoint inode-qualified namespace.
	return true, nil
}

type fallbackRelocationReservation struct {
	start  uint64
	slots  int
	cursor recordIncarnation
}

func (state *spoolSweepState) reserveFallbackRelocationBlock() (fallbackRelocationReservation, error) {
	if state == nil || state.root == nil || state.root.storageDir == nil || state.meter == nil {
		return fallbackRelocationReservation{}, errStorageClosed
	}
	if !state.meter.chargeFixedEntry(fallbackRelocationCursorName) {
		return fallbackRelocationReservation{}, errors.New("productmetrics: cleanup budget cannot inspect fallback relocation cursor")
	}

	entry, lookupErr := state.root.lookupEntry(fallbackRelocationCursorName)
	present := lookupErr == nil
	if lookupErr != nil && !errors.Is(lookupErr, fs.ErrNotExist) {
		return fallbackRelocationReservation{}, lookupErr
	}
	start := uint64(0)
	if present {
		if !safeFallbackRelocationCursor(entry) {
			return fallbackRelocationReservation{}, errors.New("productmetrics: unsafe fallback relocation cursor cannot reserve names")
		}
		if !state.meter.chargeFixedRead(maximumRelocationBytes + 1) {
			return fallbackRelocationReservation{}, errors.New("productmetrics: cleanup budget cannot read fallback relocation cursor")
		}
		data, readErr := state.root.readFile(fallbackRelocationCursorName, maximumRelocationBytes)
		cursor, decodeErr := decodeRelocationCursor(data)
		if readErr == nil && decodeErr == nil && cursor.Next <= maximumRelocationSequence-uint64(maximumRelocationSlots) {
			start = cursor.Next
		} else {
			// Corrupt and exhausted cursors recover without reusing their old
			// sequence block. The atomic replacement below also creates a fresh
			// inode, making the final candidate namespace disjoint on reopen.
			start = fallbackRelocationRecoveryStart(entry)
		}
	}
	worstCaseName := fallbackRelocationCandidateName(recordIncarnation{dev: math.MaxUint64, ino: math.MaxUint64}, maximumRelocationSequence)
	// One additional fixed name operation revalidates the cursor after its
	// atomic replacement. Reserve it together with all candidate attempts so a
	// logical meter can never persist a block that it cannot fully consume.
	available := state.meter.availableFixedSlots(uint64(len(worstCaseName)), maximumRelocationSlots+1)
	if available != maximumRelocationSlots+1 {
		return fallbackRelocationReservation{}, errors.New("productmetrics: cleanup budget cannot reserve a complete fallback relocation block")
	}
	slots := maximumRelocationSlots

	reserved := relocationCursor{Next: start + uint64(slots)}
	data, err := encodeRelocationCursor(reserved)
	if err != nil {
		return fallbackRelocationReservation{}, err
	}
	if !state.meter.chargeFixedRead(uint64(len(data))) {
		return fallbackRelocationReservation{}, errors.New("productmetrics: cleanup budget cannot write fallback relocation cursor")
	}
	result, writeErr := state.root.writeFileAtomicOutcome(fallbackRelocationCursorName, data)
	if result.state != storageWriteNotApplied {
		state.mutated = true
		state.failClosedArmed = true
	}
	if writeErr != nil {
		return fallbackRelocationReservation{}, writeErr
	}
	if result.state != storageWriteAppliedDurable {
		return fallbackRelocationReservation{}, errors.New("productmetrics: fallback relocation cursor reservation was not durable")
	}
	if !state.meter.chargeFixedEntry(fallbackRelocationCursorName) {
		return fallbackRelocationReservation{}, errors.New("productmetrics: cleanup budget cannot revalidate fallback relocation cursor")
	}
	current, err := state.root.lookupEntry(fallbackRelocationCursorName)
	if err != nil {
		return fallbackRelocationReservation{}, err
	}
	if !safeFallbackRelocationCursor(current) {
		return fallbackRelocationReservation{}, errors.New("productmetrics: fallback relocation cursor changed after reservation")
	}
	incarnation := recordIncarnation{dev: current.metadata.dev, ino: current.metadata.ino}
	if present && incarnation == (recordIncarnation{dev: entry.metadata.dev, ino: entry.metadata.ino}) {
		return fallbackRelocationReservation{}, errors.New("productmetrics: fallback relocation cursor replacement did not create a new incarnation")
	}
	if state.afterRelocationReservation != nil {
		if err := state.afterRelocationReservation(); err != nil {
			return fallbackRelocationReservation{}, fmt.Errorf("productmetrics: injected post-reservation failure: %w", err)
		}
	}
	return fallbackRelocationReservation{start: start, slots: slots, cursor: incarnation}, nil
}

func fallbackRelocationRecoveryStart(entry storageEntry) uint64 {
	span := maximumRelocationSequence - uint64(maximumRelocationSlots)
	return (entry.metadata.dev ^ entry.metadata.ino*0x9e3779b97f4a7c15) % (span + 1)
}

func fallbackRelocationCandidateName(cursor recordIncarnation, sequence uint64) string {
	return fmt.Sprintf(".pm-fallback-%x-%x-%016x", cursor.dev, cursor.ino, sequence)
}

func (state *spoolSweepState) rotateRetiredControlEntry(retired *storageDir, current storageEntry, candidateName string) (bool, storageEntry, bool, error) {
	if !state.meter.chargeFixedEntry(candidateName) {
		return false, storageEntry{}, false, errors.New("productmetrics: cleanup budget cannot rotate retired-control collision blocker")
	}
	result, renameErr := retired.renameEnumeratedDirectory(current, retired, candidateName)
	if result.state != storageRenameNotApplied {
		state.mutated = true
	}
	if !errors.Is(renameErr, errStorageDestinationExists) {
		progressed, err := validateRetiredControlRename(result, renameErr, "retired-control collision rotation")
		return progressed, storageEntry{}, false, err
	}
	if result.state != storageRenameNotApplied {
		return true, storageEntry{}, false, renameErr
	}

	occupant, lookupErr := retired.lookupEntry(candidateName)
	if lookupErr != nil {
		return false, storageEntry{}, false, lookupErr
	}
	if unlinkErr := retired.unlinkEnumeratedEntry(occupant); unlinkErr == nil {
		state.mutated = true
		return true, storageEntry{}, false, nil
	} else if !errors.Is(unlinkErr, errStorageEntryIsDirectory) {
		return false, storageEntry{}, false, unlinkErr
	}
	if removeErr := retired.removeEnumeratedCleanupDirectory(occupant); removeErr == nil {
		state.mutated = true
		return true, storageEntry{}, false, nil
	} else if !errors.Is(removeErr, errStorageDirectoryNotEmpty) {
		return false, storageEntry{}, false, removeErr
	}
	return false, occupant, true, nil
}

func validateRetiredControlRename(result storageRenameResult, err error, operation string) (bool, error) {
	progressed := result.state != storageRenameNotApplied
	if err != nil {
		return progressed, err
	}
	if result.state != storageRenameAppliedDurable {
		return progressed, fmt.Errorf("productmetrics: %s was not durable", operation)
	}
	return true, nil
}

func (state *spoolSweepState) retireRelocationCursor(control *storageDir, entry storageEntry, cause error) error {
	if entry.name == "" {
		return cause
	}
	return state.retireActiveControlNamespace(control, cause)
}

func (state *spoolSweepState) retireActiveControlNamespace(control *storageDir, cause error) error {
	if state == nil || state.root == nil {
		return errors.Join(cause, errStorageClosed)
	}
	if control != nil {
		if err := control.Close(); err != nil {
			return errors.Join(cause, err)
		}
		if state.retainedControl == control {
			state.retainedControl = nil
		}
	}
	entry, err := state.root.lookupEntry(spoolControlDirectoryName)
	if errors.Is(err, fs.ErrNotExist) {
		return cause
	}
	if err != nil {
		return errors.Join(cause, err)
	}
	if entry.metadata.kind != storageEntryDirectory {
		if !state.ensureAlternateControlBarrier(retiredControlDirectoryName) {
			return errors.Join(cause, errors.New("productmetrics: cannot remove active control without replacement fail-closed evidence"))
		}
		unlinkErr := state.root.unlinkEnumeratedEntry(entry)
		if unlinkErr == nil {
			state.mutated = true
			return nil
		}
		if errors.Is(unlinkErr, fs.ErrNotExist) {
			return nil
		}
		return errors.Join(cause, unlinkErr)
	}
	result, retireErr := state.root.renameEnumeratedDirectory(entry, state.root.storageDir, retiredControlDirectoryName)
	if result.state != storageRenameNotApplied {
		state.mutated = true
	}
	if errors.Is(retireErr, errStorageDestinationExists) {
		return nil
	}
	if retireErr != nil {
		return errors.Join(cause, retireErr)
	}
	if result.state != storageRenameAppliedDurable {
		return errors.Join(cause, errors.New("productmetrics: active control retirement was not durable"))
	}
	return nil
}

func (state *spoolSweepState) ensureConservativeRelocationQuota(control *storageDir) error {
	if !state.meter.chargeFixedEntry(quotaFileName) || !state.meter.chargeFixedRead(maximumQuotaBytes+1) {
		return errors.New("productmetrics: cleanup budget cannot inspect quota before relocation")
	}
	quota, present, loadErr := loadSpoolQuota(state.root)
	markers := spoolQuota{Events: maximumQuotaEventMarker, Bytes: maximumQuotaByteMarker}
	if loadErr == nil && present {
		if state.purgeAll {
			return nil
		}
		if quota == markers {
			state.relocationQuotaMarked = true
			return nil
		}
	}
	data, encodeErr := encodeSpoolQuota(markers)
	if encodeErr != nil {
		return errors.Join(loadErr, encodeErr)
	}
	if !state.meter.chargeFixedRead(uint64(len(data))) {
		return errors.Join(loadErr, errors.New("productmetrics: cleanup budget cannot write conservative relocation quota"))
	}
	if err := persistSpoolQuotaFromControl(state.root, control, markers, loadErr == nil && !present); err != nil {
		return errors.Join(loadErr, err)
	}
	state.mutated = true
	state.relocationQuotaMarked = true
	return nil
}

func (state *spoolSweepState) relocateQuarantineBlocker(quarantineRoot *storageDir, blocker storageEntry, eventTree bool) {
	targetName := fmt.Sprintf(".orphan-%x-%x", blocker.metadata.dev, blocker.metadata.ino)
	if !state.meter.chargeFixedEntry(targetName) {
		return
	}
	result, err := quarantineRoot.renameEnumeratedDirectory(blocker, quarantineRoot, targetName)
	if result.state != storageRenameNotApplied {
		state.mutated = true
	}
	if errors.Is(err, errStorageDestinationExists) {
		state.resolveQuarantineCollision(quarantineRoot, quarantineRoot, blocker, targetName, eventTree)
		return
	}
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	if result.state != storageRenameAppliedDurable {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: ancestor blocker relocation was not durable"))
	}
}

func (state *spoolSweepState) scanCurrentGeneration(treeName, generationName string, directory, quarantineRoot *storageDir) {
	if directory.cleanupOnly() {
		state.purgeDirectory(directory, quarantineRoot, true)
		return
	}
	if state.pruneDirs[treeName] == nil {
		retained, err := directory.openDir(nil, false)
		if err != nil {
			state.operation = errors.Join(state.operation, err)
			return
		}
		state.pruneDirs[treeName] = retained
	}
	iterator, err := directory.iterateEntries()
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	defer func() { state.operation = errors.Join(state.operation, iterator.Close()) }()
	for {
		entry, ok := state.meter.next(iterator)
		if !ok {
			return
		}
		if entry.metadata.kind != storageEntryDirectory {
			if !state.meter.chargeEventEntry() {
				return
			}
			state.scanCurrentLeaf(treeName, generationName, directory, entry)
			continue
		}
		if !state.meter.chargeCleanupDirectory() {
			state.quarantineDirectory(directory, quarantineRoot, entry, false, true)
			return
		}
		child, openErr := openEnumeratedStorageDirectory(directory, entry)
		if openErr == nil {
			state.purgeDirectory(child, quarantineRoot, true)
			state.operation = errors.Join(state.operation, child.Close())
			if !state.meter.exhausted {
				if !state.ensureFailClosedControl() {
					return
				}
				if err := directory.removeEnumeratedCleanupDirectory(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
					state.operation = errors.Join(state.operation, err)
				} else if err == nil {
					state.mutated = true
				}
			}
			continue
		}
		state.operation = errors.Join(state.operation, directoryDescriptorExhaustion(openErr))
		state.meter.refundLogicalDirectoryCharge()
		if !state.meter.chargeEventEntry() {
			return
		}
		state.scanCurrentLeaf(treeName, generationName, directory, entry)
	}
}

func directoryDescriptorExhaustion(err error) error {
	if errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) {
		return err
	}
	return nil
}

func (state *spoolSweepState) scanCurrentLeaf(treeName, generationName string, directory *storageDir, entry storageEntry) {
	eventID, validName := eventIDFromFileName(entry.name)
	if !validName || entry.metadata.size < 0 || uint64(entry.metadata.size) > maximumEventBytes {
		state.deleteLeaf(directory, entry, true)
		return
	}
	const readReservation = maximumEventBytes + 1
	if !state.meter.chargeRead(readReservation) {
		state.makeOverflowProgress(directory, entry)
		return
	}
	data, physicalReadBytes, lease, err := directory.readFileMeasured(entry.name, int64(maximumEventBytes))
	if lease != nil {
		err = errors.Join(err, lease.Close())
	}
	state.meter.refundRead(readReservation, physicalReadBytes)
	if err != nil {
		state.deleteLeaf(directory, entry, true)
		return
	}
	event, err := DecodeEvent(data)
	if err != nil || event.EventID != eventID || event.InstallationID != state.policy.installationID {
		state.deleteLeaf(directory, entry, true)
		return
	}
	canonical, err := EncodeEvent(event)
	if err != nil || !bytes.Equal(canonical, data) {
		state.deleteLeaf(directory, entry, true)
		return
	}
	occurred, err := parseCanonicalHourUTC(event.OccurredHourUTC)
	if err != nil || !eventWithinRetention(occurred, state.now) {
		state.deleteLeaf(directory, entry, true)
		return
	}
	record := spoolRecord{
		tree: treeName, generation: generationName, name: entry.name, event: event,
		bytes: uint64(len(data)), mtimeSeconds: entry.metadata.mtimeSeconds, mtimeNanoseconds: entry.metadata.mtimeNanoseconds,
	}
	if _, duplicate := state.seen[event.EventID]; duplicate {
		existing := -1
		for index := range state.records {
			if state.records[index].event.EventID == event.EventID {
				existing = index
				break
			}
		}
		if existing < 0 || !spoolRecordLess(record, state.records[existing]) {
			state.deleteLeaf(directory, entry, true)
			return
		}
		if !state.deleteValidRecord(existing) {
			state.deleteLeaf(directory, entry, true)
			return
		}
	} else {
		state.seen[event.EventID] = struct{}{}
	}
	state.records = append(state.records, record)
	events, eventsOK := checkedAddUint64(state.quota.Events, 1)
	bytes, bytesOK := checkedAddUint64(state.quota.Bytes, record.bytes)
	if !eventsOK || !bytesOK {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: reconciled quota overflow"))
		return
	}
	state.quota = spoolQuota{Events: events, Bytes: bytes}
}

func (state *spoolSweepState) makeOverflowProgress(current *storageDir, entry storageEntry) {
	if !state.ensureFailClosedControl() {
		state.addConservativeEntry(entry)
		return
	}
	currentRecord := spoolRecord{
		name:             entry.name,
		mtimeSeconds:     entry.metadata.mtimeSeconds,
		mtimeNanoseconds: entry.metadata.mtimeNanoseconds,
	}
	oldest := -1
	for index := range state.records {
		if oldest == -1 || spoolRecordLess(state.records[index], state.records[oldest]) {
			oldest = index
		}
	}
	if oldest == -1 || spoolRecordLess(currentRecord, state.records[oldest]) {
		if err := current.unlinkEnumeratedEntry(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
			state.operation = errors.Join(state.operation, err)
		} else {
			if err == nil {
				state.mutated = true
			}
			state.noteRemoved(entry.metadata.size)
		}
		return
	}
	state.deleteValidRecord(oldest)
}

func eventWithinRetention(occurred, now time.Time) bool {
	occurred = occurred.UTC().Truncate(time.Hour)
	now = now.UTC().Truncate(time.Hour)
	if occurred.After(now) {
		return false
	}
	cutoff := now.Add(-maximumEventAgeHours * time.Hour)
	return !occurred.Before(cutoff)
}

func (state *spoolSweepState) deleteLeaf(directory *storageDir, entry storageEntry, eventTree bool) {
	if !state.ensureFailClosedControl() {
		if eventTree {
			state.addConservativeEntry(entry)
		}
		return
	}
	if err := directory.unlinkEnumeratedEntry(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
		state.operation = errors.Join(state.operation, err)
		if eventTree {
			state.addConservativeEntry(entry)
		}
	} else {
		if err == nil {
			state.mutated = true
		}
		if eventTree {
			state.noteRemoved(entry.metadata.size)
		}
	}
}

func (state *spoolSweepState) noteRemoved(size int64) {
	if state.removedEvents < math.MaxUint64 {
		state.removedEvents++
	}
	if size <= 0 || state.removedBytes == math.MaxUint64 {
		return
	}
	bytes, ok := checkedAddUint64(state.removedBytes, uint64(size))
	if !ok {
		state.removedBytes = math.MaxUint64
		return
	}
	state.removedBytes = bytes
}

func (state *spoolSweepState) addConservativeEntry(entry storageEntry) {
	bytes := uint64(0)
	if entry.metadata.size < 0 || uint64(entry.metadata.size) > maximumQuotaByteMarker {
		bytes = maximumQuotaByteMarker
	} else {
		bytes = uint64(entry.metadata.size)
	}
	state.addQuota(spoolQuota{Events: 1, Bytes: bytes})
}

func (state *spoolSweepState) addQuota(add spoolQuota) {
	if add.Events >= maximumQuotaEventMarker || state.quota.Events >= maximumQuotaEventMarker-add.Events {
		state.quota.Events = maximumQuotaEventMarker
	} else {
		state.quota.Events += add.Events
	}
	if add.Bytes >= maximumQuotaByteMarker || state.quota.Bytes >= maximumQuotaByteMarker-add.Bytes {
		state.quota.Bytes = maximumQuotaByteMarker
	} else {
		state.quota.Bytes += add.Bytes
	}
}

func (state *spoolSweepState) pruneOldestToQuota() {
	for (state.quota.Events > maximumSpoolEvents || state.quota.Bytes > maximumSpoolBytes) && len(state.records) > 0 {
		oldest := 0
		for index := 1; index < len(state.records); index++ {
			if spoolRecordLess(state.records[index], state.records[oldest]) {
				oldest = index
			}
		}
		if !state.deleteValidRecord(oldest) {
			return
		}
	}
}

func (state *spoolSweepState) deleteValidRecord(index int) bool {
	if index < 0 || index >= len(state.records) {
		return false
	}
	record := state.records[index]
	directory := state.pruneDirs[record.tree]
	if directory == nil {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: missing retained generation handle for pruning"))
		return false
	}
	if !state.ensureFailClosedControl() {
		return false
	}
	if err := directory.removeFile(record.name); err != nil {
		state.operation = errors.Join(state.operation, err)
		return false
	}
	state.mutated = true
	state.quota.Events--
	state.quota.Bytes -= record.bytes
	state.noteRemoved(int64(record.bytes))
	copy(state.records[index:], state.records[index+1:])
	state.records = state.records[:len(state.records)-1]
	return true
}

func (state *spoolSweepState) finish() (spoolSweepResult, error) {
	if state.restoreDirectoryOpenHooks != nil {
		defer state.restoreDirectoryOpenHooks()
	}
	for tree, directory := range state.pruneDirs {
		state.operation = errors.Join(state.operation, directory.Close())
		delete(state.pruneDirs, tree)
	}
	// Mutating a directory while a live getdents/readdir iterator is open can
	// make that iterator skip a later entry on some filesystems. A successful
	// tree mutation therefore makes this pass progress-only; only a subsequent
	// bounded mutation-free pass may certify exact quota or an empty spool.
	complete := state.traversed && !state.mutated && !state.meter.exhausted && state.meter.traversalError == nil && state.operation == nil
	target := state.quota
	if state.purgeAll && complete {
		target = spoolQuota{}
	}
	persistQuota := true
	if !complete {
		if state.purgeAll {
			// Consent cleanup leaves the existing conservative reservation
			// untouched until the event tree is proven empty. In particular, do
			// not spend bytes outside the one global cleanup budget rereading it.
			persistQuota = false
		} else {
			target.Events = maximumQuotaEventMarker
			target.Bytes = maximumQuotaByteMarker
			if state.relocationQuotaMarked || state.failClosedArmed {
				persistQuota = false
			}
		}
	}
	var persistErr error
	if persistQuota {
		switch {
		case !state.meter.claimFixedWorkEnvelope():
			persistErr = errors.New("productmetrics: cleanup budget cannot reserve fixed quota persistence work")
		case state.retainedControl != nil:
			persistErr = state.persistQuotaFromRetainedControl(target)
		case !state.meter.chargeFixedDirectory():
			persistErr = errors.New("productmetrics: cleanup budget cannot open quota staging directory")
		default:
			persistErr = persistSpoolQuota(state.root, target)
		}
	}
	if state.retainedControl != nil {
		persistErr = errors.Join(persistErr, state.retainedControl.Close())
		state.retainedControl = nil
	}
	result := spoolSweepResult{
		complete: complete && persistErr == nil, usage: state.meter.usage, quota: target,
		removedEvents: state.removedEvents, removedBytes: state.removedBytes,
	}
	err := errors.Join(state.meter.traversalError, state.operation, persistErr)
	return result, err
}

func (state *spoolSweepState) persistQuotaFromRetainedControl(quota spoolQuota) error {
	control := state.retainedControl
	if control == nil {
		return errStorageClosed
	}
	persistErr := persistSpoolQuotaFromControl(state.root, control, quota, false)
	closeErr := control.Close()
	state.retainedControl = nil
	if persistErr != nil || closeErr != nil {
		return errors.Join(persistErr, closeErr)
	}
	entry, err := state.root.lookupEntry(spoolControlDirectoryName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	removeErr := state.root.removeEnumeratedCleanupDirectory(entry)
	if errors.Is(removeErr, errStorageDirectoryNotEmpty) || errors.Is(removeErr, fs.ErrNotExist) {
		return nil
	}
	return removeErr
}

func sortSpoolRecords(records []spoolRecord) {
	sort.Slice(records, func(left, right int) bool {
		return spoolRecordLess(records[left], records[right])
	})
}

func spoolRecordLess(left, right spoolRecord) bool {
	if left.mtimeSeconds != right.mtimeSeconds {
		return left.mtimeSeconds < right.mtimeSeconds
	}
	if left.mtimeNanoseconds != right.mtimeNanoseconds {
		return left.mtimeNanoseconds < right.mtimeNanoseconds
	}
	return left.name < right.name
}

// claimSpoolBatch is a caller-held uploader.lock-then-state.lock primitive.
// The caller revalidates permit against the current config record before entry;
// the returned claim contains one oldest-first, same-release generation batch.
func claimSpoolBatch(root *storageRoot, permit RecordingPermit, now time.Time, budget spoolWorkBudget) (spoolClaim, error) {
	if !permit.Valid() {
		return spoolClaim{}, errors.New("productmetrics: invalid permit cannot claim a spool batch")
	}
	state := runSpoolSweep(root, policyFromPermit(permit), now, budget, false)
	result, err := state.finish()
	if err != nil {
		return spoolClaim{}, err
	}
	if !result.complete {
		return spoolClaim{}, nil
	}

	queue, err := root.openDir([]string{queueDirectoryName, permit.spoolGeneration}, true)
	if err != nil {
		return spoolClaim{}, err
	}
	defer func() { _ = queue.Close() }()
	inflight, err := root.openDir([]string{inflightDirectoryName, permit.spoolGeneration}, true)
	if err != nil {
		return spoolClaim{}, err
	}
	defer func() { _ = inflight.Close() }()
	for index := range state.records {
		record := &state.records[index]
		if record.tree != inflightDirectoryName {
			continue
		}
		result, renameErr := inflight.renameFile(record.name, queue, record.name)
		if renameErr != nil {
			if errors.Is(renameErr, errStorageDestinationExists) {
				if removeErr := inflight.removeFile(record.name); removeErr != nil {
					return spoolClaim{}, errors.Join(renameErr, removeErr)
				}
				continue
			}
			return spoolClaim{}, renameErr
		}
		if result.state != storageRenameAppliedDurable {
			return spoolClaim{}, errors.New("productmetrics: inflight restore was not durable")
		}
		record.tree = queueDirectoryName
	}

	candidates := state.records[:0]
	for _, record := range state.records {
		if record.tree == queueDirectoryName {
			candidates = append(candidates, record)
		}
	}
	sortSpoolRecords(candidates)
	claim := spoolClaim{generation: permit.spoolGeneration}
	batchRelease := ""
	for _, record := range candidates {
		if len(claim.records) >= maximumBatchEvents {
			break
		}
		if batchRelease == "" {
			batchRelease = record.event.ReleaseVersion
		} else if record.event.ReleaseVersion != batchRelease {
			break
		}
		candidateRecords := append(append([]spoolRecord(nil), claim.records...), record)
		candidateEvents := make([]Event, len(candidateRecords))
		for index := range candidateRecords {
			candidateEvents[index] = candidateRecords[index].event
		}
		encoded, encodeErr := EncodeBatch(Batch{SchemaVersion: SchemaVersionV1, Events: candidateEvents})
		if encodeErr != nil {
			return spoolClaim{}, encodeErr
		}
		if len(encoded) > maximumRequestBytes {
			break
		}
		claim.records = candidateRecords
	}
	if len(claim.records) == 0 {
		return claim, nil
	}
	claimed := spoolClaim{generation: claim.generation, authority: &spoolClaimAuthority{}}
	for _, record := range claim.records {
		result, renameErr := queue.renameFile(record.name, inflight, record.name)
		if result.state != storageRenameNotApplied {
			record.tree = inflightDirectoryName
			claimed.records = append(claimed.records, record)
		}
		if renameErr != nil || result.state != storageRenameAppliedDurable {
			restoreErr := restoreSpoolClaim(root, claimed)
			if renameErr == nil {
				renameErr = errors.New("productmetrics: queue claim was not durable")
			}
			return spoolClaim{}, errors.Join(renameErr, restoreErr)
		}
	}
	return claimed, nil
}

// restoreSpoolClaim is a caller-held uploader.lock-then-state.lock primitive.
// Settlement authority is shared by copied claims, so restore and delete can
// never both consume the same claim in one process.
func restoreSpoolClaim(root *storageRoot, claim spoolClaim) error {
	if root == nil || !validCanonicalUUIDv4(claim.generation) {
		return errors.New("productmetrics: invalid spool claim")
	}
	settle, err := claim.beginSettlement()
	if err != nil {
		return err
	}
	defer settle()
	queue, err := root.openDir([]string{queueDirectoryName, claim.generation}, true)
	if err != nil {
		return err
	}
	defer func() { _ = queue.Close() }()
	inflight, err := root.openDir([]string{inflightDirectoryName, claim.generation}, true)
	if err != nil {
		return err
	}
	defer func() { _ = inflight.Close() }()
	var restoreErr error
	for _, record := range claim.records {
		if record.generation != claim.generation || record.name != eventFileName(record.event.EventID) ||
			record.bytes == 0 || record.bytes > maximumEventBytes {
			restoreErr = errors.Join(restoreErr, errors.New("productmetrics: malformed claimed record"))
			continue
		}
		result, err := inflight.renameFile(record.name, queue, record.name)
		if err == nil && result.state == storageRenameAppliedDurable {
			continue
		}
		if errors.Is(err, errStorageDestinationExists) {
			err = inflight.removeFile(record.name)
			if err == nil || errors.Is(err, fs.ErrNotExist) {
				continue
			}
		}
		if errors.Is(err, fs.ErrNotExist) && queuedRecordMatches(queue, record) {
			continue
		}
		if err == nil {
			err = errors.New("productmetrics: claim restore was not durable")
		}
		restoreErr = errors.Join(restoreErr, err)
	}
	return restoreErr
}

func queuedRecordMatches(queue *storageDir, record spoolRecord) bool {
	data, err := queue.readFile(record.name, int64(maximumEventBytes))
	if err != nil {
		return false
	}
	want, err := EncodeEvent(record.event)
	return err == nil && bytes.Equal(data, want)
}

// deleteSpoolClaim is a caller-held uploader.lock-then-state.lock primitive.
// Files are durably removed before quota is lowered; every uncertain window
// therefore leaves a conservative overcount for reconciliation.
func deleteSpoolClaim(root *storageRoot, claim spoolClaim) error {
	if root == nil || !validCanonicalUUIDv4(claim.generation) {
		return errors.New("productmetrics: invalid spool claim")
	}
	settle, err := claim.beginSettlement()
	if err != nil {
		return err
	}
	defer settle()
	inflight, err := root.openDir([]string{inflightDirectoryName, claim.generation}, false)
	if errors.Is(err, fs.ErrNotExist) && len(claim.records) == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = inflight.Close() }()
	seen := make(map[string]struct{}, len(claim.records))
	released := spoolQuota{}
	var deleteErr error
	for _, record := range claim.records {
		if record.generation != claim.generation || record.name != eventFileName(record.event.EventID) ||
			record.bytes == 0 || record.bytes > maximumEventBytes {
			deleteErr = errors.Join(deleteErr, errors.New("productmetrics: malformed claimed record"))
			continue
		}
		if _, duplicate := seen[record.name]; duplicate {
			deleteErr = errors.Join(deleteErr, errors.New("productmetrics: duplicate claimed record"))
			continue
		}
		seen[record.name] = struct{}{}
		removed, err := deleteOneClaimedRecord(root, inflight, claim.generation, record)
		if err != nil {
			deleteErr = errors.Join(deleteErr, err)
			continue
		}
		if !removed {
			continue
		}
		bytes, ok := checkedAddUint64(released.Bytes, record.bytes)
		if !ok {
			deleteErr = errors.Join(deleteErr, errors.New("productmetrics: claimed-byte release overflow"))
			continue
		}
		released.Events++
		released.Bytes = bytes
	}
	if released.Events == 0 && released.Bytes == 0 {
		return deleteErr
	}
	quota, present, err := loadSpoolQuota(root)
	if err != nil {
		return errors.Join(deleteErr, err)
	}
	if !present {
		return errors.Join(deleteErr, errors.New("productmetrics: quota is absent while settling a spool claim"))
	}
	quota, err = quota.release(released.Events, released.Bytes)
	if err != nil {
		return errors.Join(deleteErr, err)
	}
	return errors.Join(deleteErr, persistSpoolQuota(root, quota))
}

func deleteOneClaimedRecord(root *storageRoot, inflight *storageDir, generation string, record spoolRecord) (bool, error) {
	data, lease, readErr := inflight.readFileLease(record.name, int64(maximumEventBytes))
	closeLease := func() error {
		if lease == nil {
			return nil
		}
		return lease.Close()
	}
	switch {
	case errors.Is(readErr, fs.ErrNotExist):
		if closeErr := closeLease(); closeErr != nil {
			return false, closeErr
		}
		if err := inflight.confirmEntryAbsent(record.name); err != nil {
			return false, err
		}
		restored, err := missingClaimQueueDisposition(root, generation, record)
		if err != nil {
			return false, err
		}
		return !restored, nil
	case readErr != nil:
		return false, errors.Join(readErr, closeLease())
	}
	want, encodeErr := EncodeEvent(record.event)
	if encodeErr != nil || uint64(len(data)) != record.bytes || !bytes.Equal(data, want) {
		return false, errors.Join(encodeErr, errors.New("productmetrics: claimed event changed before deletion"), closeLease())
	}
	if lease == nil {
		return false, errors.New("productmetrics: claimed event read returned no record lease")
	}
	removeErr := inflight.removeFileMatchingLease(record.name, lease)
	closeErr := lease.Close()
	if removeErr != nil || closeErr != nil {
		return false, errors.Join(removeErr, closeErr)
	}
	return true, nil
}

func missingClaimQueueDisposition(root *storageRoot, generation string, record spoolRecord) (restored bool, err error) {
	queueRoot, err := root.openDir([]string{queueDirectoryName}, false)
	if errors.Is(err, fs.ErrNotExist) {
		return false, root.confirmEntryAbsent(queueDirectoryName)
	}
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, queueRoot.Close()) }()

	queue, err := queueRoot.openDir([]string{generation}, false)
	if errors.Is(err, fs.ErrNotExist) {
		return false, queueRoot.confirmEntryAbsent(generation)
	}
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, queue.Close()) }()

	data, readErr := queue.readFile(record.name, int64(maximumEventBytes))
	if errors.Is(readErr, fs.ErrNotExist) {
		return false, queue.confirmEntryAbsent(record.name)
	}
	if readErr != nil {
		return false, readErr
	}
	want, encodeErr := EncodeEvent(record.event)
	if encodeErr != nil {
		return false, encodeErr
	}
	if !bytes.Equal(data, want) {
		return false, errors.New("productmetrics: restored queue event changed before settlement")
	}
	return true, nil
}

// String returns the bounded recording outcome name.
func (result RecordResult) String() string {
	switch result {
	case RecordDropped:
		return "dropped"
	case RecordStored:
		return "stored"
	default:
		return fmt.Sprintf("RecordResult(%d)", result)
	}
}
