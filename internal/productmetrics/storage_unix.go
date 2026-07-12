//go:build (linux && !android) || (darwin && !ios)

package productmetrics

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gastownhall/gascity/internal/gchome"
	"golang.org/x/sys/unix"
)

const (
	unixDirectoryOpenFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	unixFileReadFlags      = unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	unixFileWriteFlags     = unix.O_WRONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
)

var storageTempSequence atomic.Uint64

type unixStorageDirectory struct {
	mu            sync.Mutex
	fd            int
	path          string
	euid          uint32
	mutable       bool
	rootDirectory bool
	cleanupOnly   bool
	hooks         storageTestHooks
}

type unixStorageIterator struct {
	mu          sync.Mutex
	file        *os.File
	path        string
	euid        uint32
	cleanupOnly bool
	hooks       storageTestHooks
	pendingName string
}

type unixStorageRecordLease struct {
	mu sync.Mutex
	fd int
}

func (directory *unixStorageDirectory) cleanupOnlyHandle() bool {
	return directory != nil && directory.cleanupOnly
}

func (directory *unixStorageDirectory) fileDescriptorSoftLimit() (uint64, error) {
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		return 0, fmt.Errorf("productmetrics: read file-descriptor limit: %w", err)
	}
	return limit.Cur, nil
}

func (directory *unixStorageDirectory) installDirectoryOpenHooks(before func(string) error, after func(string)) func() {
	directory.mu.Lock()
	originalBefore := directory.hooks.beforeDirectoryOpen
	originalAfter := directory.hooks.afterDirectoryOpen
	directory.hooks.beforeDirectoryOpen = func(path string) error {
		if originalBefore != nil {
			if err := originalBefore(path); err != nil {
				return err
			}
		}
		if before != nil {
			return before(path)
		}
		return nil
	}
	directory.hooks.afterDirectoryOpen = func(path string) {
		if originalAfter != nil {
			originalAfter(path)
		}
		if after != nil {
			after(path)
		}
	}
	directory.mu.Unlock()
	return func() {
		directory.mu.Lock()
		directory.hooks.beforeDirectoryOpen = originalBefore
		directory.hooks.afterDirectoryOpen = originalAfter
		directory.mu.Unlock()
	}
}

func (lease *unixStorageRecordLease) close() error {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.fd < 0 {
		return nil
	}
	fd := lease.fd
	lease.fd = -1
	if err := unix.Close(fd); err != nil {
		return fmt.Errorf("productmetrics: close retained config record: %w", err)
	}
	return nil
}

func (lease *unixStorageRecordLease) metadata() (storageMetadata, error) {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.fd < 0 {
		return storageMetadata{}, errStorageClosed
	}
	var stat unix.Stat_t
	if err := unix.Fstat(lease.fd, &stat); err != nil {
		return storageMetadata{}, fmt.Errorf("productmetrics: inspect retained record descriptor: %w", err)
	}
	return metadataFromStat(stat), nil
}

func platformOpenStorageRoot(home gchome.ProductUsageHome, mutable bool, hooks storageTestHooks) (storageDirectoryBackend, error) {
	if !isCleanAbsoluteProductRoot(home) {
		return nil, errors.New("productmetrics: invalid or unstable product-usage home")
	}
	euid := uint32(os.Geteuid())
	rootFD, err := openDirectoryPath("/", hooks)
	if err != nil {
		return nil, storagePathError("open", "/", err)
	}
	if err := hooks.canStartStorageWork(); err != nil {
		_ = unix.Close(rootFD)
		return nil, err
	}
	rootMetadata, err := metadataForFD(rootFD, "/", hooks)
	if err != nil {
		_ = unix.Close(rootFD)
		return nil, err
	}
	if err := validateAncestorDirectory(rootMetadata, "/", euid); err != nil {
		_ = unix.Close(rootFD)
		return nil, err
	}

	homePath := home.Home().Path()
	rootPath := home.Root()
	currentFD := rootFD
	currentPath := "/"
	stickyAwaitingPrivateBoundary := isRootOwnedStickyWritable(rootMetadata)
	components := strings.Split(strings.TrimPrefix(rootPath, "/"), "/")
	for _, component := range components {
		nextPath := filepath.Join(currentPath, component)
		privateBoundary := nextPath == homePath || nextPath == rootPath
		nextFD, created, openErr := openDirectoryComponent(currentFD, component, mutable, stickyAwaitingPrivateBoundary, euid, hooks, nextPath)
		if openErr != nil {
			_ = unix.Close(currentFD)
			return nil, storagePathError("open directory", nextPath, openErr)
		}
		componentHooks := hooks
		if created {
			componentHooks.decisionGate = nil
		}
		if err := componentHooks.canStartStorageWork(); err != nil {
			_ = unix.Close(nextFD)
			_ = unix.Close(currentFD)
			return nil, err
		}
		metadata, metadataErr := metadataForFD(nextFD, nextPath, componentHooks)
		if metadataErr != nil {
			_ = unix.Close(nextFD)
			_ = unix.Close(currentFD)
			return nil, metadataErr
		}
		if privateBoundary || created {
			metadataErr = validatePrivateDirectory(metadata, nextPath, euid, created)
		} else {
			metadataErr = validateAncestorDirectory(metadata, nextPath, euid)
		}
		if metadataErr != nil {
			_ = unix.Close(nextFD)
			_ = unix.Close(currentFD)
			return nil, metadataErr
		}

		componentHooks.openedComponent(nextPath)
		if err := revalidateOpenedDirectory(currentFD, component, nextFD, nextPath, euid, privateBoundary || created, created, componentHooks); err != nil {
			_ = unix.Close(nextFD)
			_ = unix.Close(currentFD)
			return nil, err
		}
		// A failed creation attempt can leave any newly visible intermediate
		// component awaiting its parent-directory sync. On retry that component
		// is indistinguishable from a pre-existing effective-UID private
		// ancestor, so recover every such retained component, not just the two
		// lexical private boundaries. Sync child before parent in the same order
		// as initial creation.
		recoverExistingPrivateComponent := privateBoundary ||
			(metadata.uid == euid && privateDirectoryPermissions(metadata.mode))
		if mutable && !created && recoverExistingPrivateComponent {
			if err := hooks.canStartStorageWork(); err != nil {
				_ = unix.Close(nextFD)
				_ = unix.Close(currentFD)
				return nil, err
			}
			if err := syncDirectoryFD(nextFD, hooks); err != nil {
				_ = unix.Close(nextFD)
				_ = unix.Close(currentFD)
				return nil, fmt.Errorf("productmetrics: recover private-directory sync: %w", err)
			}
			if err := hooks.canStartStorageWork(); err != nil {
				_ = unix.Close(nextFD)
				_ = unix.Close(currentFD)
				return nil, err
			}
			if err := syncDirectoryFD(currentFD, hooks); err != nil {
				_ = unix.Close(nextFD)
				_ = unix.Close(currentFD)
				return nil, fmt.Errorf("productmetrics: recover private-directory parent sync: %w", err)
			}
		}
		if stickyAwaitingPrivateBoundary && !created && metadata.uid == euid && privateDirectoryPermissions(metadata.mode) {
			stickyAwaitingPrivateBoundary = false
		}
		if isRootOwnedStickyWritable(metadata) {
			stickyAwaitingPrivateBoundary = true
		}
		if err := unix.Close(currentFD); err != nil {
			_ = unix.Close(nextFD)
			return nil, fmt.Errorf("productmetrics: close parent directory: %w", err)
		}
		currentFD = nextFD
		currentPath = nextPath
	}
	if stickyAwaitingPrivateBoundary {
		_ = unix.Close(currentFD)
		return nil, errors.New("productmetrics: root-owned sticky ancestor has no later existing private boundary")
	}
	return &unixStorageDirectory{
		fd:            currentFD,
		path:          rootPath,
		euid:          euid,
		mutable:       mutable,
		rootDirectory: true,
		hooks:         hooks,
	}, nil
}

func openDirectoryPath(path string, hooks storageTestHooks) (int, error) {
	for {
		if err := hooks.canStartStorageWork(); err != nil {
			return -1, err
		}
		fd, err := unix.Open(path, unixDirectoryOpenFlags, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return fd, err
	}
}

func openDirectoryComponent(parentFD int, name string, mutable, stickyPending bool, euid uint32, hooks storageTestHooks, path string) (int, bool, error) {
	for {
		fd, err := openDirectoryAt(parentFD, name, hooks, path, true)
		if err == nil {
			return fd, false, nil
		}
		if !errors.Is(err, fs.ErrNotExist) || !mutable {
			return -1, false, err
		}
		if stickyPending {
			return -1, false, errors.New("root-owned sticky ancestor has no later existing private boundary")
		}
		if err := hooks.canStartStorageWork(); err != nil {
			return -1, false, err
		}
		if err := unix.Mkdirat(parentFD, name, 0o700); err != nil {
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			return -1, false, err
		}
		createdHooks := hooks
		createdHooks.decisionGate = nil
		fd, err = openDirectoryAt(parentFD, name, createdHooks, path, false)
		if err != nil {
			return -1, true, err
		}
		if err := unix.Fchmod(fd, 0o700); err != nil {
			_ = unix.Close(fd)
			return -1, true, err
		}
		metadata, err := metadataForFD(fd, path, createdHooks)
		if err != nil {
			_ = unix.Close(fd)
			return -1, true, err
		}
		if err := validatePrivateDirectory(metadata, path, euid, true); err != nil {
			_ = unix.Close(fd)
			return -1, true, err
		}
		if err := syncDirectoryFD(fd, createdHooks); err != nil {
			_ = unix.Close(fd)
			return -1, true, fmt.Errorf("sync new directory: %w", err)
		}
		if err := syncDirectoryFD(parentFD, createdHooks); err != nil {
			_ = unix.Close(fd)
			return -1, true, fmt.Errorf("sync parent after directory creation: %w", err)
		}
		return fd, true, nil
	}
}

func revalidateOpenedDirectory(parentFD int, name string, fd int, path string, euid uint32, private, exactMode bool, hooks storageTestHooks) error {
	if err := hooks.canStartStorageWork(); err != nil {
		return err
	}
	opened, err := metadataForFD(fd, path, hooks)
	if err != nil {
		return err
	}
	if private {
		err = validatePrivateDirectory(opened, path, euid, exactMode)
	} else {
		err = validateAncestorDirectory(opened, path, euid)
	}
	if err != nil {
		return err
	}
	named, err := metadataAt(parentFD, name, path, hooks)
	if err != nil {
		return storagePathError("revalidate directory entry", path, err)
	}
	if named.dev != opened.dev || named.ino != opened.ino {
		return fmt.Errorf("productmetrics: directory entry %q changed after descriptor validation", path)
	}
	if private {
		return validatePrivateDirectory(named, path, euid, exactMode)
	}
	return validateAncestorDirectory(named, path, euid)
}

func metadataForFD(fd int, path string, hooks storageTestHooks) (storageMetadata, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return storageMetadata{}, storagePathError("inspect descriptor", path, err)
	}
	return hooks.inspect(path, metadataFromStat(stat)), nil
}

