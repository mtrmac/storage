package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/containers/storage/pkg/idtools"
	"github.com/containers/storage/pkg/ioutils"
	"github.com/containers/storage/pkg/lockfile"
	"github.com/containers/storage/pkg/stringid"
	"github.com/containers/storage/pkg/truncindex"
	digest "github.com/opencontainers/go-digest"
	"github.com/sirupsen/logrus"
)

type containerLocations uint8

// The backing store is split in two json files, one (the volatile)
// that is written without fsync() meaning it isn't as robust to
// unclean shutdown
const (
	stableContainerLocation containerLocations = 1 << iota
	volatileContainerLocation

	numContainerLocationIndex = iota
)

func containerLocationFromIndex(index int) containerLocations {
	return 1 << index
}

// A Container is a reference to a read-write layer with metadata.
type Container struct {
	// ID is either one which was specified at create-time, or a random
	// value which was generated by the library.
	ID string `json:"id"`

	// Names is an optional set of user-defined convenience values.  The
	// container can be referred to by its ID or any of its names.  Names
	// are unique among containers.
	Names []string `json:"names,omitempty"`

	// ImageID is the ID of the image which was used to create the container.
	ImageID string `json:"image"`

	// LayerID is the ID of the read-write layer for the container itself.
	// It is assumed that the image's top layer is the parent of the container's
	// read-write layer.
	LayerID string `json:"layer"`

	// Metadata is data we keep for the convenience of the caller.  It is not
	// expected to be large, since it is kept in memory.
	Metadata string `json:"metadata,omitempty"`

	// BigDataNames is a list of names of data items that we keep for the
	// convenience of the caller.  They can be large, and are only in
	// memory when being read from or written to disk.
	BigDataNames []string `json:"big-data-names,omitempty"`

	// BigDataSizes maps the names in BigDataNames to the sizes of the data
	// that has been stored, if they're known.
	BigDataSizes map[string]int64 `json:"big-data-sizes,omitempty"`

	// BigDataDigests maps the names in BigDataNames to the digests of the
	// data that has been stored, if they're known.
	BigDataDigests map[string]digest.Digest `json:"big-data-digests,omitempty"`

	// Created is the datestamp for when this container was created.  Older
	// versions of the library did not track this information, so callers
	// will likely want to use the IsZero() method to verify that a value
	// is set before using it.
	Created time.Time `json:"created,omitempty"`

	// UIDMap and GIDMap are used for setting up a container's root
	// filesystem for use inside of a user namespace where UID mapping is
	// being used.
	UIDMap []idtools.IDMap `json:"uidmap,omitempty"`
	GIDMap []idtools.IDMap `json:"gidmap,omitempty"`

	Flags map[string]interface{} `json:"flags,omitempty"`

	// volatileStore is true if the container is from the volatile json file
	volatileStore bool `json:"-"`
}

// rwContainerStore provides bookkeeping for information about Containers.
type rwContainerStore interface {
	metadataStore
	containerBigDataStore
	flaggableStore

	// startWriting makes sure the store is fresh, and locks it for writing.
	// If this succeeds, the caller MUST call stopWriting().
	startWriting() error

	// stopWriting releases locks obtained by startWriting.
	stopWriting()

	// startReading makes sure the store is fresh, and locks it for reading.
	// If this succeeds, the caller MUST call stopReading().
	startReading() error

	// stopReading releases locks obtained by startReading.
	stopReading()

	// create creates a container that has a specified ID (or generates a
	// random one if an empty value is supplied) and optional names,
	// based on the specified image, using the specified layer as its
	// read-write layer.
	// The maps in the container's options structure are recorded for the
	// convenience of the caller, nothing more.
	create(id string, names []string, image, layer string, options *ContainerOptions) (*Container, error)

	// updateNames modifies names associated with a  container based on (op, names).
	updateNames(id string, names []string, op updateNameOperation) error

	// Get retrieves information about a container given an ID or name.
	Get(id string) (*Container, error)

	// Exists checks if there is a container with the given ID or name.
	Exists(id string) bool

	// Delete removes the record of the container.
	Delete(id string) error

	// Wipe removes records of all containers.
	Wipe() error

	// Lookup attempts to translate a name to an ID.  Most methods do this
	// implicitly.
	Lookup(name string) (string, error)

	// Containers returns a slice enumerating the known containers.
	Containers() ([]Container, error)

	// Clean up unreferenced datadirs
	GarbageCollect() error
}

