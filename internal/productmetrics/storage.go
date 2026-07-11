package productmetrics

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/gastownhall/gascity/internal/gchome"
)

var (
	errStorageDestinationExists = errors.New("productmetrics: rename destination already exists")
	errStorageEntryExists       = errors.New("productmetrics: storage entry already exists")
	errStorageEntryChanged      = errors.New("productmetrics: enumerated storage entry changed")
	errStorageClosed            = errors.New("productmetrics: storage handle is closed")
)

type storageStep string

const (
	storageStepFileSync      storageStep = "file-sync"
	storageStepWrite         storageStep = "write"
	storageStepRename        storageStep = "rename"
	storageStepInstall       storageStep = "install-no-replace"
	storageStepDelete        storageStep = "delete"
	storageStepEnumerate     storageStep = "enumerate"
	storageStepEntryStat     storageStep = "entry-stat"
	storageStepUnlink        storageStep = "unlink"
	storageStepRmdir         storageStep = "rmdir"
	storageStepDirectorySync storageStep = "directory-sync"
	storageStepLock          storageStep = "lock"
)

type storageMetadata struct {
	uid              uint32
	mode             uint32
	nlink            uint64
	dev              uint64
	ino              uint64
	size             int64
	mtimeSeconds     int64
	mtimeNanoseconds int64
}

type storageEntry struct {
	name      string
	nameBytes int
	metadata  storageMetadata
}

type storageRenameState uint8

const (
	storageRenameNotApplied storageRenameState = iota
	storageRenameAppliedSyncPending
	storageRenameAppliedDurable
)

type storageRenameResult struct {
	state storageRenameState
}

type storageWriteState uint8

const (
	storageWriteNotApplied storageWriteState = iota
	storageWriteAppliedSyncPending
	storageWriteAppliedDurable
)

type storageWriteResult struct {
	state storageWriteState
}

type recordIncarnation struct {
	dev uint64
	ino uint64
}

type storageRecordBackend interface {
	close() error
}

// storageRecordLease retains the validated descriptor for one exact atomic
// config record. Keeping that descriptor open prevents its inode from being
// reused while stale in-process authority still exists.
type storageRecordLease struct {
	mu      sync.Mutex
	backend storageRecordBackend
	record  recordIncarnation
}

func newStorageRecordLease(backend storageRecordBackend, metadata storageMetadata) *storageRecordLease {
	if backend == nil {
		return nil
	}
	lease := &storageRecordLease{
		backend: backend,
		record:  recordIncarnation{dev: metadata.dev, ino: metadata.ino},
	}
	runtime.SetFinalizer(lease, func(retained *storageRecordLease) { _ = retained.Close() })
	return lease
}

func (lease *storageRecordLease) Close() error {
	if lease == nil {
		return nil
	}
	lease.mu.Lock()
	backend := lease.backend
	lease.backend = nil
	lease.mu.Unlock()
	if backend == nil {
		return nil
	}
	runtime.SetFinalizer(lease, nil)
	return backend.close()
}