func metadataAt(parentFD int, name, path string, hooks storageTestHooks) (storageMetadata, error) {
	for {
		if err := hooks.canStartStorageWork(); err != nil {
			return storageMetadata{}, err
		}
		if hooks.beforeMetadataAttempt != nil {
			err := hooks.beforeMetadataAttempt(path)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				return storageMetadata{}, err
			}
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(parentFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return storageMetadata{}, err
		}
		return hooks.inspect(path, metadataFromStat(stat)), nil
	}
}

func metadataFromStat(stat unix.Stat_t) storageMetadata {
	kind := storageEntryOther
	switch uint32(stat.Mode) & unix.S_IFMT { //nolint:unconvert // Darwin's field is uint16.
	case unix.S_IFREG:
		kind = storageEntryRegular
	case unix.S_IFDIR:
		kind = storageEntryDirectory
	}
	return storageMetadata{
		uid:              stat.Uid,
		mode:             uint32(stat.Mode),  //nolint:unconvert // Darwin's field is uint16.
		nlink:            uint64(stat.Nlink), //nolint:unconvert // Darwin's field is uint16.
		dev:              uint64(stat.Dev),   //nolint:unconvert // Darwin's field is signed int32.
		ino:              uint64(stat.Ino),   //nolint:unconvert // Keep one cross-platform representation.
		size:             stat.Size,
		mtimeSeconds:     int64(stat.Mtim.Sec),  //nolint:unconvert // 32-bit Linux exposes int32 timespec fields.
		mtimeNanoseconds: int64(stat.Mtim.Nsec), //nolint:unconvert // Keep one cross-platform representation.
		kind:             kind,
		ownerOnly:        privateFilePermissions(uint32(stat.Mode)), //nolint:unconvert // Darwin's field is uint16.
	}
}

func validateAncestorDirectory(metadata storageMetadata, path string, euid uint32) error {
	if metadata.mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("productmetrics: path component %q is not a directory", path)
	}
	if metadata.nlink == 0 {
		return fmt.Errorf("productmetrics: directory %q has zero links", path)
	}
	if metadata.uid != 0 && metadata.uid != euid {
		return fmt.Errorf("productmetrics: ancestor %q has untrusted owner UID %d", path, metadata.uid)
	}
	if metadata.mode&0o022 != 0 && !isRootOwnedStickyWritable(metadata) {
		return fmt.Errorf("productmetrics: ancestor %q is group/world writable", path)
	}
	return nil
}

func validatePrivateDirectory(metadata storageMetadata, path string, euid uint32, exactMode bool) error {
	if metadata.mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("productmetrics: private path %q is not a directory", path)
	}
	if metadata.nlink == 0 {
		return fmt.Errorf("productmetrics: private directory %q has zero links", path)
	}
	if metadata.uid != euid {
		return fmt.Errorf("productmetrics: private path %q has owner UID %d, want effective UID %d", path, metadata.uid, euid)
	}
	if !privateDirectoryPermissions(metadata.mode) {
		return fmt.Errorf("productmetrics: private path %q has broader than owner-only permissions", path)
	}
	if exactMode && metadata.mode&0o777 != 0o700 {
		return fmt.Errorf("productmetrics: new private path %q has mode %04o, want 0700", path, metadata.mode&0o777)
	}
	return nil
}

func validateDirectoryForHandle(metadata storageMetadata, path string, euid uint32, cleanupOnly bool) error {
	if !cleanupOnly {
		return validatePrivateDirectory(metadata, path, euid, false)
	}
	if metadata.mode&unix.S_IFMT != unix.S_IFDIR || metadata.nlink == 0 {
		return fmt.Errorf("productmetrics: cleanup path %q is not a linked directory", path)
	}
	return nil
}

func validatePrivateRegularFile(metadata storageMetadata, path string, euid uint32, exactMode bool) error {
	if metadata.mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("productmetrics: file %q is not regular", path)
	}
	if metadata.uid != euid {
		return fmt.Errorf("productmetrics: file %q has owner UID %d, want effective UID %d", path, metadata.uid, euid)
	}
	if metadata.nlink != 1 {
		return fmt.Errorf("productmetrics: file %q has link count %d, want 1", path, metadata.nlink)
	}
	if !privateFilePermissions(metadata.mode) {
		return fmt.Errorf("productmetrics: file %q has broader than owner-only permissions", path)
	}
	if exactMode && metadata.mode&0o777 != 0o600 {
		return fmt.Errorf("productmetrics: new file %q has mode %04o, want 0600", path, metadata.mode&0o777)
	}
	return nil
}

func privateDirectoryPermissions(mode uint32) bool {
	return mode&0o077 == 0 && mode&(unix.S_ISUID|unix.S_ISGID|unix.S_ISVTX) == 0
}

func privateFilePermissions(mode uint32) bool {
	return mode&0o077 == 0 && mode&(unix.S_ISUID|unix.S_ISGID|unix.S_ISVTX) == 0
}

func isRootOwnedStickyWritable(metadata storageMetadata) bool {
	return metadata.uid == 0 && metadata.mode&unix.S_ISVTX != 0 && metadata.mode&0o022 != 0
}