type containerStore struct {
	// The following fields are only set when constructing containerStore, and must never be modified afterwards.
	// They are safe to access without any other locking.
	lockfile *lockfile.LockFile // Synchronizes readers vs. writers of the _filesystem data_, both cross-process and in-process.
	dir      string
	jsonPath [numContainerLocationIndex]string

	inProcessLock sync.RWMutex // Can _only_ be obtained with lockfile held.
	// The following fields can only be read/written with read/write ownership of inProcessLock, respectively.
	// Almost all users should use startReading() or startWriting().
	lastWrite  lockfile.LastWrite
	containers []*Container
	idindex    *truncindex.TruncIndex
	byid       map[string]*Container
	bylayer    map[string]*Container
	byname     map[string]*Container
}

func copyContainer(c *Container) *Container {
	return &Container{
		ID:             c.ID,
		Names:          copyStringSlice(c.Names),
		ImageID:        c.ImageID,
		LayerID:        c.LayerID,
		Metadata:       c.Metadata,
		BigDataNames:   copyStringSlice(c.BigDataNames),
		BigDataSizes:   copyStringInt64Map(c.BigDataSizes),
		BigDataDigests: copyStringDigestMap(c.BigDataDigests),
		Created:        c.Created,
		UIDMap:         copyIDMap(c.UIDMap),
		GIDMap:         copyIDMap(c.GIDMap),
		Flags:          copyStringInterfaceMap(c.Flags),
		volatileStore:  c.volatileStore,
	}
}

func (c *Container) MountLabel() string {
	if label, ok := c.Flags[mountLabelFlag].(string); ok {
		return label
	}
	return ""
}

func (c *Container) ProcessLabel() string {
	if label, ok := c.Flags[processLabelFlag].(string); ok {
		return label
	}
	return ""
}

func (c *Container) MountOpts() []string {
	switch value := c.Flags[mountOptsFlag].(type) {
	case []string:
		return value
	case []interface{}:
		var mountOpts []string
		for _, v := range value {
			if flag, ok := v.(string); ok {
				mountOpts = append(mountOpts, flag)
			}
		}
		return mountOpts
	default:
		return nil
	}
}

// The caller must hold r.inProcessLock for reading.
func containerLocation(c *Container) containerLocations {
	if c.volatileStore {
		return volatileContainerLocation
	}
	return stableContainerLocation
}

// startWritingWithReload makes sure the store is fresh if canReload, and locks it for writing.
// If this succeeds, the caller MUST call stopWriting().
//
// This is an internal implementation detail of containerStore construction, every other caller
// should use startWriting() instead.
func (r *containerStore) startWritingWithReload(canReload bool) error {
	r.lockfile.Lock()
	r.inProcessLock.Lock()
	succeeded := false
	defer func() {
		if !succeeded {
			r.inProcessLock.Unlock()
			r.lockfile.Unlock()
		}
	}()

	if canReload {
		if _, err := r.reloadIfChanged(true); err != nil {
			return err
		}
	}

	succeeded = true
	return nil
}

// startWriting makes sure the store is fresh, and locks it for writing.
// If this succeeds, the caller MUST call stopWriting().
func (r *containerStore) startWriting() error {
	return r.startWritingWithReload(true)
}

// stopWriting releases locks obtained by startWriting.
func (r *containerStore) stopWriting() {
	r.inProcessLock.Unlock()
	r.lockfile.Unlock()
}

