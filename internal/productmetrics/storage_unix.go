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
	hooks         storageTestHooks
}

type unixStorageIterator struct {
	mu          sync.Mutex
	file        *os.File
	path        string
	euid        uint32
	hooks       storageTestHooks
	pendingName string
}

func platformOpenStorageRoot(home gchome.ProductUsageHome, mutable bool, hooks storageTestHooks) (storageDirectoryBackend, error) {
	if !isCleanAbsoluteProductRoot(home) {
		return nil, errors.New("productmetrics: invalid or unstable product-usage home")
	}
	euid := uint32(os.Geteuid())
	rootFD, err := openDirectoryPath("/")
	if err != nil {
		return nil, storagePathError("open", "/", err)
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
		metadata, metadataErr := metadataForFD(nextFD, nextPath, hooks)
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

		hooks.openedComponent(nextPath)
		if err := revalidateOpenedDirectory(currentFD, component, nextFD, nextPath, euid, privateBoundary || created, created, hooks); err != nil {
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
			if err := syncDirectoryFD(nextFD, hooks); err != nil {
				_ = unix.Close(nextFD)
				_ = unix.Close(currentFD)
				return nil, fmt.Errorf("productmetrics: recover private-directory sync: %w", err)
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

func openDirectoryPath(path string) (int, error) {
	for {
		fd, err := unix.Open(path, unixDirectoryOpenFlags, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return fd, err
	}
}

func openDirectoryComponent(parentFD int, name string, mutable, stickyPending bool, euid uint32, hooks storageTestHooks, path string) (int, bool, error) {
	for {
		fd, err := unix.Openat(parentFD, name, unixDirectoryOpenFlags, 0)
		if err == nil {
			return fd, false, nil
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if !errors.Is(err, fs.ErrNotExist) || !mutable {
			return -1, false, err
		}
		if stickyPending {
			return -1, false, errors.New("root-owned sticky ancestor has no later existing private boundary")
		}
		if err := unix.Mkdirat(parentFD, name, 0o700); err != nil {
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			return -1, false, err
		}
		fd, err = unix.Openat(parentFD, name, unixDirectoryOpenFlags, 0)
		if err != nil {
			return -1, true, err
		}
		if err := unix.Fchmod(fd, 0o700); err != nil {
			_ = unix.Close(fd)
			return -1, true, err
		}
		metadata, err := metadataForFD(fd, path, hooks)
		if err != nil {
			_ = unix.Close(fd)
			return -1, true, err
		}
		if err := validatePrivateDirectory(metadata, path, euid, true); err != nil {
			_ = unix.Close(fd)
			return -1, true, err
		}
		if err := syncDirectoryFD(fd, hooks); err != nil {
			_ = unix.Close(fd)
			return -1, true, fmt.Errorf("sync new directory: %w", err)
		}
		if err := syncDirectoryFD(parentFD, hooks); err != nil {
			_ = unix.Close(fd)
			return -1, true, fmt.Errorf("sync parent after directory creation: %w", err)
		}
		return fd, true, nil
	}
}

func revalidateOpenedDirectory(parentFD int, name string, fd int, path string, euid uint32, private, exactMode bool, hooks storageTestHooks) error {
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
	return storageMetadata{
		uid:              stat.Uid,
		mode:             uint32(stat.Mode),  //nolint:unconvert // Darwin's field is uint16.
		nlink:            uint64(stat.Nlink), //nolint:unconvert // Darwin's field is uint16.
		dev:              uint64(stat.Dev),   //nolint:unconvert // Darwin's field is signed int32.
		ino:              uint64(stat.Ino),   //nolint:unconvert // Keep one cross-platform representation.
		size:             stat.Size,
		mtimeSeconds:     int64(stat.Mtim.Sec),  //nolint:unconvert // 32-bit Linux exposes int32 timespec fields.
		mtimeNanoseconds: int64(stat.Mtim.Nsec), //nolint:unconvert // Keep one cross-platform representation.
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
	if err := validatePrivateDirectory(metadata, directory.path, directory.euid, false); err != nil {
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
		if err := validateAndRevalidatePrivateComponent(currentFD, component, nextFD, nextPath, directory.euid, created, directory.hooks); err != nil {
			_ = unix.Close(nextFD)
			_ = unix.Close(currentFD)
			return nil, err
		}
		// Any existing descendant reached from a mutable handle may be the
		// visible remainder of an earlier creation whose parent sync failed.
		// Recover child then parent before returning a write-capable handle,
		// regardless of whether this particular open allowed creation.
		if directory.mutable && !created {
			if err := syncDirectoryFD(nextFD, directory.hooks); err != nil {
				_ = unix.Close(nextFD)
				_ = unix.Close(currentFD)
				return nil, fmt.Errorf("productmetrics: recover private-directory sync: %w", err)
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
		hooks:         directory.hooks,
	}, nil
}

func validateAndRevalidatePrivateComponent(parentFD int, name string, fd int, path string, euid uint32, exactMode bool, hooks storageTestHooks) error {
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
		file:  file,
		path:  directory.path,
		euid:  directory.euid,
		hooks: directory.hooks,
	}, nil
}

func (directory *unixStorageDirectory) openIteratorFD() (int, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if directory.fd < 0 {
		return -1, errStorageClosed
	}
	var iteratorFD int
	var err error
	for {
		iteratorFD, err = unix.Openat(directory.fd, ".", unixDirectoryOpenFlags, 0)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		return -1, storagePathError("open directory iterator", directory.path, err)
	}
	if err := revalidateOpenedDirectory(directory.fd, ".", iteratorFD, directory.path, directory.euid, true, false, directory.hooks); err != nil {
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
	if err := validatePrivateDirectory(metadata, iterator.path, iterator.euid, false); err != nil {
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

func (directory *unixStorageDirectory) readFile(name string, maximumBytes int64) ([]byte, error) {
	directoryFD, err := directory.duplicateFD()
	if err != nil {
		return nil, err
	}
	defer closeUnixFD(directoryFD)
	path := filepath.Join(directory.path, name)
	fileFD, err := openFileAt(directoryFD, name, unixFileReadFlags, 0)
	if err != nil {
		return nil, storagePathError("open file", path, err)
	}
	defer closeUnixFD(fileFD)
	metadata, err := validateOpenedRegularFile(directoryFD, name, fileFD, path, directory.euid, false, directory.hooks)
	if err != nil {
		return nil, err
	}
	if metadata.size < 0 || metadata.size > maximumBytes {
		return nil, fmt.Errorf("productmetrics: file %q exceeds the read limit", path)
	}
	return readFDWithLimit(fileFD, maximumBytes)
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

func readFDWithLimit(fd int, maximumBytes int64) ([]byte, error) {
	capacity := maximumBytes
	if capacity > 32*1024 {
		capacity = 32 * 1024
	}
	result := make([]byte, 0, int(capacity))
	buffer := make([]byte, 4096)
	for {
		read, err := unix.Read(fd, buffer)
		if read > 0 {
			if int64(len(result))+int64(read) > maximumBytes {
				return nil, errors.New("productmetrics: file exceeds the read limit")
			}
			result = append(result, buffer[:read]...)
		}
		if err == nil {
			if read == 0 {
				return result, nil
			}
			continue
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, io.EOF) {
			return result, nil
		}
		return nil, fmt.Errorf("productmetrics: read private file: %w", err)
	}
}

func (directory *unixStorageDirectory) writeFileAtomic(name string, data []byte) (returnErr error) {
	return directory.writeFileAtomically(name, data, false)
}

func (directory *unixStorageDirectory) writeFileAtomicNoReplace(name string, data []byte) error {
	return directory.writeFileAtomically(name, data, true)
}

func (directory *unixStorageDirectory) writeFileAtomically(name string, data []byte, noReplace bool) (returnErr error) {
	if !directory.mutable {
		return errors.New("productmetrics: read-only storage cannot write")
	}
	directoryFD, err := directory.duplicateFD()
	if err != nil {
		return err
	}
	defer closeUnixFD(directoryFD)
	path := filepath.Join(directory.path, name)
	if noReplace {
		if err := requireAbsentEntry(directoryFD, name, path, directory.hooks, errStorageEntryExists); err != nil {
			return err
		}
	} else {
		if err := validateExistingRegularFile(directoryFD, name, path, directory.euid, directory.hooks); err != nil {
			return err
		}
	}

	tempName, tempFD, err := createPrivateTempFile(directoryFD, directory.path, directory.euid, directory.hooks)
	if err != nil {
		return err
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
		return fmt.Errorf("productmetrics: write temporary file: %w", err)
	}
	if err := syncFileFD(tempFD, directory.hooks); err != nil {
		return fmt.Errorf("productmetrics: sync temporary file: %w", err)
	}
	if err := unix.Close(tempFD); err != nil {
		tempFD = -1
		return fmt.Errorf("productmetrics: close temporary file: %w", err)
	}
	tempFD = -1
	if noReplace {
		if err := installNoReplace(directoryFD, tempName, name, directory.hooks); err != nil {
			return storagePathError("install no-replace file", path, err)
		}
		if err := unix.Unlinkat(directoryFD, tempName, 0); err != nil {
			return fmt.Errorf("productmetrics: remove installed temporary link: %w", err)
		}
		tempExists = false
	} else {
		if err := renameAt(directoryFD, tempName, directoryFD, name, directory.hooks); err != nil {
			return storagePathError("rename temporary file", path, err)
		}
		tempExists = false
	}
	if err := syncDirectoryFD(directoryFD, directory.hooks); err != nil {
		return fmt.Errorf("productmetrics: sync directory after atomic rename: %w", err)
	}
	return nil
}

func installNoReplace(directoryFD int, sourceName, targetName string, hooks storageTestHooks) error {
	for {
		if err := hooks.run(storageStepInstall); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		if err := unix.Linkat(directoryFD, sourceName, directoryFD, targetName, 0); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if errors.Is(err, unix.EEXIST) {
				return errStorageEntryExists
			}
			return err
		}
		return nil
	}
}

func createPrivateTempFile(directoryFD int, directoryPath string, euid uint32, hooks storageTestHooks) (string, int, error) {
	for attempts := 0; attempts < 64; attempts++ {
		sequence := storageTempSequence.Add(1)
		name := fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), sequence)
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

func validateExistingRegularFile(directoryFD int, name, path string, euid uint32, hooks storageTestHooks) error {
	metadata, err := metadataAt(directoryFD, name, path, hooks)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return storagePathError("inspect existing file", path, err)
	}
	return validatePrivateRegularFile(metadata, path, euid, false)
}

func (directory *unixStorageDirectory) removeFile(name string) error {
	if !directory.mutable {
		return errors.New("productmetrics: read-only storage cannot delete")
	}
	directoryFD, err := directory.duplicateFD()
	if err != nil {
		return err
	}
	defer closeUnixFD(directoryFD)
	path := filepath.Join(directory.path, name)
	metadata, err := metadataAt(directoryFD, name, path, directory.hooks)
	if errors.Is(err, fs.ErrNotExist) {
		if err := syncDirectoryFD(directoryFD, directory.hooks); err != nil {
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
	if err := directory.hooks.run(storageStepDelete); err != nil {
		return fmt.Errorf("productmetrics: injected delete failure: %w", err)
	}
	if err := unix.Unlinkat(directoryFD, name, 0); err != nil {
		return storagePathError("delete file", path, err)
	}
	if err := syncDirectoryFD(directoryFD, directory.hooks); err != nil {
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
		return fmt.Errorf("productmetrics: enumerated entry %q is a directory", path)
	}
	if err := unlinkAt(directoryFD, entry.name, 0, storageStepUnlink, directory.hooks); err != nil {
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
	if err := validatePrivateDirectory(current, path, directory.euid, false); err != nil {
		return err
	}
	if err := unlinkAt(directoryFD, entry.name, unix.AT_REMOVEDIR, storageStepRmdir, directory.hooks); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return syncMissingEntryParent(directoryFD, directory.hooks)
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

func unlinkAt(directoryFD int, name string, flags int, step storageStep, hooks storageTestHooks) error {
	for {
		if err := hooks.run(step); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return err
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
	if err := renameNoReplaceAt(sourceFD, name, targetFD, targetName, directory.hooks); err != nil {
		if errors.Is(err, errStorageDestinationExists) {
			return notApplied, fmt.Errorf("%w: %q", err, targetPath)
		}
		return notApplied, err
	}
	pending := storageRenameResult{state: storageRenameAppliedSyncPending}
	var syncErrors []error
	if err := syncDirectoryFD(sourceFD, directory.hooks); err != nil {
		syncErrors = append(syncErrors, fmt.Errorf("sync rename source directory: %w", err))
	}
	if sourceDirectoryMetadata.dev != targetDirectoryMetadata.dev || sourceDirectoryMetadata.ino != targetDirectoryMetadata.ino {
		if err := syncDirectoryFD(targetFD, target.hooks); err != nil {
			syncErrors = append(syncErrors, fmt.Errorf("sync rename target directory: %w", err))
		}
	}
	if err := errors.Join(syncErrors...); err != nil {
		return pending, err
	}
	return storageRenameResult{state: storageRenameAppliedDurable}, nil
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

func renameAt(sourceFD int, sourceName string, targetFD int, targetName string, hooks storageTestHooks) error {
	for {
		if err := hooks.run(storageStepRename); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return fmt.Errorf("productmetrics: injected rename failure: %w", err)
		}
		if err := unix.Renameat(sourceFD, sourceName, targetFD, targetName); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return fmt.Errorf("productmetrics: rename private file: %w", err)
		}
		return nil
	}
}

func renameNoReplaceAt(sourceFD int, sourceName string, targetFD int, targetName string, hooks storageTestHooks) error {
	for {
		if err := hooks.run(storageStepRename); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return fmt.Errorf("productmetrics: injected rename failure: %w", err)
		}
		if err := platformRenameNoReplaceAt(sourceFD, sourceName, targetFD, targetName); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if errors.Is(err, unix.EEXIST) {
				return errStorageDestinationExists
			}
			// No replacing-rename fallback is safe here. In particular, ENOSYS,
			// EINVAL, and filesystem-specific unsupported errors must leave the
			// transition not applied rather than weakening destination exclusion.
			return fmt.Errorf("productmetrics: atomic no-replace rename: %w", err)
		}
		return nil
	}
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