func (directory *unixStorageDirectory) duplicateFD() (int, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if directory.fd < 0 {
		return -1, errStorageClosed
	}
	fd, err := unix.FcntlInt(uintptr(directory.fd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("productmetrics: duplicate directory descriptor: %w", err)
	}
	metadata, err := metadataForFD(fd, directory.path, directory.hooks)
	if err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	if err := validateDirectoryForHandle(metadata, directory.path, directory.euid, directory.cleanupOnly); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func closeUnixFD(fd int) {
	_ = unix.Close(fd)
}

func (directory *unixStorageDirectory) close() error {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if directory.fd < 0 {
		return nil
	}
	fd := directory.fd
	directory.fd = -1
	if err := unix.Close(fd); err != nil {
		return fmt.Errorf("productmetrics: close storage directory: %w", err)
	}
	return nil
}

func (directory *unixStorageDirectory) openDir(components []string, create bool) (storageDirectoryBackend, error) {
	if create && !directory.mutable {
		return nil, errors.New("productmetrics: read-only storage cannot create a directory")
	}
	currentFD, err := directory.duplicateFD()
	if err != nil {
		return nil, err
	}
	currentPath := directory.path
	for _, component := range components {
		nextPath := filepath.Join(currentPath, component)
		nextFD, created, openErr := openDirectoryComponent(currentFD, component, create, false, directory.euid, directory.hooks, nextPath)
		if openErr != nil {
			_ = unix.Close(currentFD)
			return nil, storagePathError("open private directory", nextPath, openErr)
		}
		componentHooks := directory.hooks
		if created {
			componentHooks.decisionGate = nil
		}
		if err := validateAndRevalidatePrivateComponent(currentFD, component, nextFD, nextPath, directory.euid, created, componentHooks); err != nil {
			_ = unix.Close(nextFD)
			_ = unix.Close(currentFD)
			return nil, err
		}
		// Any existing descendant reached from a mutable handle may be the
		// visible remainder of an earlier creation whose parent sync failed.
		// Recover child then parent before returning a write-capable handle,
		// regardless of whether this particular open allowed creation.
		if directory.mutable && !created {
			if err := directory.hooks.canStartStorageWork(); err != nil {
				_ = unix.Close(nextFD)
				_ = unix.Close(currentFD)
				return nil, err
			}
			if err := syncDirectoryFD(nextFD, directory.hooks); err != nil {
				_ = unix.Close(nextFD)
				_ = unix.Close(currentFD)
				return nil, fmt.Errorf("productmetrics: recover private-directory sync: %w", err)
			}
			if err := directory.hooks.canStartStorageWork(); err != nil {
				_ = unix.Close(nextFD)
				_ = unix.Close(currentFD)
				return nil, err
			}
			if err := syncDirectoryFD(currentFD, directory.hooks); err != nil {
				_ = unix.Close(nextFD)
				_ = unix.Close(currentFD)
				return nil, fmt.Errorf("productmetrics: recover private-directory parent sync: %w", err)
			}
		}
		if err := unix.Close(currentFD); err != nil {
			_ = unix.Close(nextFD)
			return nil, fmt.Errorf("productmetrics: close private parent: %w", err)
		}
		currentFD = nextFD
		currentPath = nextPath
	}
	return &unixStorageDirectory{
		fd:            currentFD,
		path:          currentPath,
		euid:          directory.euid,
		mutable:       directory.mutable,
		rootDirectory: directory.rootDirectory && len(components) == 0,
		cleanupOnly:   directory.cleanupOnly,
		hooks:         directory.hooks,
	}, nil
}

func validateAndRevalidatePrivateComponent(parentFD int, name string, fd int, path string, euid uint32, exactMode bool, hooks storageTestHooks) error {
	if err := hooks.canStartStorageWork(); err != nil {
		return err
	}
	metadata, err := metadataForFD(fd, path, hooks)
	if err != nil {
		return err
	}
	if err := validatePrivateDirectory(metadata, path, euid, exactMode); err != nil {
		return err
	}
	hooks.openedComponent(path)
	return revalidateOpenedDirectory(parentFD, name, fd, path, euid, true, exactMode, hooks)
}

func (directory *unixStorageDirectory) iterateEntries() (storageIteratorBackend, error) {
	directoryFD, err := directory.openIteratorFD()
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(directoryFD), directory.path)
	if file == nil {
		_ = unix.Close(directoryFD)
		return nil, errors.New("productmetrics: create directory iterator")
	}
	return &unixStorageIterator{
		file: file, path: directory.path, euid: directory.euid,
		cleanupOnly: directory.cleanupOnly, hooks: directory.hooks,
	}, nil
}

func (directory *unixStorageDirectory) firstEntryFromRetainedHandle() (storageEntry, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if directory.fd < 0 {
		return storageEntry{}, errStorageClosed
	}
	metadata, err := metadataForFD(directory.fd, directory.path, directory.hooks)
	if err != nil {
		return storageEntry{}, err
	}
	if err := validateDirectoryForHandle(metadata, directory.path, directory.euid, directory.cleanupOnly); err != nil {
		return storageEntry{}, err
	}
	if _, err := unix.Seek(directory.fd, 0, io.SeekStart); err != nil {
		return storageEntry{}, fmt.Errorf("productmetrics: rewind retained directory: %w", err)
	}
	buffer := make([]byte, 4096)
	for {
		if err := directory.hooks.run(storageStepEnumerate); err != nil {
			return storageEntry{}, fmt.Errorf("productmetrics: enumerate retained directory: %w", err)
		}
		count, readErr := unix.ReadDirent(directory.fd, buffer)
		if errors.Is(readErr, unix.EINTR) {
			continue
		}
		if readErr != nil {
			return storageEntry{}, fmt.Errorf("productmetrics: enumerate retained directory: %w", readErr)
		}
		if count == 0 {
			return storageEntry{}, io.EOF
		}
		_, _, names := unix.ParseDirent(buffer[:count], 1, nil)
		if len(names) == 0 {
			continue
		}
		name := names[0]
		entry := storageEntry{name: name, nameBytes: len(name)}
		if err := validateEnumeratedEntry(entry); err != nil {
			return storageEntry{}, err
		}
		if err := directory.hooks.run(storageStepEntryStat); err != nil {
			return storageEntry{}, fmt.Errorf("productmetrics: inspect enumerated entry: %w", err)
		}
		entry.metadata, err = metadataAt(directory.fd, name, filepath.Join(directory.path, name), directory.hooks)
		if err != nil {
			return storageEntry{}, storagePathError("inspect enumerated entry", filepath.Join(directory.path, name), err)
		}
		return entry, nil
	}
}

func (directory *unixStorageDirectory) openIteratorFD() (int, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if directory.fd < 0 {
		return -1, errStorageClosed
	}
	iteratorFD, err := openDirectoryAt(directory.fd, ".", directory.hooks, directory.path, true)
	if err != nil {
		return -1, storagePathError("open directory iterator", directory.path, err)
	}
	if directory.cleanupOnly {
		opened, openedErr := metadataForFD(iteratorFD, directory.path, directory.hooks)
		retained, retainedErr := metadataForFD(directory.fd, directory.path, directory.hooks)
		if openedErr != nil || retainedErr != nil || opened.dev != retained.dev || opened.ino != retained.ino || opened.mode&unix.S_IFMT != unix.S_IFDIR {
			_ = unix.Close(iteratorFD)
			return -1, errors.Join(openedErr, retainedErr, errors.New("productmetrics: cleanup directory iterator changed"))
		}
	} else if err := revalidateOpenedDirectory(directory.fd, ".", iteratorFD, directory.path, directory.euid, true, false, directory.hooks); err != nil {
		_ = unix.Close(iteratorFD)
		return -1, err
	}
	return iteratorFD, nil
}

func (iterator *unixStorageIterator) next() (storageEntry, error) {
	iterator.mu.Lock()
	defer iterator.mu.Unlock()
	if iterator.file == nil {
		return storageEntry{}, errStorageClosed
	}
	directoryFD := int(iterator.file.Fd())
	metadata, err := metadataForFD(directoryFD, iterator.path, iterator.hooks)
	if err != nil {
		return storageEntry{}, err
	}
	if err := validateDirectoryForHandle(metadata, iterator.path, iterator.euid, iterator.cleanupOnly); err != nil {
		return storageEntry{}, err
	}
	name := iterator.pendingName
	if name == "" {
		if err := iterator.hooks.run(storageStepEnumerate); err != nil {
			return storageEntry{}, fmt.Errorf("productmetrics: enumerate retained directory: %w", err)
		}
		names, readErr := iterator.file.Readdirnames(1)
		if len(names) == 0 {
			if errors.Is(readErr, io.EOF) {
				return storageEntry{}, io.EOF
			}
			if readErr != nil {
				return storageEntry{}, fmt.Errorf("productmetrics: enumerate retained directory: %w", readErr)
			}
			return storageEntry{}, io.EOF
		}
		name = names[0]
		iterator.pendingName = name
	}
	entry := storageEntry{name: name, nameBytes: len(name)}
	if err := validateEnumeratedEntry(entry); err != nil {
		return storageEntry{}, err
	}
	if err := iterator.hooks.run(storageStepEntryStat); err != nil {
		return storageEntry{}, fmt.Errorf("productmetrics: inspect enumerated entry: %w", err)
	}
	entry.metadata, err = metadataAt(directoryFD, name, filepath.Join(iterator.path, name), iterator.hooks)
	if err != nil {
		return storageEntry{}, storagePathError("inspect enumerated entry", filepath.Join(iterator.path, name), err)
	}
	iterator.pendingName = ""
	return entry, nil
}

func (iterator *unixStorageIterator) close() error {
	iterator.mu.Lock()
	defer iterator.mu.Unlock()
	if iterator.file == nil {
		return nil
	}
	file := iterator.file
	iterator.file = nil
	iterator.pendingName = ""
	if err := file.Close(); err != nil {
		return fmt.Errorf("productmetrics: close directory iterator: %w", err)
	}
	return nil
}

func (directory *unixStorageDirectory) lookupEntry(name string) (storageEntry, error) {
	directoryFD, err := directory.duplicateFD()
	if err != nil {
		return storageEntry{}, err
	}
	defer closeUnixFD(directoryFD)
	parentMetadata, err := metadataForFD(directoryFD, directory.path, directory.hooks)
	if err != nil {
		return storageEntry{}, err
	}
	if err := validateDirectoryForHandle(parentMetadata, directory.path, directory.euid, directory.cleanupOnly); err != nil {
		return storageEntry{}, err
	}
	if err := directory.hooks.run(storageStepEntryStat); err != nil {
		return storageEntry{}, fmt.Errorf("productmetrics: inspect named entry: %w", err)
	}
	path := filepath.Join(directory.path, name)
	metadata, err := metadataAt(directoryFD, name, path, directory.hooks)
	if err != nil {
		return storageEntry{}, storagePathError("inspect named entry", path, err)
	}
	return storageEntry{name: name, nameBytes: len(name), metadata: metadata}, nil
}

func (directory *unixStorageDirectory) openEnumeratedCleanupDirectory(entry storageEntry) (storageDirectoryBackend, error) {
	if !directory.mutable {
		return nil, errors.New("productmetrics: read-only storage cannot open a cleanup directory")
	}
	parentFD, err := directory.duplicateFD()
	if err != nil {
		return nil, err
	}
	defer closeUnixFD(parentFD)
	path := filepath.Join(directory.path, entry.name)
	current, missing, err := inspectEnumeratedEntry(parentFD, entry, path, directory.hooks)
	if err != nil {
		return nil, err
	}
	if missing {
		return nil, storagePathError("open cleanup directory", path, fs.ErrNotExist)
	}
	if current.mode&unix.S_IFMT != unix.S_IFDIR {
		return nil, fmt.Errorf("productmetrics: cleanup entry %q is not a directory", path)
	}
	childFD, err := openDirectoryAt(parentFD, entry.name, directory.hooks, path, true)
	if err != nil {
		return nil, storagePathError("open cleanup directory", path, err)
	}
	opened, openedErr := metadataForFD(childFD, path, directory.hooks)
	named, namedErr := metadataAt(parentFD, entry.name, path, directory.hooks)
	if openedErr != nil || namedErr != nil || opened.mode&unix.S_IFMT != unix.S_IFDIR ||
		opened.dev != current.dev || opened.ino != current.ino || named.dev != current.dev || named.ino != current.ino {
		_ = unix.Close(childFD)
		return nil, errors.Join(openedErr, namedErr, errors.New("productmetrics: cleanup directory entry changed while opening"))
	}
	// A prior cleanup mutation may have applied but lost its parent-sync
	// acknowledgement to a crash. Before trusting this exact retained child for
	// contents or absence, recovery-sync both the child contents and its named
	// link in the retained parent. Retry both on every reopen after uncertainty.
	childSyncErr := syncDirectoryFD(childFD, directory.hooks)
	parentSyncErr := syncDirectoryFD(parentFD, directory.hooks)
	if childSyncErr != nil || parentSyncErr != nil {
		_ = unix.Close(childFD)
		return nil, errors.Join(
			wrapStorageSyncError("sync retained cleanup child", childSyncErr),
			wrapStorageSyncError("sync retained cleanup parent", parentSyncErr),
		)
	}
	directory.hooks.openedComponent(path)
	cleanupOnly := directory.cleanupOnly || validatePrivateDirectory(opened, path, directory.euid, false) != nil
	return &unixStorageDirectory{
		fd: childFD, path: path, euid: directory.euid, mutable: true,
		cleanupOnly: cleanupOnly, hooks: directory.hooks,
	}, nil
}

func wrapStorageSyncError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("productmetrics: %s: %w", operation, err)
}