// startReading makes sure the store is fresh, and locks it for reading.
// If this succeeds, the caller MUST call stopReading().
func (r *containerStore) startReading() error {
	// inProcessLocked calls the nested function with r.inProcessLock held for writing.
	inProcessLocked := func(fn func() error) error {
		r.inProcessLock.Lock()
		defer r.inProcessLock.Unlock()
		return fn()
	}

	r.lockfile.RLock()
	unlockFn := r.lockfile.Unlock // A function to call to clean up, or nil.
	defer func() {
		if unlockFn != nil {
			unlockFn()
		}
	}()
	r.inProcessLock.RLock()
	unlockFn = r.stopReading

	// If we are lucky, we can just hold the read locks, check that we are fresh, and continue.
	_, modified, err := r.modified()
	if err != nil {
		return err
	}
	if modified {
		// We are unlucky, and need to reload.
		// NOTE: Multiple goroutines can get to this place approximately simultaneously.
		r.inProcessLock.RUnlock()
		unlockFn = r.lockfile.Unlock

		// r.lastWrite can change at this point if another goroutine reloads the store before us. That’s why we don’t unconditionally
		// trigger a load below; we (lock and) reloadIfChanged() again.

		// First try reloading with r.lockfile held for reading.
		// r.inProcessLock will serialize all goroutines that got here;
		// each will re-check on-disk state vs. r.lastWrite, and the first one will actually reload the data.
		var tryLockedForWriting bool
		if err := inProcessLocked(func() error {
			// We could optimize this further: The r.lockfile.GetLastWrite() value shouldn’t change as long as we hold r.lockfile,
			// so if r.lastWrite was already updated, we don’t need to actually read the on-filesystem lock.
			var err error
			tryLockedForWriting, err = r.reloadIfChanged(false)
			return err
		}); err != nil {
			if !tryLockedForWriting {
				return err
			}
			// Not good enough, we need r.lockfile held for writing. So, let’s do that.
			unlockFn()
			unlockFn = nil

			r.lockfile.Lock()
			unlockFn = r.lockfile.Unlock
			if err := inProcessLocked(func() error {
				_, err := r.reloadIfChanged(true)
				return err
			}); err != nil {
				return err
			}
			unlockFn()
			unlockFn = nil

			r.lockfile.RLock()
			unlockFn = r.lockfile.Unlock
			// We need to check for a reload once more because the on-disk state could have been modified
			// after we released the lock.
			// If that, _again_, finds inconsistent state, just give up.
			// We could, plausibly, retry a few times, but that inconsistent state (duplicate container names)
			// shouldn’t be saved (by correct implementations) in the first place.
			if err := inProcessLocked(func() error {
				_, err := r.reloadIfChanged(false)
				return err
			}); err != nil {
				return fmt.Errorf("(even after successfully cleaning up once:) %w", err)
			}
		}

		// NOTE that we hold neither a read nor write inProcessLock at this point. That’s fine in ordinary operation, because
		// the on-filesystem r.lockfile should protect us against (cooperating) writers, and any use of r.inProcessLock
		// protects us against in-process writers modifying data.
		// In presence of non-cooperating writers, we just ensure that 1) the in-memory data is not clearly out-of-date
		// and 2) access to the in-memory data is not racy;
		// but we can’t protect against those out-of-process writers modifying _files_ while we are assuming they are in a consistent state.

		r.inProcessLock.RLock()
	}
	unlockFn = nil
	return nil
}

// stopReading releases locks obtained by startReading.
func (r *containerStore) stopReading() {
	r.inProcessLock.RUnlock()
	r.lockfile.Unlock()
}

// modified returns true if the on-disk state has changed (i.e. if reloadIfChanged may need to modify the store),
// and a lockfile.LastWrite value for that update.
//
// The caller must hold r.lockfile for reading _or_ writing.
// The caller must hold r.inProcessLock for reading or writing.
func (r *containerStore) modified() (lockfile.LastWrite, bool, error) {
	return r.lockfile.ModifiedSince(r.lastWrite)
}