func (lease *storageRecordLease) Valid() bool {
	if lease == nil {
		return false
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.backend != nil
}

func (lease *storageRecordLease) incarnation() recordIncarnation {
	if lease == nil {
		return recordIncarnation{}
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.backend == nil {
		return recordIncarnation{}
	}
	return lease.record
}

func (lease *storageRecordLease) Matches(other *storageRecordLease) bool {
	if lease == nil || other == nil {
		return false
	}
	left := lease.incarnation()
	right := other.incarnation()
	return left != (recordIncarnation{}) && left == right
}

// storageTestHooks is deliberately package-private. No external construction
// path can weaken validation or inject filesystem behavior.
type storageTestHooks struct {
	beforeStep         func(storageStep) error
	afterComponentOpen func(string)
	metadata           func(string, storageMetadata) storageMetadata
}

func (hooks storageTestHooks) run(step storageStep) error {
	if hooks.beforeStep == nil {
		return nil
	}
	return hooks.beforeStep(step)
}

func (hooks storageTestHooks) openedComponent(path string) {
	if hooks.afterComponentOpen != nil {
		hooks.afterComponentOpen(path)
	}
}

func (hooks storageTestHooks) inspect(path string, metadata storageMetadata) storageMetadata {
	if hooks.metadata != nil {
		return hooks.metadata(path, metadata)
	}
	return metadata
}

type storageDirectoryBackend interface {
	close() error
	openDir([]string, bool) (storageDirectoryBackend, error)
	readFile(string, int64) ([]byte, error)
	readFileLease(string, int64) ([]byte, storageRecordBackend, storageMetadata, error)
	writeFileAtomic(string, []byte) error
	writeFileAtomicOutcome(string, []byte) (storageWriteResult, error)
	writeFileAtomicNoReplace(string, []byte) error
	removeFile(string) error
	renameFile(string, storageDirectoryBackend, string) (storageRenameResult, error)
	syncDirectory() error
	iterateEntries() (storageIteratorBackend, error)
	unlinkEnumeratedEntry(storageEntry) error
	removeEnumeratedDirectory(storageEntry) error
	acquireLock(context.Context, string) (storageLockBackend, error)
}

type storageIteratorBackend interface {
	next() (storageEntry, error)
	close() error
}

type storageLockBackend interface {
	release() error
}

type storageRoot struct {
	*storageDir
}

type storageDir struct {
	backend storageDirectoryBackend
}

type advisoryLock struct {
	backend storageLockBackend
}

type storageIterator struct {
	backend storageIteratorBackend
}

func openStorageRootReadOnly(home gchome.ProductUsageHome) (*storageRoot, error) {
	return openStorageRoot(home, false, storageTestHooks{})
}

func openStorageRootMutable(home gchome.ProductUsageHome) (*storageRoot, error) {
	return openStorageRoot(home, true, storageTestHooks{})
}

func openStorageRootMutableWithHooks(home gchome.ProductUsageHome, hooks storageTestHooks) (*storageRoot, error) {
	return openStorageRoot(home, true, hooks)
}

func openStorageRoot(home gchome.ProductUsageHome, mutable bool, hooks storageTestHooks) (*storageRoot, error) {
	backend, err := platformOpenStorageRoot(home, mutable, hooks)
	if err != nil {
		return nil, err
	}
	return &storageRoot{storageDir: &storageDir{backend: backend}}, nil
}

func (directory *storageDir) Close() error {
	if directory == nil || directory.backend == nil {
		return nil
	}
	return directory.backend.close()
}

func (directory *storageDir) openDir(components []string, create bool) (*storageDir, error) {
	if directory == nil || directory.backend == nil {
		return nil, errStorageClosed
	}
	for _, component := range components {
		if err := validateStorageName(component); err != nil {
			return nil, fmt.Errorf("productmetrics: invalid directory component: %w", err)
		}
	}
	backend, err := directory.backend.openDir(components, create)
	if err != nil {
		return nil, err
	}
	return &storageDir{backend: backend}, nil
}

func (directory *storageDir) readFile(name string, maximumBytes int64) ([]byte, error) {
	data, lease, err := directory.readFileLease(name, maximumBytes)
	if lease != nil {
		err = errors.Join(err, lease.Close())
	}
	return data, err
}

func (directory *storageDir) readFileLease(name string, maximumBytes int64) ([]byte, *storageRecordLease, error) {
	if directory == nil || directory.backend == nil {
		return nil, nil, errStorageClosed
	}
	if err := validateStorageName(name); err != nil {
		return nil, nil, err
	}
	if maximumBytes <= 0 {
		return nil, nil, errors.New("productmetrics: read size limit must be positive")
	}
	data, backend, metadata, err := directory.backend.readFileLease(name, maximumBytes)
	return data, newStorageRecordLease(backend, metadata), err
}

func (directory *storageDir) writeFileAtomic(name string, data []byte) error {
	if directory == nil || directory.backend == nil {
		return errStorageClosed
	}
	if err := validateMutableStorageName(name); err != nil {
		return err
	}
	return directory.backend.writeFileAtomic(name, data)
}

func (directory *storageDir) writeFileAtomicOutcome(name string, data []byte) (storageWriteResult, error) {
	if directory == nil || directory.backend == nil {
		return storageWriteResult{state: storageWriteNotApplied}, errStorageClosed
	}
	if err := validateMutableStorageName(name); err != nil {
		return storageWriteResult{state: storageWriteNotApplied}, err
	}
	return directory.backend.writeFileAtomicOutcome(name, data)
}

func (directory *storageDir) writeFileAtomicNoReplace(name string, data []byte) error {
	if directory == nil || directory.backend == nil {
		return errStorageClosed
	}
	if err := validateMutableStorageName(name); err != nil {
		return err
	}
	return directory.backend.writeFileAtomicNoReplace(name, data)
}

func (directory *storageDir) removeFile(name string) error {
	if directory == nil || directory.backend == nil {
		return errStorageClosed
	}
	if err := validateMutableStorageName(name); err != nil {
		return err
	}
	return directory.backend.removeFile(name)
}

func (directory *storageDir) renameFile(name string, target *storageDir, targetName string) (storageRenameResult, error) {
	if directory == nil || directory.backend == nil || target == nil || target.backend == nil {
		return storageRenameResult{state: storageRenameNotApplied}, errStorageClosed
	}
	if err := validateMutableStorageName(name); err != nil {
		return storageRenameResult{state: storageRenameNotApplied}, err
	}
	if err := validateMutableStorageName(targetName); err != nil {
		return storageRenameResult{state: storageRenameNotApplied}, err
	}
	return directory.backend.renameFile(name, target.backend, targetName)
}

func (directory *storageDir) syncDirectory() error {
	if directory == nil || directory.backend == nil {
		return errStorageClosed
	}
	return directory.backend.syncDirectory()
}

func (directory *storageDir) iterateEntries() (*storageIterator, error) {
	if directory == nil || directory.backend == nil {
		return nil, errStorageClosed
	}
	backend, err := directory.backend.iterateEntries()
	if err != nil {
		return nil, err
	}
	return &storageIterator{backend: backend}, nil
}

func (iterator *storageIterator) Next() (storageEntry, error) {
	if iterator == nil || iterator.backend == nil {
		return storageEntry{}, errStorageClosed
	}
	return iterator.backend.next()
}

func (iterator *storageIterator) Close() error {
	if iterator == nil || iterator.backend == nil {
		return nil
	}
	return iterator.backend.close()
}

func (directory *storageDir) unlinkEnumeratedEntry(entry storageEntry) error {
	if directory == nil || directory.backend == nil {
		return errStorageClosed
	}
	if err := validateEnumeratedEntry(entry); err != nil {
		return err
	}
	return directory.backend.unlinkEnumeratedEntry(entry)
}

func (directory *storageDir) removeEnumeratedDirectory(entry storageEntry) error {
	if directory == nil || directory.backend == nil {
		return errStorageClosed
	}
	if err := validateEnumeratedEntry(entry); err != nil {
		return err
	}
	return directory.backend.removeEnumeratedDirectory(entry)
}

func validateEnumeratedEntry(entry storageEntry) error {
	if entry.name == "" || entry.name == "." || entry.name == ".." || entry.nameBytes != len(entry.name) {
		return errors.New("productmetrics: invalid enumerated entry name")
	}
	for index := range len(entry.name) {
		if entry.name[index] == 0 || entry.name[index] == '/' {
			return errors.New("productmetrics: invalid enumerated entry name")
		}
	}
	return nil
}

func (directory *storageDir) acquireLock(ctx context.Context, name string) (*advisoryLock, error) {
	if directory == nil || directory.backend == nil {
		return nil, errStorageClosed
	}
	if ctx == nil {
		return nil, errors.New("productmetrics: lock context is nil")
	}
	if !isStorageLockName(name) {
		return nil, fmt.Errorf("productmetrics: unrecognized lock name %q", name)
	}
	backend, err := directory.backend.acquireLock(ctx, name)
	if err != nil {
		return nil, err
	}
	return &advisoryLock{backend: backend}, nil
}

func (lock *advisoryLock) Release() error {
	if lock == nil || lock.backend == nil {
		return nil
	}
	return lock.backend.release()
}

func validateStorageName(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("productmetrics: invalid empty or relative storage name %q", name)
	}
	if len(name) > 128 {
		return fmt.Errorf("productmetrics: storage name exceeds 128 bytes")
	}
	for index := range len(name) {
		if name[index] < 0x21 || name[index] > 0x7e || name[index] == '/' || name[index] == '\\' {
			return fmt.Errorf("productmetrics: storage name contains a forbidden byte")
		}
	}
	return nil
}

func validateMutableStorageName(name string) error {
	if err := validateStorageName(name); err != nil {
		return err
	}
	if isStorageLockName(name) {
		return fmt.Errorf("productmetrics: stable lock inode %q cannot be replaced or removed", name)
	}
	return nil
}

func isStorageLockName(name string) bool {
	return name == "state.lock" || name == "uploader.lock"
}

func storagePathError(operation, path string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("productmetrics: %s %q: %w", operation, path, fs.ErrNotExist)
	}
	return fmt.Errorf("productmetrics: %s %q: %w", operation, path, err)
}

func isCleanAbsoluteProductRoot(home gchome.ProductUsageHome) bool {
	path := home.Home().Path()
	return home.Home().Provenance().Stable() && filepath.IsAbs(path) &&
		filepath.Clean(path) == path && home.Root() == filepath.Join(path, "product-usage")
}