func (directory *unixStorageDirectory) readFile(name string, maximumBytes int64) ([]byte, error) {
	data, lease, _, err := directory.readFileLease(name, maximumBytes)
	if lease != nil {
		err = errors.Join(err, lease.close())
	}
	return data, err
}

func (directory *unixStorageDirectory) readFileLease(name string, maximumBytes int64) ([]byte, storageRecordBackend, storageMetadata, error) {
	return directory.readFileLeaseWithHooks(name, maximumBytes, directory.hooks)
}

func (directory *unixStorageDirectory) readFileLeaseClockFree(name string, maximumBytes int64) ([]byte, storageRecordBackend, storageMetadata, error) {
	hooks := directory.hooks
	hooks.decisionGate = nil
	return directory.readFileLeaseWithHooks(name, maximumBytes, hooks)
}

func (directory *unixStorageDirectory) readFileLeaseWithHooks(name string, maximumBytes int64, hooks storageTestHooks) ([]byte, storageRecordBackend, storageMetadata, error) {
	directoryFD, err := directory.duplicateFD()
	if err != nil {
		return nil, nil, storageMetadata{}, err
	}
	defer closeUnixFD(directoryFD)
	path := filepath.Join(directory.path, name)
	fileFD, err := openFileAtGated(directoryFD, name, unixFileReadFlags, 0, hooks)
	if err != nil {
		return nil, nil, storageMetadata{}, storagePathError("open file", path, err)
	}
	hooks.openedFile(path)
	metadata, err := validateOpenedRegularFileGated(directoryFD, name, fileFD, path, directory.euid, false, hooks)
	if err != nil {
		_ = unix.Close(fileFD)
		return nil, nil, storageMetadata{}, err
	}
	lease := &unixStorageRecordLease{fd: fileFD}
	if metadata.size < 0 || metadata.size > maximumBytes {
		return nil, lease, metadata, fmt.Errorf("productmetrics: file %q exceeds the read limit", path)
	}
	hooks.startingRead(path)
	data, physicalReadBytes, err := readFDWithLimit(fileFD, maximumBytes, path, hooks)
	metadata.physicalReadBytes = physicalReadBytes
	return data, lease, metadata, err
}

func openFileAtGated(directoryFD int, name string, flags int, mode uint32, hooks storageTestHooks) (int, error) {
	for {
		if err := hooks.canStartStorageWork(); err != nil {
			return -1, err
		}
		fd, err := unix.Openat(directoryFD, name, flags, mode)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return fd, err
	}
}

func openFileAt(directoryFD int, name string, flags int, mode uint32) (int, error) {
	for {
		fd, err := unix.Openat(directoryFD, name, flags, mode)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return fd, err
	}
}

func openDirectoryAt(directoryFD int, name string, hooks storageTestHooks, path string, gated bool) (int, error) {
	for {
		if gated {
			if err := hooks.canStartStorageWork(); err != nil {
				return -1, err
			}
		}
		if err := hooks.openingDirectory(path); err != nil {
			return -1, err
		}
		fd, err := unix.Openat(directoryFD, name, unixDirectoryOpenFlags, 0)
		hooks.observedDirectoryAttempt(path, err)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err == nil {
			hooks.openedDirectory(path)
		}
		return fd, err
	}
}

//nolint:unparam // Metadata result preserves the shared validator contract used by stable lock callers.
func validateOpenedRegularFile(directoryFD int, name string, fileFD int, path string, euid uint32, exactMode bool, hooks storageTestHooks) (storageMetadata, error) {
	metadata, err := metadataForFD(fileFD, path, hooks)
	if err != nil {
		return storageMetadata{}, err
	}
	if err := validatePrivateRegularFile(metadata, path, euid, exactMode); err != nil {
		return storageMetadata{}, err
	}
	named, err := metadataAt(directoryFD, name, path, hooks)
	if err != nil {
		return storageMetadata{}, storagePathError("revalidate file entry", path, err)
	}
	if named.dev != metadata.dev || named.ino != metadata.ino {
		return storageMetadata{}, fmt.Errorf("productmetrics: file entry %q changed after descriptor validation", path)
	}
	if err := validatePrivateRegularFile(named, path, euid, exactMode); err != nil {
		return storageMetadata{}, err
	}
	return metadata, nil
}

func validateOpenedRegularFileGated(directoryFD int, name string, fileFD int, path string, euid uint32, exactMode bool, hooks storageTestHooks) (storageMetadata, error) {
	if err := hooks.canStartStorageWork(); err != nil {
		return storageMetadata{}, err
	}
	metadata, err := metadataForFD(fileFD, path, hooks)
	if err != nil {
		return storageMetadata{}, err
	}
	if err := validatePrivateRegularFile(metadata, path, euid, exactMode); err != nil {
		return storageMetadata{}, err
	}
	if err := hooks.canStartStorageWork(); err != nil {
		return storageMetadata{}, err
	}
	named, err := metadataAt(directoryFD, name, path, hooks)
	if err != nil {
		return storageMetadata{}, storagePathError("revalidate file entry", path, err)
	}
	if named.dev != metadata.dev || named.ino != metadata.ino {
		return storageMetadata{}, fmt.Errorf("productmetrics: file entry %q changed after descriptor validation", path)
	}
	if err := validatePrivateRegularFile(named, path, euid, exactMode); err != nil {
		return storageMetadata{}, err
	}
	return metadata, nil
}

func readFDWithLimit(fd int, maximumBytes int64, path string, hooks storageTestHooks) ([]byte, uint64, error) {
	capacity := maximumBytes
	if capacity > 32*1024 {
		capacity = 32 * 1024
	}
	result := make([]byte, 0, int(capacity))
	buffer := make([]byte, 4096)
	physicalReadBytes := uint64(0)
	for {
		remaining := maximumBytes - int64(len(result))
		request := len(buffer)
		if remaining+1 < int64(request) {
			request = int(remaining + 1)
		}
		if err := hooks.canStartStorageWork(); err != nil {
			return nil, physicalReadBytes, err
		}
		read, err := unix.Read(fd, buffer[:request])
		hooks.observedRead(path, request, read, err)
		if read > 0 {
			physicalReadBytes += uint64(read)
			if int64(len(result))+int64(read) > maximumBytes {
				return nil, physicalReadBytes, errors.New("productmetrics: file exceeds the read limit")
			}
			result = append(result, buffer[:read]...)
		}
		if err == nil {
			if read == 0 {
				return result, physicalReadBytes, nil
			}
			continue
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, io.EOF) {
			return result, physicalReadBytes, nil
		}
		return nil, physicalReadBytes, fmt.Errorf("productmetrics: read private file: %w", err)
	}
}

func (directory *unixStorageDirectory) writeFileAtomic(name string, data []byte) (returnErr error) {
	_, err := directory.writeFileAtomically(name, data, false)
	return err
}

func (directory *unixStorageDirectory) writeFileAtomicOutcome(name string, data []byte) (storageWriteResult, error) {
	return directory.writeFileAtomically(name, data, false)
}

func (directory *unixStorageDirectory) writeFileAtomicNoReplace(name string, data []byte) error {
	_, err := directory.writeFileAtomically(name, data, true)
	return err
}

func (directory *unixStorageDirectory) writeFileAtomically(name string, data []byte, noReplace bool) (result storageWriteResult, returnErr error) {
	result.state = storageWriteNotApplied
	if !directory.mutable {
		return result, errors.New("productmetrics: read-only storage cannot write")
	}
	directoryFD, err := directory.duplicateFD()
	if err != nil {
		return result, err
	}
	defer closeUnixFD(directoryFD)
	path := filepath.Join(directory.path, name)
	var replacedMetadata storageMetadata
	replacedPresent := false
	if noReplace {
		if err := requireAbsentEntry(directoryFD, name, path, directory.hooks, errStorageEntryExists); err != nil {
			return result, err
		}
	} else {
		var inspectErr error
		replacedMetadata, replacedPresent, inspectErr = inspectReplaceTarget(directoryFD, name, path, directory.euid, directory.hooks)
		if inspectErr != nil {
			return result, inspectErr
		}
	}

	tempName, tempFD, err := createPrivateTempFile(directoryFD, directory.path, directory.euid, directory.hooks)
	if err != nil {
		return result, err
	}
	tempExists := true
	defer func() {
		if tempFD >= 0 {
			returnErr = errors.Join(returnErr, unix.Close(tempFD))
		}
		if tempExists {
			if err := unix.Unlinkat(directoryFD, tempName, 0); err == nil {
				returnErr = errors.Join(returnErr, syncDirectoryFD(directoryFD, directory.hooks))
			} else if !errors.Is(err, fs.ErrNotExist) {
				returnErr = errors.Join(returnErr, fmt.Errorf("productmetrics: remove temporary file: %w", err))
			}
		}
	}()

	if err := writeAllFD(tempFD, data, directory.hooks); err != nil {
		return result, fmt.Errorf("productmetrics: write temporary file: %w", err)
	}
	if err := syncFileFD(tempFD, directory.hooks); err != nil {
		return result, fmt.Errorf("productmetrics: sync temporary file: %w", err)
	}
	if err := unix.Close(tempFD); err != nil {
		tempFD = -1
		return result, fmt.Errorf("productmetrics: close temporary file: %w", err)
	}
	tempFD = -1
	tempMetadata, metadataErr := metadataAt(directoryFD, tempName, filepath.Join(directory.path, tempName), directory.hooks)
	if metadataErr != nil {
		return result, metadataErr
	}
	if noReplace {
		outcome, installErr := renameNoReplaceAt(directoryFD, tempName, tempMetadata, directoryFD, name, directory.hooks)
		if outcome == noReplaceNotApplied {
			return result, storagePathError("install no-replace file", path, installErr)
		}
		result.state = storageWriteAppliedSyncPending
		if outcome == noReplaceApplied {
			tempExists = false
		}
		syncErr := syncDirectoryFD(directoryFD, directory.hooks)
		if err := errors.Join(installErr, syncErr); err != nil {
			return result, storagePathError("install no-replace file", path, err)
		}
	} else {
		outcome, renameErr := renameReplaceAt(directoryFD, tempName, tempMetadata, directoryFD, name,
			replacedMetadata, replacedPresent, directory.hooks)
		if outcome == noReplaceNotApplied {
			return result, storagePathError("rename temporary file", path, renameErr)
		}
		result.state = storageWriteAppliedSyncPending
		if outcome == noReplaceApplied {
			tempExists = false
		}
		if err := errors.Join(renameErr, syncDirectoryFD(directoryFD, directory.hooks)); err != nil {
			return result, storagePathError("rename temporary file", path, err)
		}
	}
	result.state = storageWriteAppliedDurable
	directory.hooks.wroteAtomic(path, result.state)
	return result, nil
}