// reloadIfChanged reloads the contents of the store from disk if it is changed.
//
// The caller must hold r.lockfile for reading _or_ writing; lockedForWriting is true
// if it is held for writing.
//
// The caller must hold r.inProcessLock for WRITING.
//
// If !lockedForWriting and this function fails, the return value indicates whether
// reloadIfChanged() with lockedForWriting could succeed.
func (r *containerStore) reloadIfChanged(lockedForWriting bool) (bool, error) {
	lastWrite, modified, err := r.modified()
	if err != nil {
		return false, err
	}
	// We require callers to always hold r.inProcessLock for WRITING, even if they might not end up calling r.load()
	// and modify no fields, to ensure they see fresh data:
	// r.lockfile.Modified() only returns true once per change.  Without an exclusive lock,
	// one goroutine might see r.lockfile.Modified() == true and decide to load, and in the meanwhile another one could
	// see r.lockfile.Modified() == false and proceed to use in-memory data without noticing it is stale.
	if modified {
		if tryLockedForWriting, err := r.load(lockedForWriting); err != nil {
			return tryLockedForWriting, err // r.lastWrite is unchanged, so we will load the next time again.
		}
		r.lastWrite = lastWrite
	}
	return false, nil
}

// Requires startReading or startWriting.
func (r *containerStore) Containers() ([]Container, error) {
	containers := make([]Container, len(r.containers))
	for i := range r.containers {
		containers[i] = *copyContainer(r.containers[i])
	}
	return containers, nil
}

// This looks for datadirs in the store directory that are not referenced
// by the json file and removes it. These can happen in the case of unclean
// shutdowns or regular restarts in transient store mode.
// Requires startReading.
func (r *containerStore) GarbageCollect() error {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		// Unexpected, don't try any GC
		return err
	}

	for _, entry := range entries {
		id := entry.Name()
		// Does it look like a datadir directory?
		if !entry.IsDir() || stringid.ValidateID(id) != nil {
			continue
		}

		// Should the id be there?
		if r.byid[id] != nil {
			continue
		}

		// Otherwise remove datadir
		logrus.Debugf("removing %q", filepath.Join(r.dir, id))
		moreErr := os.RemoveAll(filepath.Join(r.dir, id))
		// Propagate first error
		if moreErr != nil && err == nil {
			err = moreErr
		}
	}

	return err
}

func (r *containerStore) datadir(id string) string {
	return filepath.Join(r.dir, id)
}

func (r *containerStore) datapath(id, key string) string {
	return filepath.Join(r.datadir(id), makeBigDataBaseName(key))
}