type noReplaceOutcome uint8

const (
	noReplaceNotApplied noReplaceOutcome = iota
	noReplaceApplied
	noReplaceAmbiguous
)

func createPrivateTempFile(directoryFD int, directoryPath string, euid uint32, hooks storageTestHooks) (string, int, error) {
	for attempts := 0; attempts < maximumStorageTempAttempts; attempts++ {
		if err := hooks.canStartStorageWork(); err != nil {
			return "", -1, err
		}
		sequence := storageTempSequence.Add(1)
		name := fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), sequence)
		hooks.creatingTempFile(filepath.Join(directoryPath, name))
		fd, err := openFileAt(directoryFD, name, unixFileWriteFlags|unix.O_CREAT|unix.O_EXCL, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", -1, storagePathError("create temporary file", filepath.Join(directoryPath, name), err)
		}
		if err := unix.Fchmod(fd, 0o600); err != nil {
			cleanupErr := discardPrivateTemp(directoryFD, name, fd, hooks)
			return "", -1, errors.Join(fmt.Errorf("productmetrics: set private temporary-file mode: %w", err), cleanupErr)
		}
		if _, err := validateOpenedRegularFile(directoryFD, name, fd, filepath.Join(directoryPath, name), euid, true, hooks); err != nil {
			return "", -1, errors.Join(err, discardPrivateTemp(directoryFD, name, fd, hooks))
		}
		return name, fd, nil
	}
	return "", -1, errors.New("productmetrics: could not allocate a private temporary file")
}

func discardPrivateTemp(directoryFD int, name string, fileFD int, hooks storageTestHooks) error {
	closeErr := unix.Close(fileFD)
	unlinkErr := unix.Unlinkat(directoryFD, name, 0)
	if unlinkErr != nil && !errors.Is(unlinkErr, fs.ErrNotExist) {
		return errors.Join(closeErr, fmt.Errorf("productmetrics: remove rejected temporary file: %w", unlinkErr))
	}
	return errors.Join(closeErr, syncDirectoryFD(directoryFD, hooks))
}

func writeAllFD(fd int, data []byte, hooks storageTestHooks) error {
	for {
		if err := hooks.run(storageStepWrite); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		break
	}
	for len(data) > 0 {
		written, err := unix.Write(fd, data)
		if written > 0 {
			data = data[written:]
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func syncFileFD(fd int, hooks storageTestHooks) error {
	for {
		if err := hooks.run(storageStepFileSync); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		if err := unix.Fsync(fd); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		return nil
	}
}

func syncDirectoryFD(fd int, hooks storageTestHooks) error {
	for {
		if err := hooks.run(storageStepDirectorySync); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		if err := unix.Fsync(fd); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		return nil
	}
}

func inspectReplaceTarget(directoryFD int, name, path string, euid uint32, hooks storageTestHooks) (storageMetadata, bool, error) {
	metadata, err := metadataAt(directoryFD, name, path, hooks)
	if errors.Is(err, fs.ErrNotExist) {
		return storageMetadata{}, false, nil
	}
	if err != nil {
		return storageMetadata{}, false, storagePathError("inspect existing file", path, err)
	}
	if err := validatePrivateRegularFile(metadata, path, euid, false); err != nil {
		return storageMetadata{}, false, err
	}
	return metadata, true, nil
}

func (directory *unixStorageDirectory) removeFile(name string) error {
	return directory.removeFileWithHooks(name, directory.hooks, recordIncarnation{})
}

func (directory *unixStorageDirectory) removeFileClockFree(name string) error {
	hooks := directory.hooks
	hooks.decisionGate = nil
	return directory.removeFileWithHooks(name, hooks, recordIncarnation{})
}

func (directory *unixStorageDirectory) removeFileMatching(name string, expected recordIncarnation) error {
	if expected == (recordIncarnation{}) {
		return errors.New("productmetrics: invalid expected record incarnation for deletion")
	}
	return directory.removeFileWithHooks(name, directory.hooks, expected)
}

func (directory *unixStorageDirectory) confirmEntryAbsent(name string) error {
	if !directory.mutable {
		return errors.New("productmetrics: read-only storage cannot confirm deletion")
	}
	directoryFD, err := directory.duplicateFD()
	if err != nil {
		return err
	}
	defer closeUnixFD(directoryFD)
	path := filepath.Join(directory.path, name)
	if _, err := metadataAt(directoryFD, name, path, directory.hooks); err == nil {
		return storagePathError("confirm missing entry", path, errStorageEntryChanged)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return storagePathError("confirm missing entry", path, err)
	}
	if err := syncDirectoryFD(directoryFD, directory.hooks); err != nil {
		return fmt.Errorf("productmetrics: sync directory while confirming entry absence: %w", err)
	}
	if _, err := metadataAt(directoryFD, name, path, directory.hooks); err == nil {
		return storagePathError("reconfirm missing entry", path, errStorageEntryChanged)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return storagePathError("reconfirm missing entry", path, err)
	}
	return nil
}

func (directory *unixStorageDirectory) removeFileWithHooks(name string, hooks storageTestHooks, expected recordIncarnation) error {
	if !directory.mutable {
		return errors.New("productmetrics: read-only storage cannot delete")
	}
	directoryFD, err := directory.duplicateFD()
	if err != nil {
		return err
	}
	defer closeUnixFD(directoryFD)
	path := filepath.Join(directory.path, name)
	metadata, err := metadataAt(directoryFD, name, path, hooks)
	if errors.Is(err, fs.ErrNotExist) {
		if expected != (recordIncarnation{}) {
			return storagePathError("revalidate leased file before deletion", path, errStorageEntryChanged)
		}
		if err := syncDirectoryFD(directoryFD, hooks); err != nil {
			return fmt.Errorf("productmetrics: sync directory after confirming deletion: %w", err)
		}
		return nil
	}
	if err != nil {
		return storagePathError("inspect file for deletion", path, err)
	}
	if err := validatePrivateRegularFile(metadata, path, directory.euid, false); err != nil {
		return err
	}
	if expected != (recordIncarnation{}) &&
		(metadata.dev != expected.dev || metadata.ino != expected.ino) {
		return storagePathError("revalidate leased file before deletion", path, errStorageEntryChanged)
	}
	hooks.preparingMutation(storageStepDelete, path)
	if err := hooks.canStartStorageWork(); err != nil {
		return err
	}
	if err := hooks.run(storageStepDelete); err != nil {
		return fmt.Errorf("productmetrics: injected delete failure: %w", err)
	}
	current, err := metadataAt(directoryFD, name, path, hooks)
	if errors.Is(err, fs.ErrNotExist) {
		return storagePathError("revalidate file before deletion", path, errStorageEntryChanged)
	}
	if err != nil {
		return storagePathError("revalidate file before deletion", path, errors.Join(errStorageEntryChanged, err))
	}
	if !sameStorageIdentity(current, metadata) {
		return storagePathError("revalidate file before deletion", path, errStorageEntryChanged)
	}
	if err := validatePrivateRegularFile(current, path, directory.euid, false); err != nil {
		return err
	}
	if err := hooks.canStartStorageWork(); err != nil {
		return err
	}
	if err := unix.Unlinkat(directoryFD, name, 0); err != nil {
		return storagePathError("delete file", path, err)
	}
	if err := syncDirectoryFD(directoryFD, hooks); err != nil {
		return fmt.Errorf("productmetrics: sync directory after delete: %w", err)
	}
	return nil
}

func (directory *unixStorageDirectory) unlinkEnumeratedEntry(entry storageEntry) error {
	if !directory.mutable {
		return errors.New("productmetrics: read-only storage cannot unlink an enumerated entry")
	}
	if directory.rootDirectory && isStorageLockName(entry.name) {
		return fmt.Errorf("productmetrics: stable root lock %q cannot be removed by enumerated cleanup", entry.name)
	}
	directoryFD, err := directory.duplicateFD()
	if err != nil {
		return err
	}
	defer closeUnixFD(directoryFD)
	path := filepath.Join(directory.path, entry.name)
	current, missing, err := inspectEnumeratedEntry(directoryFD, entry, path, directory.hooks)
	if err != nil {
		return err
	}
	if missing {
		return syncMissingEntryParent(directoryFD, directory.hooks)
	}
	if current.mode&unix.S_IFMT == unix.S_IFDIR {
		return fmt.Errorf("%w: %q", errStorageEntryIsDirectory, path)
	}
	directory.hooks.preparingMutation(storageStepUnlink, path)
	if err := directory.hooks.canStartStorageWork(); err != nil {
		return err
	}
	if err := unlinkAt(directoryFD, entry.name, current, path, 0, storageStepUnlink, directory.hooks); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return syncMissingEntryParent(directoryFD, directory.hooks)
		}
		return storagePathError("unlink enumerated entry", path, err)
	}
	if err := syncDirectoryFD(directoryFD, directory.hooks); err != nil {
		return fmt.Errorf("productmetrics: sync directory after enumerated unlink: %w", err)
	}
	return nil
}

func (directory *unixStorageDirectory) removeEnumeratedDirectory(entry storageEntry) error {
	return directory.removeEnumeratedDirectoryWithPolicy(entry, true)
}

func (directory *unixStorageDirectory) removeEnumeratedCleanupDirectory(entry storageEntry) error {
	return directory.removeEnumeratedDirectoryWithPolicy(entry, false)
}

func (directory *unixStorageDirectory) removeEnumeratedDirectoryWithPolicy(entry storageEntry, requirePrivate bool) error {
	if !directory.mutable {
		return errors.New("productmetrics: read-only storage cannot remove an enumerated directory")
	}
	if directory.rootDirectory && isStorageLockName(entry.name) {
		return fmt.Errorf("productmetrics: stable root lock name %q cannot be removed by enumerated cleanup", entry.name)
	}
	directoryFD, err := directory.duplicateFD()
	if err != nil {
		return err
	}
	defer closeUnixFD(directoryFD)
	path := filepath.Join(directory.path, entry.name)
	current, missing, err := inspectEnumeratedEntry(directoryFD, entry, path, directory.hooks)
	if err != nil {
		return err
	}
	if missing {
		return syncMissingEntryParent(directoryFD, directory.hooks)
	}
	if requirePrivate {
		if err := validatePrivateDirectory(current, path, directory.euid, false); err != nil {
			return err
		}
	} else if current.mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("productmetrics: enumerated cleanup entry %q is not a directory", path)
	}
	directory.hooks.preparingMutation(storageStepRmdir, path)
	if err := directory.hooks.canStartStorageWork(); err != nil {
		return err
	}
	if err := unlinkAt(directoryFD, entry.name, current, path, unix.AT_REMOVEDIR, storageStepRmdir, directory.hooks); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return syncMissingEntryParent(directoryFD, directory.hooks)
		}
		if errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("%w: %q", errStorageDirectoryNotEmpty, path)
		}
		return storagePathError("remove enumerated directory", path, err)
	}
	if err := syncDirectoryFD(directoryFD, directory.hooks); err != nil {
		return fmt.Errorf("productmetrics: sync parent after directory removal: %w", err)
	}
	return nil
}

func inspectEnumeratedEntry(directoryFD int, entry storageEntry, path string, hooks storageTestHooks) (storageMetadata, bool, error) {
	if err := hooks.run(storageStepEntryStat); err != nil {
		return storageMetadata{}, false, fmt.Errorf("productmetrics: inspect enumerated entry before cleanup: %w", err)
	}
	current, err := metadataAt(directoryFD, entry.name, path, hooks)
	if errors.Is(err, fs.ErrNotExist) {
		return storageMetadata{}, true, nil
	}
	if err != nil {
		return storageMetadata{}, false, storagePathError("revalidate enumerated entry", path, err)
	}
	if current.dev != entry.metadata.dev || current.ino != entry.metadata.ino || current.mode&unix.S_IFMT != entry.metadata.mode&unix.S_IFMT {
		return storageMetadata{}, false, fmt.Errorf("%w: %q", errStorageEntryChanged, path)
	}
	return current, false, nil
}

func unlinkAt(directoryFD int, name string, expected storageMetadata, path string, flags int, step storageStep, hooks storageTestHooks) error {
	for {
		if err := hooks.run(step); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		current, err := metadataAt(directoryFD, name, path, storageTestHooks{})
		if err != nil {
			return errors.Join(errStorageEntryChanged, err)
		}
		if !sameStorageIdentity(current, expected) || current.mode&unix.S_IFMT != expected.mode&unix.S_IFMT {
			return errStorageEntryChanged
		}
		if err := unix.Unlinkat(directoryFD, name, flags); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		return nil
	}
}

func syncMissingEntryParent(directoryFD int, hooks storageTestHooks) error {
	if err := syncDirectoryFD(directoryFD, hooks); err != nil {
		return fmt.Errorf("productmetrics: sync parent after confirming missing entry: %w", err)
	}
	return nil
}

func (directory *unixStorageDirectory) renameFile(name string, targetBackend storageDirectoryBackend, targetName string) (storageRenameResult, error) {
	notApplied := storageRenameResult{state: storageRenameNotApplied}
	if !directory.mutable {
		return notApplied, errors.New("productmetrics: read-only storage cannot rename")
	}
	target, ok := targetBackend.(*unixStorageDirectory)
	if !ok || !target.mutable {
		return notApplied, errors.New("productmetrics: incompatible or read-only rename target")
	}
	sourceFD, err := directory.duplicateFD()
	if err != nil {
		return notApplied, err
	}
	defer closeUnixFD(sourceFD)
	targetFD, err := target.duplicateFD()
	if err != nil {
		return notApplied, err
	}
	defer closeUnixFD(targetFD)
	sourcePath := filepath.Join(directory.path, name)
	targetPath := filepath.Join(target.path, targetName)
	sourceMetadata, err := metadataAt(sourceFD, name, sourcePath, directory.hooks)
	if err != nil {
		return notApplied, storagePathError("inspect rename source", sourcePath, err)
	}
	if err := validatePrivateRegularFile(sourceMetadata, sourcePath, directory.euid, false); err != nil {
		return notApplied, err
	}
	if err := requireAbsentEntry(targetFD, targetName, targetPath, target.hooks, errStorageDestinationExists); err != nil {
		return notApplied, err
	}
	sourceDirectoryMetadata, sourceMetadataErr := metadataForFD(sourceFD, directory.path, storageTestHooks{})
	targetDirectoryMetadata, targetMetadataErr := metadataForFD(targetFD, target.path, storageTestHooks{})
	if sourceMetadataErr != nil || targetMetadataErr != nil {
		return notApplied, errors.Join(sourceMetadataErr, targetMetadataErr)
	}
	outcome, renameErr := renameNoReplaceAt(sourceFD, name, sourceMetadata, targetFD, targetName, directory.hooks)
	if outcome == noReplaceNotApplied {
		if errors.Is(renameErr, errStorageDestinationExists) {
			return notApplied, fmt.Errorf("%w: %q", renameErr, targetPath)
		}
		return notApplied, renameErr
	}
	pending := storageRenameResult{state: storageRenameAppliedSyncPending}
	sameParent := sourceDirectoryMetadata.dev == targetDirectoryMetadata.dev && sourceDirectoryMetadata.ino == targetDirectoryMetadata.ino
	syncErr := syncOneWayRenameParents(
		sourceFD, targetFD, sameParent, directory.hooks, target.hooks,
		"sync rename source directory", "sync rename target directory",
	)
	if err := errors.Join(renameErr, syncErr); err != nil {
		directory.hooks.renamed(sourcePath, targetPath, pending.state)
		return pending, err
	}
	durable := storageRenameResult{state: storageRenameAppliedDurable}
	directory.hooks.renamed(sourcePath, targetPath, durable.state)
	return durable, nil
}

func (directory *unixStorageDirectory) replaceFile(name string, targetBackend storageDirectoryBackend, targetName string) (storageRenameResult, error) {
	notApplied := storageRenameResult{state: storageRenameNotApplied}
	if !directory.mutable {
		return notApplied, errors.New("productmetrics: read-only storage cannot replace a file")
	}
	target, ok := targetBackend.(*unixStorageDirectory)
	if !ok || !target.mutable {
		return notApplied, errors.New("productmetrics: incompatible or read-only file-replacement target")
	}
	sourceFD, err := directory.duplicateFD()
	if err != nil {
		return notApplied, err
	}
	defer closeUnixFD(sourceFD)
	targetFD, err := target.duplicateFD()
	if err != nil {
		return notApplied, err
	}
	defer closeUnixFD(targetFD)
	sourcePath := filepath.Join(directory.path, name)
	targetPath := filepath.Join(target.path, targetName)
	sourceMetadata, err := metadataAt(sourceFD, name, sourcePath, directory.hooks)
	if err != nil {
		return notApplied, storagePathError("inspect replacement source", sourcePath, err)
	}
	if err := validatePrivateRegularFile(sourceMetadata, sourcePath, directory.euid, false); err != nil {
		return notApplied, err
	}
	targetMetadata, targetPresent, err := inspectReplaceTarget(targetFD, targetName, targetPath, target.euid, target.hooks)
	if err != nil {
		return notApplied, err
	}
	sourceParent, sourceParentErr := metadataForFD(sourceFD, directory.path, storageTestHooks{})
	targetParent, targetParentErr := metadataForFD(targetFD, target.path, storageTestHooks{})
	if sourceParentErr != nil || targetParentErr != nil {
		return notApplied, errors.Join(sourceParentErr, targetParentErr)
	}
	outcome, renameErr := renameReplaceAt(sourceFD, name, sourceMetadata, targetFD, targetName,
		targetMetadata, targetPresent, directory.hooks)
	if outcome == noReplaceNotApplied {
		return notApplied, renameErr
	}
	pending := storageRenameResult{state: storageRenameAppliedSyncPending}
	sameParent := sourceParent.dev == targetParent.dev && sourceParent.ino == targetParent.ino
	syncErr := syncOneWayRenameParents(
		sourceFD, targetFD, sameParent, directory.hooks, target.hooks,
		"sync file-replacement source", "sync file-replacement target",
	)
	if err := errors.Join(renameErr, syncErr); err != nil {
		directory.hooks.renamed(sourcePath, targetPath, pending.state)
		return pending, err
	}
	durable := storageRenameResult{state: storageRenameAppliedDurable}
	directory.hooks.renamed(sourcePath, targetPath, durable.state)
	return durable, nil
}

func (directory *unixStorageDirectory) renameEnumeratedDirectory(entry storageEntry, targetBackend storageDirectoryBackend, targetName string) (storageRenameResult, error) {
	if entry.metadata.kind != storageEntryDirectory {
		return storageRenameResult{state: storageRenameNotApplied}, errors.New("productmetrics: enumerated rename source is not a directory")
	}
	return directory.renameEnumeratedEntry(entry, targetBackend, targetName)
}