// load reloads the contents of the store from disk.
//
// Most callers should call reloadIfChanged() instead, to avoid overhead and to correctly
// manage r.lastWrite.
//
// The caller must hold r.lockfile for reading _or_ writing; lockedForWriting is true
// if it is held for writing.
// The caller must hold r.inProcessLock for WRITING.
//
// If !lockedForWriting and this function fails, the return value indicates whether
// retrying with lockedForWriting could succeed.
func (r *containerStore) load(lockedForWriting bool) (bool, error) {
	var modifiedLocations containerLocations
	containers := []*Container{}

	ids := make(map[string]*Container)

	for locationIndex := 0; locationIndex < numContainerLocationIndex; locationIndex++ {
		location := containerLocationFromIndex(locationIndex)
		rpath := r.jsonPath[locationIndex]

		data, err := os.ReadFile(rpath)
		if err != nil && !os.IsNotExist(err) {
			return false, err
		}

		locationContainers := []*Container{}
		if len(data) != 0 {
			if err := json.Unmarshal(data, &locationContainers); err != nil {
				return false, fmt.Errorf("loading %q: %w", rpath, err)
			}
		}

		for _, container := range locationContainers {
			// There should be no duplicated ids between json files, but lets check to be sure
			if ids[container.ID] != nil {
				continue // skip invalid duplicated container
			}
			// Remember where the container came from
			if location == volatileContainerLocation {
				container.volatileStore = true
			}
			containers = append(containers, container)
			ids[container.ID] = container
		}
	}

	idlist := make([]string, 0, len(containers))
	layers := make(map[string]*Container)
	names := make(map[string]*Container)
	var errorToResolveBySaving error // == nil
	for n, container := range containers {
		idlist = append(idlist, container.ID)
		layers[container.LayerID] = containers[n]
		for _, name := range container.Names {
			if conflict, ok := names[name]; ok {
				r.removeName(conflict, name)
				errorToResolveBySaving = errors.New("container store is inconsistent and the current caller does not hold a write lock")
				modifiedLocations |= containerLocation(container)
			}
			names[name] = containers[n]
		}
	}

	r.containers = containers
	r.idindex = truncindex.NewTruncIndex(idlist) // Invalid values in idlist are ignored: they are not a reason to refuse processing the whole store.
	r.byid = ids
	r.bylayer = layers
	r.byname = names
	if errorToResolveBySaving != nil {
		if !lockedForWriting {
			return true, errorToResolveBySaving
		}
		return false, r.save(modifiedLocations)
	}
	return false, nil
}

// save saves the contents of the store to disk.
// The caller must hold r.lockfile locked for writing.
// The caller must hold r.inProcessLock for reading (but usually holds it for writing in order to make the desired changes).
func (r *containerStore) save(saveLocations containerLocations) error {
	r.lockfile.AssertLockedForWriting()
	// This must be done before we write the file, because the process could be terminated
	// after the file is written but before the lock file is updated.
	lw, err := r.lockfile.RecordWrite()
	if err != nil {
		return err
	}
	r.lastWrite = lw
	for locationIndex := 0; locationIndex < numContainerLocationIndex; locationIndex++ {
		location := containerLocationFromIndex(locationIndex)
		if location&saveLocations == 0 {
			continue
		}
		rpath := r.jsonPath[locationIndex]
		if err := os.MkdirAll(filepath.Dir(rpath), 0o700); err != nil {
			return err
		}
		subsetContainers := make([]*Container, 0, len(r.containers))
		for _, container := range r.containers {
			if containerLocation(container) == location {
				subsetContainers = append(subsetContainers, container)
			}
		}

		jdata, err := json.Marshal(&subsetContainers)
		if err != nil {
			return err
		}
		var opts *ioutils.AtomicFileWriterOptions
		if location == volatileContainerLocation {
			opts = &ioutils.AtomicFileWriterOptions{
				NoSync: true,
			}
		}
		if err := ioutils.AtomicWriteFileWithOpts(rpath, jdata, 0o600, opts); err != nil {
			return err
		}
	}
	return nil
}

// saveFor saves the contents of the store relevant for modifiedContainer to disk.
// The caller must hold r.lockfile locked for writing.
// The caller must hold r.inProcessLock for reading (but usually holds it for writing in order to make the desired changes).
func (r *containerStore) saveFor(modifiedContainer *Container) error {
	return r.save(containerLocation(modifiedContainer))
}

func newContainerStore(dir string, runDir string, transient bool) (rwContainerStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	volatileDir := dir
	if transient {
		if err := os.MkdirAll(runDir, 0o700); err != nil {
			return nil, err
		}
		volatileDir = runDir
	}
	lockfile, err := lockfile.GetLockFile(filepath.Join(volatileDir, "containers.lock"))
	if err != nil {
		return nil, err
	}
	cstore := containerStore{
		lockfile: lockfile,
		dir:      dir,
		jsonPath: [numContainerLocationIndex]string{
			filepath.Join(dir, "containers.json"),
			filepath.Join(volatileDir, "volatile-containers.json"),
		},

		containers: []*Container{},
		byid:       make(map[string]*Container),
		bylayer:    make(map[string]*Container),
		byname:     make(map[string]*Container),
	}

	if err := cstore.startWritingWithReload(false); err != nil {
		return nil, err
	}
	cstore.lastWrite, err = cstore.lockfile.GetLastWrite()
	if err != nil {
		return nil, err
	}
	defer cstore.stopWriting()
	if _, err := cstore.load(true); err != nil {
		return nil, err
	}
	return &cstore, nil
}