func (directory *unixStorageDirectory) renameEnumeratedEntry(entry storageEntry, targetBackend storageDirectoryBackend, targetName string) (storageRenameResult, error) {
	notApplied := storageRenameResult{state: storageRenameNotApplied}
	if !directory.mutable {
		return notApplied, errors.New("productmetrics: read-only storage cannot rename an enumerated entry")
	}
	if directory.rootDirectory && isStorageLockName(entry.name) {
		return notApplied, fmt.Errorf("productmetrics: stable root lock %q cannot be renamed by enumerated cleanup", entry.name)
	}
	target, ok := targetBackend.(*unixStorageDirectory)
	if !ok || !target.mutable {
		return notApplied, errors.New("productmetrics: incompatible or read-only enumerated-entry rename target")
	}
	if target.rootDirectory && isStorageLockName(targetName) {
		return notApplied, fmt.Errorf("productmetrics: stable root lock name %q cannot be created by enumerated cleanup", targetName)
	}
	sourceFD, err := directory.duplicateFD()
	if err != nil {
		return notApplied, err
	}
	defer closeUnixFD(sourceFD)
	targetFD, err := target.duplicateFD()
	if err != nil {
		return notApplied, err
	}
	defer closeUnixFD(targetFD)
	sourcePath := filepath.Join(directory.path, entry.name)
	current, missing, err := inspectEnumeratedEntry(sourceFD, entry, sourcePath, directory.hooks)
	if err != nil {
		return notApplied, err
	}
	if missing {
		return notApplied, storagePathError("inspect enumerated rename source", sourcePath, fs.ErrNotExist)
	}
	targetPath := filepath.Join(target.path, targetName)
	if err := requireAbsentEntry(targetFD, targetName, targetPath, target.hooks, errStorageDestinationExists); err != nil {
		return notApplied, err
	}
	sourceDirectoryMetadata, sourceMetadataErr := metadataForFD(sourceFD, directory.path, storageTestHooks{})
	targetDirectoryMetadata, targetMetadataErr := metadataForFD(targetFD, target.path, storageTestHooks{})
	if sourceMetadataErr != nil || targetMetadataErr != nil {
		return notApplied, errors.Join(sourceMetadataErr, targetMetadataErr)
	}
	outcome, renameErr := renameNoReplaceAt(sourceFD, entry.name, current, targetFD, targetName, directory.hooks)
	if outcome == noReplaceNotApplied {
		if errors.Is(renameErr, errStorageDestinationExists) {
			return notApplied, fmt.Errorf("%w: %q", renameErr, targetPath)
		}
		return notApplied, renameErr
	}
	pending := storageRenameResult{state: storageRenameAppliedSyncPending}
	sameParent := sourceDirectoryMetadata.dev == targetDirectoryMetadata.dev && sourceDirectoryMetadata.ino == targetDirectoryMetadata.ino
	syncErr := syncOneWayRenameParents(
		sourceFD, targetFD, sameParent, directory.hooks, target.hooks,
		"sync enumerated-entry rename source", "sync enumerated-entry rename target",
	)
	if err := errors.Join(renameErr, syncErr); err != nil {
		directory.hooks.renamed(sourcePath, targetPath, pending.state)
		return pending, err
	}
	durable := storageRenameResult{state: storageRenameAppliedDurable}
	directory.hooks.renamed(sourcePath, targetPath, durable.state)
	return durable, nil
}

func syncOneWayRenameParents(
	sourceFD, targetFD int,
	sameParent bool,
	sourceHooks, targetHooks storageTestHooks,
	sourceOperation, targetOperation string,
) error {
	if !sameParent {
		if err := syncDirectoryFD(targetFD, targetHooks); err != nil {
			return fmt.Errorf("%s: %w", targetOperation, err)
		}
	}
	if err := syncDirectoryFD(sourceFD, sourceHooks); err != nil {
		return fmt.Errorf("%s: %w", sourceOperation, err)
	}
	return nil
}

func (directory *unixStorageDirectory) exchangeEnumeratedEntries(source storageEntry, targetBackend storageDirectoryBackend, targetEntry storageEntry) (storageRenameResult, error) {
	notApplied := storageRenameResult{state: storageRenameNotApplied}
	if !directory.mutable {
		return notApplied, errors.New("productmetrics: read-only storage cannot exchange enumerated entries")
	}
	if directory.rootDirectory && isStorageLockName(source.name) {
		return notApplied, fmt.Errorf("productmetrics: stable root lock %q cannot be exchanged by enumerated cleanup", source.name)
	}
	target, ok := targetBackend.(*unixStorageDirectory)
	if !ok || !target.mutable {
		return notApplied, errors.New("productmetrics: incompatible or read-only entry exchange target")
	}
	if target.rootDirectory && isStorageLockName(targetEntry.name) {
		return notApplied, fmt.Errorf("productmetrics: stable root lock %q cannot be exchanged by enumerated cleanup", targetEntry.name)
	}
	sourceFD, err := directory.duplicateFD()
	if err != nil {
		return notApplied, err
	}
	defer closeUnixFD(sourceFD)
	targetFD, err := target.duplicateFD()
	if err != nil {
		return notApplied, err
	}
	defer closeUnixFD(targetFD)
	sourcePath := filepath.Join(directory.path, source.name)
	currentSource, sourceMissing, err := inspectEnumeratedEntry(sourceFD, source, sourcePath, directory.hooks)
	if err != nil {
		return notApplied, err
	}
	if sourceMissing {
		return notApplied, storagePathError("inspect entry-exchange source", sourcePath, fs.ErrNotExist)
	}
	targetPath := filepath.Join(target.path, targetEntry.name)
	currentTarget, targetMissing, err := inspectEnumeratedEntry(targetFD, targetEntry, targetPath, target.hooks)
	if err != nil {
		return notApplied, err
	}
	if targetMissing {
		return notApplied, storagePathError("inspect entry-exchange target", targetPath, fs.ErrNotExist)
	}
	if currentSource.dev == currentTarget.dev && currentSource.ino == currentTarget.ino {
		return notApplied, errStorageExchangeSameEntry
	}
	if currentTarget.mode&unix.S_IFMT == unix.S_IFDIR && pathContainsDirectory(targetPath, directory.path) {
		return notApplied, fmt.Errorf("%w: %q contains %q", errStorageExchangeAncestor, targetPath, sourcePath)
	}
	if currentSource.mode&unix.S_IFMT == unix.S_IFDIR && pathContainsDirectory(sourcePath, target.path) {
		return notApplied, fmt.Errorf("%w: %q contains %q", errStorageExchangeAncestor, sourcePath, targetPath)
	}
	sourceParent, sourceParentErr := metadataForFD(sourceFD, directory.path, storageTestHooks{})
	targetParent, targetParentErr := metadataForFD(targetFD, target.path, storageTestHooks{})
	if sourceParentErr != nil || targetParentErr != nil {
		return notApplied, errors.Join(sourceParentErr, targetParentErr)
	}
	applied := false
	var exchangeOutcomeErr error
	for attempts := 0; attempts < 8; attempts++ {
		exchangeApplied, exchangeErr := exchangeAt(sourceFD, source, sourcePath, targetFD, targetEntry, targetPath, directory.hooks)
		if exchangeApplied {
			postState, inspectErr := inspectExchangePostState(sourceFD, source, sourcePath, targetFD, targetEntry, targetPath)
			if inspectErr != nil || postState != exchangePostSwapped {
				applied = true
				exchangeOutcomeErr = errors.Join(exchangeErr, inspectErr, errStorageEntryChanged,
					errors.New("productmetrics: entry exchange did not retain the exact swapped incarnations"))
				break
			}
			applied = true
			if exchangeErr != nil && !errors.Is(exchangeErr, unix.EINTR) {
				exchangeOutcomeErr = exchangeErr
			}
			break
		}
		if exchangeErr == nil {
			return notApplied, errors.New("productmetrics: entry exchange reported neither application nor error")
		}
		if isUnsupportedExchangeError(exchangeErr) {
			return notApplied, fmt.Errorf("%w: %w", errStorageExchangeUnsupported, exchangeErr)
		}
		if !errors.Is(exchangeErr, unix.EINTR) {
			return notApplied, exchangeErr
		}
		postState, inspectErr := inspectExchangePostState(sourceFD, source, sourcePath, targetFD, targetEntry, targetPath)
		if inspectErr != nil || postState == exchangePostAmbiguous {
			applied = true
			exchangeOutcomeErr = errors.Join(
				exchangeErr, inspectErr, errors.New("productmetrics: directory exchange outcome is ambiguous"),
			)
			break
		}
		if postState == exchangePostSwapped {
			applied = true
			break
		}
	}
	if !applied {
		return notApplied, errors.New("productmetrics: directory exchange remained interrupted without application")
	}
	pending := storageRenameResult{state: storageRenameAppliedSyncPending}
	var syncErrors []error
	if err := syncDirectoryFD(sourceFD, directory.hooks); err != nil {
		syncErrors = append(syncErrors, fmt.Errorf("sync entry-exchange source: %w", err))
	}
	if sourceParent.dev != targetParent.dev || sourceParent.ino != targetParent.ino {
		if err := syncDirectoryFD(targetFD, target.hooks); err != nil {
			syncErrors = append(syncErrors, fmt.Errorf("sync entry-exchange target: %w", err))
		}
	}
	if err := errors.Join(append([]error{exchangeOutcomeErr}, syncErrors...)...); err != nil {
		return pending, err
	}
	return storageRenameResult{state: storageRenameAppliedDurable}, nil
}

type exchangePostState uint8

const (
	exchangePostAmbiguous exchangePostState = iota
	exchangePostUnchanged
	exchangePostSwapped
)

func inspectExchangePostState(sourceFD int, source storageEntry, sourcePath string, targetFD int, target storageEntry, targetPath string) (exchangePostState, error) {
	currentSource, sourceErr := metadataAt(sourceFD, source.name, sourcePath, storageTestHooks{})
	currentTarget, targetErr := metadataAt(targetFD, target.name, targetPath, storageTestHooks{})
	if sourceErr != nil || targetErr != nil {
		return exchangePostAmbiguous, errors.Join(sourceErr, targetErr)
	}
	sourceUnchanged := currentSource.dev == source.metadata.dev && currentSource.ino == source.metadata.ino
	targetUnchanged := currentTarget.dev == target.metadata.dev && currentTarget.ino == target.metadata.ino
	if sourceUnchanged && targetUnchanged {
		return exchangePostUnchanged, nil
	}
	sourceSwapped := currentSource.dev == target.metadata.dev && currentSource.ino == target.metadata.ino
	targetSwapped := currentTarget.dev == source.metadata.dev && currentTarget.ino == source.metadata.ino
	if sourceSwapped && targetSwapped {
		return exchangePostSwapped, nil
	}
	return exchangePostAmbiguous, nil
}

func isUnsupportedExchangeError(err error) bool {
	return errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTSUP) ||
		errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EXDEV)
}

func pathContainsDirectory(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return false
	}
	return true
}

func requireAbsentEntry(directoryFD int, name, path string, hooks storageTestHooks, conflict error) error {
	_, err := metadataAt(directoryFD, name, path, hooks)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return storagePathError("inspect rename destination", path, err)
	}
	return fmt.Errorf("%w: %q", conflict, path)
}

func renameReplaceAt(sourceFD int, sourceName string, source storageMetadata, targetFD int, targetName string,
	targetBefore storageMetadata, targetPresent bool, hooks storageTestHooks,
) (noReplaceOutcome, error) {
	for {
		if err := hooks.canStartStorageWork(); err != nil {
			return noReplaceNotApplied, err
		}
		before, inspectErr := inspectReplaceRenamePostState(sourceFD, sourceName, source, targetFD, targetName, targetBefore, targetPresent)
		if inspectErr != nil || before != noReplaceNotApplied {
			return noReplaceNotApplied, errors.Join(inspectErr, errStorageEntryChanged,
				errors.New("productmetrics: replacing rename entries changed before mutation"))
		}
		hooks.preparingMutation(storageStepRename, targetName)
		if err := hooks.canStartStorageWork(); err != nil {
			return noReplaceNotApplied, err
		}
		err := hooks.run(storageStepRename)
		if err == nil {
			err = unix.Renameat(sourceFD, sourceName, targetFD, targetName)
		}
		if err == nil {
			return noReplaceApplied, nil
		}
		if errors.Is(err, unix.EINTR) {
			outcome, inspectErr := inspectReplaceRenamePostState(sourceFD, sourceName, source, targetFD, targetName, targetBefore, targetPresent)
			if inspectErr != nil || outcome == noReplaceAmbiguous {
				return noReplaceAmbiguous, errors.Join(err, inspectErr, errors.New("productmetrics: replacing rename outcome is ambiguous"))
			}
			if outcome == noReplaceApplied {
				return noReplaceApplied, nil
			}
			continue
		}
		return noReplaceNotApplied, fmt.Errorf("productmetrics: rename private file: %w", err)
	}
}

func inspectReplaceRenamePostState(sourceFD int, sourceName string, source storageMetadata, targetFD int, targetName string,
	targetBefore storageMetadata, targetPresent bool,
) (noReplaceOutcome, error) {
	currentSource, sourceErr := metadataAt(sourceFD, sourceName, sourceName, storageTestHooks{})
	currentTarget, targetErr := metadataAt(targetFD, targetName, targetName, storageTestHooks{})
	sourceMissing := errors.Is(sourceErr, fs.ErrNotExist)
	targetMissing := errors.Is(targetErr, fs.ErrNotExist)
	if sourceErr != nil && !sourceMissing || targetErr != nil && !targetMissing {
		return noReplaceAmbiguous, errors.Join(sourceErr, targetErr)
	}
	sourceSame := sourceErr == nil && sameStorageIdentity(currentSource, source)
	targetUnchanged := targetMissing && !targetPresent || targetErr == nil && targetPresent && sameStorageIdentity(currentTarget, targetBefore)
	targetIsSource := targetErr == nil && sameStorageIdentity(currentTarget, source)
	if sourceSame && targetUnchanged {
		return noReplaceNotApplied, nil
	}
	if sourceMissing && targetIsSource {
		return noReplaceApplied, nil
	}
	return noReplaceAmbiguous, nil
}

func renameNoReplaceAt(sourceFD int, sourceName string, source storageMetadata, targetFD int, targetName string, hooks storageTestHooks) (noReplaceOutcome, error) {
	for {
		hooks.preparingMutation(storageStepRename, sourceName)
		if err := hooks.canStartStorageWork(); err != nil {
			return noReplaceNotApplied, err
		}
		err := hooks.run(storageStepRename)
		if err == nil {
			if err = validateNoReplaceRenamePreState(sourceFD, sourceName, source, targetFD, targetName); err == nil {
				if err = hooks.canStartStorageWork(); err == nil {
					err = platformRenameNoReplaceAt(sourceFD, sourceName, targetFD, targetName)
				}
			}
		}
		if err == nil {
			outcome, inspectErr := inspectNoReplaceRenamePostState(sourceFD, sourceName, source, targetFD, targetName)
			if inspectErr != nil || outcome != noReplaceApplied {
				return noReplaceAmbiguous, errors.Join(inspectErr, errStorageEntryChanged,
					errors.New("productmetrics: no-replace rename applied to an unexpected source incarnation"))
			}
			return outcome, nil
		}
		if errors.Is(err, unix.EINTR) {
			outcome, inspectErr := inspectNoReplaceRenamePostState(sourceFD, sourceName, source, targetFD, targetName)
			if inspectErr != nil || outcome == noReplaceAmbiguous {
				return noReplaceAmbiguous, errors.Join(err, inspectErr, errors.New("productmetrics: no-replace rename outcome is ambiguous"))
			}
			if outcome == noReplaceApplied {
				return noReplaceApplied, nil
			}
			continue
		}
		if errors.Is(err, unix.EEXIST) {
			outcome, inspectErr := inspectNoReplaceRenamePostState(sourceFD, sourceName, source, targetFD, targetName)
			if inspectErr == nil && outcome == noReplaceApplied {
				return noReplaceApplied, nil
			}
			return noReplaceNotApplied, errors.Join(errStorageDestinationExists, inspectErr)
		}
		// No replacing-rename fallback is safe here. In particular, ENOSYS,
		// EINVAL, and filesystem-specific unsupported errors must leave the
		// transition not applied rather than weakening destination exclusion.
		return noReplaceNotApplied, fmt.Errorf("productmetrics: atomic no-replace rename: %w", err)
	}
}

func validateNoReplaceRenamePreState(sourceFD int, sourceName string, source storageMetadata, targetFD int, targetName string) error {
	currentSource, err := metadataAt(sourceFD, sourceName, sourceName, storageTestHooks{})
	if err != nil {
		return errors.Join(errStorageEntryChanged, err)
	}
	if !sameStorageIdentity(currentSource, source) {
		return errStorageEntryChanged
	}
	_, err = metadataAt(targetFD, targetName, targetName, storageTestHooks{})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return errStorageDestinationExists
}

func inspectNoReplaceRenamePostState(sourceFD int, sourceName string, source storageMetadata, targetFD int, targetName string) (noReplaceOutcome, error) {
	currentSource, sourceErr := metadataAt(sourceFD, sourceName, sourceName, storageTestHooks{})
	currentTarget, targetErr := metadataAt(targetFD, targetName, targetName, storageTestHooks{})
	sourceMissing := errors.Is(sourceErr, fs.ErrNotExist)
	targetMissing := errors.Is(targetErr, fs.ErrNotExist)
	if sourceErr != nil && !sourceMissing || targetErr != nil && !targetMissing {
		return noReplaceAmbiguous, errors.Join(sourceErr, targetErr)
	}
	sourceSame := sourceErr == nil && sameStorageIdentity(currentSource, source)
	targetSame := targetErr == nil && sameStorageIdentity(currentTarget, source)
	if sourceSame && targetMissing {
		return noReplaceNotApplied, nil
	}
	if sourceMissing && targetSame {
		return noReplaceApplied, nil
	}
	return noReplaceAmbiguous, nil
}

func sameStorageIdentity(left, right storageMetadata) bool {
	return left.dev == right.dev && left.ino == right.ino
}

func exchangeAt(sourceFD int, source storageEntry, sourcePath string, targetFD int, target storageEntry, targetPath string, hooks storageTestHooks) (bool, error) {
	if hooks.beforeExchange != nil {
		if err := hooks.beforeExchange(); err != nil {
			return false, fmt.Errorf("productmetrics: injected pre-exchange failure: %w", err)
		}
	}
	if err := hooks.run(storageStepRename); err != nil {
		return false, fmt.Errorf("productmetrics: injected directory-exchange failure: %w", err)
	}
	preState, inspectErr := inspectExchangePostState(sourceFD, source, sourcePath, targetFD, target, targetPath)
	if inspectErr != nil || preState != exchangePostUnchanged {
		return false, errors.Join(inspectErr, errStorageEntryChanged,
			errors.New("productmetrics: entry exchange endpoints changed before mutation"))
	}
	if err := platformExchangeAt(sourceFD, source.name, targetFD, target.name); err != nil {
		return false, fmt.Errorf("productmetrics: atomic directory exchange: %w", err)
	}
	if hooks.afterExchange != nil {
		if err := hooks.afterExchange(); err != nil {
			return true, fmt.Errorf("productmetrics: injected post-exchange outcome: %w", err)
		}
	}
	return true, nil
}

func (directory *unixStorageDirectory) syncDirectory() error {
	if !directory.mutable {
		return errors.New("productmetrics: read-only storage cannot sync a directory")
	}
	directoryFD, err := directory.duplicateFD()
	if err != nil {
		return err
	}
	defer closeUnixFD(directoryFD)
	if err := syncDirectoryFD(directoryFD, directory.hooks); err != nil {
		return fmt.Errorf("productmetrics: sync retained directory: %w", err)
	}
	return nil
}