// Requires startReading or startWriting.
func (r *containerStore) lookup(id string) (*Container, bool) {
	if container, ok := r.byid[id]; ok {
		return container, ok
	} else if container, ok := r.byname[id]; ok {
		return container, ok
	} else if container, ok := r.bylayer[id]; ok {
		return container, ok
	} else if longid, err := r.idindex.Get(id); err == nil {
		if container, ok := r.byid[longid]; ok {
			return container, ok
		}
	}
	return nil, false
}

// Requires startWriting.
func (r *containerStore) ClearFlag(id string, flag string) error {
	container, ok := r.lookup(id)
	if !ok {
		return ErrContainerUnknown
	}
	delete(container.Flags, flag)
	return r.saveFor(container)
}

// Requires startWriting.
func (r *containerStore) SetFlag(id string, flag string, value interface{}) error {
	container, ok := r.lookup(id)
	if !ok {
		return ErrContainerUnknown
	}
	if container.Flags == nil {
		container.Flags = make(map[string]interface{})
	}
	container.Flags[flag] = value
	return r.saveFor(container)
}

// Requires startWriting.
func (r *containerStore) create(id string, names []string, image, layer string, options *ContainerOptions) (container *Container, err error) {
	if options == nil {
		options = &ContainerOptions{}
	}
	if id == "" {
		id = stringid.GenerateRandomID()
		_, idInUse := r.byid[id]
		for idInUse {
			id = stringid.GenerateRandomID()
			_, idInUse = r.byid[id]
		}
	}
	if _, idInUse := r.byid[id]; idInUse {
		return nil, ErrDuplicateID
	}
	names = dedupeStrings(names)
	for _, name := range names {
		if _, nameInUse := r.byname[name]; nameInUse {
			return nil, fmt.Errorf("the container name %q is already in use by %s. You have to remove that container to be able to reuse that name: %w", name, r.byname[name].ID, ErrDuplicateName)
		}
	}
	if err := hasOverlappingRanges(options.UIDMap); err != nil {
		return nil, err
	}
	if err := hasOverlappingRanges(options.GIDMap); err != nil {
		return nil, err
	}
	container = &Container{
		ID:             id,
		Names:          names,
		ImageID:        image,
		LayerID:        layer,
		Metadata:       options.Metadata,
		BigDataNames:   []string{},
		BigDataSizes:   make(map[string]int64),
		BigDataDigests: make(map[string]digest.Digest),
		Created:        time.Now().UTC(),
		Flags:          copyStringInterfaceMap(options.Flags),
		UIDMap:         copyIDMap(options.UIDMap),
		GIDMap:         copyIDMap(options.GIDMap),
		volatileStore:  options.Volatile,
	}
	if options.MountOpts != nil {
		container.Flags[mountOptsFlag] = slices.Clone(options.MountOpts)
	}
	if options.Volatile {
		container.Flags[volatileFlag] = true
	}
	r.containers = append(r.containers, container)
	// This can only fail on duplicate IDs, which shouldn’t happen — and in
	// that case the index is already in the desired state anyway.
	// Implementing recovery from an unlikely and unimportant failure here
	// would be too risky.
	_ = r.idindex.Add(id)
	r.byid[id] = container
	r.bylayer[layer] = container
	for _, name := range names {
		r.byname[name] = container
	}
	defer func() {
		if err != nil {
			// now that the in-memory structures know about the new
			// record, we can use regular Delete() to clean up if
			// anything breaks from here on out
			if e := r.Delete(id); e != nil {
				logrus.Debugf("while cleaning up partially-created container %q we failed to create: %v", id, e)
			}
		}
	}()
	err = r.saveFor(container)
	if err != nil {
		return nil, err
	}
	for _, item := range options.BigData {
		if err = r.SetBigData(id, item.Key, item.Data); err != nil {
			return nil, err
		}
	}
	container = copyContainer(container)
	return container, err
}

// Requires startReading or startWriting.
func (r *containerStore) Metadata(id string) (string, error) {
	if container, ok := r.lookup(id); ok {
		return container.Metadata, nil
	}
	return "", ErrContainerUnknown
}

// Requires startWriting.
func (r *containerStore) SetMetadata(id, metadata string) error {
	if container, ok := r.lookup(id); ok {
		container.Metadata = metadata
		return r.saveFor(container)
	}
	return ErrContainerUnknown
}

// The caller must hold r.inProcessLock for writing.
func (r *containerStore) removeName(container *Container, name string) {
	container.Names = stringSliceWithoutValue(container.Names, name)
}

// Requires startWriting.
func (r *containerStore) updateNames(id string, names []string, op updateNameOperation) error {
	container, ok := r.lookup(id)
	if !ok {
		return ErrContainerUnknown
	}
	oldNames := container.Names
	names, err := applyNameOperation(oldNames, names, op)
	if err != nil {
		return err
	}
	for _, name := range oldNames {
		delete(r.byname, name)
	}
	for _, name := range names {
		if otherContainer, ok := r.byname[name]; ok {
			r.removeName(otherContainer, name)
		}
		r.byname[name] = container
	}
	container.Names = names
	return r.saveFor(container)
}

// Requires startWriting.
func (r *containerStore) Delete(id string) error {
	container, ok := r.lookup(id)
	if !ok {
		return ErrContainerUnknown
	}
	id = container.ID
	toDeleteIndex := -1
	for i, candidate := range r.containers {
		if candidate.ID == id {
			toDeleteIndex = i
			break
		}
	}
	delete(r.byid, id)
	// This can only fail if the ID is already missing, which shouldn’t happen — and in that case the index is already in the desired state anyway.
	// The store’s Delete method is used on various paths to recover from failures, so this should be robust against partially missing data.
	_ = r.idindex.Delete(id)
	delete(r.bylayer, container.LayerID)
	for _, name := range container.Names {
		delete(r.byname, name)
	}
	if toDeleteIndex != -1 {
		// delete the container at toDeleteIndex
		if toDeleteIndex == len(r.containers)-1 {
			r.containers = r.containers[:len(r.containers)-1]
		} else {
			r.containers = append(r.containers[:toDeleteIndex], r.containers[toDeleteIndex+1:]...)
		}
	}
	if err := r.saveFor(container); err != nil {
		return err
	}
	if err := os.RemoveAll(r.datadir(id)); err != nil {
		return err
	}
	return nil
}

// Requires startReading or startWriting.
func (r *containerStore) Get(id string) (*Container, error) {
	if container, ok := r.lookup(id); ok {
		return copyContainer(container), nil
	}
	return nil, ErrContainerUnknown
}

// Requires startReading or startWriting.
func (r *containerStore) Lookup(name string) (id string, err error) {
	if container, ok := r.lookup(name); ok {
		return container.ID, nil
	}
	return "", ErrContainerUnknown
}

// Requires startReading or startWriting.
func (r *containerStore) Exists(id string) bool {
	_, ok := r.lookup(id)
	return ok
}

// Requires startReading or startWriting.
func (r *containerStore) BigData(id, key string) ([]byte, error) {
	if key == "" {
		return nil, fmt.Errorf("can't retrieve container big data value for empty name: %w", ErrInvalidBigDataName)
	}
	c, ok := r.lookup(id)
	if !ok {
		return nil, ErrContainerUnknown
	}
	return os.ReadFile(r.datapath(c.ID, key))
}

// Requires startWriting. Yes, really, WRITING (see SetBigData).
func (r *containerStore) BigDataSize(id, key string) (int64, error) {
	if key == "" {
		return -1, fmt.Errorf("can't retrieve size of container big data with empty name: %w", ErrInvalidBigDataName)
	}
	c, ok := r.lookup(id)
	if !ok {
		return -1, ErrContainerUnknown
	}
	if size, ok := c.BigDataSizes[key]; ok { // This is valid, and returns ok == false, for BigDataSizes == nil.
		return size, nil
	}
	if data, err := r.BigData(id, key); err == nil && data != nil {
		if err = r.SetBigData(id, key, data); err == nil {
			c, ok := r.lookup(id)
			if !ok {
				return -1, ErrContainerUnknown
			}
			if size, ok := c.BigDataSizes[key]; ok {
				return size, nil
			}
		} else {
			return -1, err
		}
	}
	return -1, ErrSizeUnknown
}

// Requires startWriting. Yes, really, WRITING (see SetBigData).
func (r *containerStore) BigDataDigest(id, key string) (digest.Digest, error) {
	if key == "" {
		return "", fmt.Errorf("can't retrieve digest of container big data value with empty name: %w", ErrInvalidBigDataName)
	}
	c, ok := r.lookup(id)
	if !ok {
		return "", ErrContainerUnknown
	}
	if d, ok := c.BigDataDigests[key]; ok { // This is valid, and returns ok == false, for BigDataSizes == nil.
		return d, nil
	}
	if data, err := r.BigData(id, key); err == nil && data != nil {
		if err = r.SetBigData(id, key, data); err == nil {
			c, ok := r.lookup(id)
			if !ok {
				return "", ErrContainerUnknown
			}
			if d, ok := c.BigDataDigests[key]; ok {
				return d, nil
			}
		} else {
			return "", err
		}
	}
	return "", ErrDigestUnknown
}

// Requires startReading or startWriting.
func (r *containerStore) BigDataNames(id string) ([]string, error) {
	c, ok := r.lookup(id)
	if !ok {
		return nil, ErrContainerUnknown
	}
	return copyStringSlice(c.BigDataNames), nil
}

// Requires startWriting.
func (r *containerStore) SetBigData(id, key string, data []byte) error {
	if key == "" {
		return fmt.Errorf("can't set empty name for container big data item: %w", ErrInvalidBigDataName)
	}
	c, ok := r.lookup(id)
	if !ok {
		return ErrContainerUnknown
	}
	if err := os.MkdirAll(r.datadir(c.ID), 0o700); err != nil {
		return err
	}
	err := ioutils.AtomicWriteFile(r.datapath(c.ID, key), data, 0o600)
	if err == nil {
		save := false
		if c.BigDataSizes == nil {
			c.BigDataSizes = make(map[string]int64)
		}
		oldSize, sizeOk := c.BigDataSizes[key]
		c.BigDataSizes[key] = int64(len(data))
		if c.BigDataDigests == nil {
			c.BigDataDigests = make(map[string]digest.Digest)
		}
		oldDigest, digestOk := c.BigDataDigests[key]
		newDigest := digest.Canonical.FromBytes(data)
		c.BigDataDigests[key] = newDigest
		if !sizeOk || oldSize != c.BigDataSizes[key] || !digestOk || oldDigest != newDigest {
			save = true
		}
		addName := true
		for _, name := range c.BigDataNames {
			if name == key {
				addName = false
				break
			}
		}
		if addName {
			c.BigDataNames = append(c.BigDataNames, key)
			save = true
		}
		if save {
			err = r.saveFor(c)
		}
	}
	return err
}

// Requires startWriting.
func (r *containerStore) Wipe() error {
	ids := make([]string, 0, len(r.byid))
	for id := range r.byid {
		ids = append(ids, id)
	}
	for _, id := range ids {
		if err := r.Delete(id); err != nil {
			return err
		}
	}
	return nil
}
