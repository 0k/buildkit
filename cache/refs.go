package cache

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/leases"
	"github.com/containerd/containerd/mount"
	"github.com/docker/docker/pkg/idtools"
	"github.com/moby/buildkit/cache/metadata"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/util/flightcontrol"
	"github.com/opencontainers/go-digest"
	imagespaceidentity "github.com/opencontainers/image-spec/identity"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// Ref is a reference to cacheable objects.
type Ref interface {
	Mountable
	ID() string
	Release(context.Context) error
	Size(ctx context.Context) (int64, error)
	Metadata() *metadata.StorageItem
	IdentityMapping() *idtools.IdentityMapping
}

type ImmutableRef interface {
	Ref
	Parent() ImmutableRef
	Finalize(ctx context.Context, commit bool) error // Make sure reference is flushed to driver
	Clone() ImmutableRef

	Info() RefInfo
	SetBlob(ctx context.Context, diffID, blob digest.Digest) error
	Extract(ctx context.Context) error // +progress
}

type RefInfo struct {
	SnapshotID  string
	ChainID     digest.Digest
	BlobChainID digest.Digest
	DiffID      digest.Digest
	Blob        digest.Digest
	Extracted   bool
}

type MutableRef interface {
	Ref
	Commit(context.Context) (ImmutableRef, error)
}

type Mountable interface {
	Mount(ctx context.Context, readonly bool) (snapshot.Mountable, error)
}

type ref interface {
	updateLastUsed() bool
}

type cacheRecord struct {
	cm *cacheManager
	mu *sync.Mutex // the mutex is shared by records sharing data

	mutable bool
	refs    map[ref]struct{}
	parent  *immutableRef
	md      *metadata.StorageItem

	// dead means record is marked as deleted
	dead bool

	view      string
	viewMount snapshot.Mountable

	sizeG flightcontrol.Group

	// these are filled if multiple refs point to same data
	equalMutable   *mutableRef
	equalImmutable *immutableRef
}

// hold ref lock before calling
func (cr *cacheRecord) ref(triggerLastUsed bool) *immutableRef {
	ref := &immutableRef{cacheRecord: cr, triggerLastUsed: triggerLastUsed}
	cr.refs[ref] = struct{}{}
	return ref
}

// hold ref lock before calling
func (cr *cacheRecord) mref(triggerLastUsed bool) *mutableRef {
	ref := &mutableRef{cacheRecord: cr, triggerLastUsed: triggerLastUsed}
	cr.refs[ref] = struct{}{}
	return ref
}

// hold ref lock before calling
func (cr *cacheRecord) isDead() bool {
	return cr.dead || (cr.equalImmutable != nil && cr.equalImmutable.dead) || (cr.equalMutable != nil && cr.equalMutable.dead)
}

func (cr *cacheRecord) IdentityMapping() *idtools.IdentityMapping {
	return cr.cm.IdentityMapping()
}

func (cr *cacheRecord) Size(ctx context.Context) (int64, error) {
	// this expects that usage() is implemented lazily
	s, err := cr.sizeG.Do(ctx, cr.ID(), func(ctx context.Context) (interface{}, error) {
		cr.mu.Lock()
		s := getSize(cr.md)
		if s != sizeUnknown {
			cr.mu.Unlock()
			return s, nil
		}
		driverID := getSnapshotID(cr.md)
		if cr.equalMutable != nil {
			driverID = getSnapshotID(cr.equalMutable.md)
		}
		cr.mu.Unlock()
		usage, err := cr.cm.ManagerOpt.Snapshotter.Usage(ctx, driverID)
		if err != nil {
			cr.mu.Lock()
			isDead := cr.isDead()
			cr.mu.Unlock()
			if isDead {
				return int64(0), nil
			}
			return s, errors.Wrapf(err, "failed to get usage for %s", cr.ID())
		}
		cr.mu.Lock()
		setSize(cr.md, usage.Size)
		if err := cr.md.Commit(); err != nil {
			cr.mu.Unlock()
			return s, err
		}
		cr.mu.Unlock()
		return usage.Size, nil
	})
	return s.(int64), err
}

func (cr *cacheRecord) Parent() ImmutableRef {
	if p := cr.parentRef(true); p != nil { // avoid returning typed nil pointer
		return p
	}
	return nil
}

func (cr *cacheRecord) parentRef(hidden bool) *immutableRef {
	p := cr.parent
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ref(hidden)
}

func (cr *cacheRecord) Mount(ctx context.Context, readonly bool) (snapshot.Mountable, error) {
	cr.mu.Lock()
	defer cr.mu.Unlock()

	if cr.mutable {
		m, err := cr.cm.Snapshotter.Mounts(ctx, getSnapshotID(cr.md))
		if err != nil {
			return nil, errors.Wrapf(err, "failed to mount %s", cr.ID())
		}
		if readonly {
			m = setReadonly(m)
		}
		return m, nil
	}

	if cr.equalMutable != nil && readonly {
		m, err := cr.cm.Snapshotter.Mounts(ctx, getSnapshotID(cr.equalMutable.md))
		if err != nil {
			return nil, errors.Wrapf(err, "failed to mount %s", cr.equalMutable.ID())
		}
		return setReadonly(m), nil
	}

	if err := cr.finalize(ctx, true); err != nil {
		return nil, err
	}
	if cr.viewMount == nil { // TODO: handle this better
		cr.view = identity.NewID()
		m, err := cr.cm.Snapshotter.View(ctx, cr.view, getSnapshotID(cr.md))
		if err != nil {
			cr.view = ""
			return nil, errors.Wrapf(err, "failed to mount %s", cr.ID())
		}
		cr.viewMount = m
	}
	return cr.viewMount, nil
}

// call when holding the manager lock
func (cr *cacheRecord) remove(ctx context.Context, removeSnapshot bool) error {
	delete(cr.cm.records, cr.ID())
	if cr.parent != nil {
		if err := cr.parent.release(ctx); err != nil {
			return err
		}
	}
	if removeSnapshot {
		if err := cr.cm.LeaseManager.Delete(ctx, leases.Lease{ID: cr.ID()}); err != nil { // TODO: handle cancellation, async
			return errors.Wrapf(err, "failed to remove %s", cr.ID())
		}
	}
	if err := cr.cm.md.Clear(cr.ID()); err != nil {
		return err
	}
	return nil
}

func (cr *cacheRecord) ID() string {
	return cr.md.ID()
}

type immutableRef struct {
	*cacheRecord
	triggerLastUsed bool
}

type mutableRef struct {
	*cacheRecord
	triggerLastUsed bool
}

func (sr *immutableRef) Clone() ImmutableRef {
	sr.mu.Lock()
	ref := sr.ref(false)
	sr.mu.Unlock()
	return ref
}

func (sr *immutableRef) Extract(ctx context.Context) error {
	return errors.Errorf("extract not implemented")
}

func (sr *immutableRef) Info() RefInfo {
	return RefInfo{
		ChainID:     digest.Digest(getChainID(sr.md)),
		DiffID:      digest.Digest(getDiffID(sr.md)),
		Blob:        digest.Digest(getBlob(sr.md)),
		BlobChainID: digest.Digest(getBlobChainID(sr.md)),
		SnapshotID:  getSnapshotID(sr.md),
		Extracted:   !getBlobOnly(sr.md),
	}
}

// SetBlob associates a blob with the cache record.
// A lease must be held for the blob when calling this function
// Caller should call Info() for knowing what current values are actually set
func (sr *immutableRef) SetBlob(ctx context.Context, diffID, blob digest.Digest) error {
	if _, err := sr.cm.ContentStore.Info(ctx, blob); err != nil {
		return err
	}

	sr.mu.Lock()
	defer sr.mu.Unlock()

	if getChainID(sr.md) != "" {
		return nil
	}

	if err := sr.finalize(ctx, true); err != nil {
		return err
	}

	p := sr.parent
	var parentChainID digest.Digest
	var parentBlobChainID digest.Digest
	if p != nil {
		pInfo := p.Info()
		if pInfo.ChainID == "" || pInfo.BlobChainID == "" {
			return errors.Errorf("failed to set blob for reference with non-addressable parent")
		}
		parentChainID = pInfo.ChainID
		parentBlobChainID = pInfo.BlobChainID
	}

	if err := sr.cm.LeaseManager.AddResource(ctx, leases.Lease{ID: sr.ID()}, leases.Resource{
		ID:   blob.String(),
		Type: "content",
	}); err != nil {
		return err
	}

	queueDiffID(sr.md, diffID.String())
	queueBlob(sr.md, blob.String())
	chainID := diffID
	blobChainID := imagespaceidentity.ChainID([]digest.Digest{blob, diffID})
	if parentChainID != "" {
		chainID = imagespaceidentity.ChainID([]digest.Digest{parentChainID, chainID})
		blobChainID = imagespaceidentity.ChainID([]digest.Digest{parentBlobChainID, blobChainID})
	}
	queueChainID(sr.md, chainID.String())
	queueBlobChainID(sr.md, blobChainID.String())
	if err := sr.md.Commit(); err != nil {
		return err
	}
	return nil
}

func (sr *immutableRef) Release(ctx context.Context) error {
	sr.cm.mu.Lock()
	defer sr.cm.mu.Unlock()

	sr.mu.Lock()
	defer sr.mu.Unlock()

	return sr.release(ctx)
}

func (sr *immutableRef) updateLastUsed() bool {
	return sr.triggerLastUsed
}

func (sr *immutableRef) updateLastUsedNow() bool {
	if !sr.triggerLastUsed {
		return false
	}
	for r := range sr.refs {
		if r.updateLastUsed() {
			return false
		}
	}
	return true
}

func (sr *immutableRef) release(ctx context.Context) error {
	delete(sr.refs, sr)

	if sr.updateLastUsedNow() {
		updateLastUsed(sr.md)
		if sr.equalMutable != nil {
			sr.equalMutable.triggerLastUsed = true
		}
	}

	if len(sr.refs) == 0 {
		if sr.viewMount != nil { // TODO: release viewMount earlier if possible
			if err := sr.cm.Snapshotter.Remove(ctx, sr.view); err != nil {
				return errors.Wrapf(err, "failed to remove view %s", sr.view)
			}
			sr.view = ""
			sr.viewMount = nil
		}

		if sr.equalMutable != nil {
			sr.equalMutable.release(ctx)
		}
		// go sr.cm.GC()
	}

	return nil
}

func (sr *immutableRef) Finalize(ctx context.Context, b bool) error {
	sr.mu.Lock()
	defer sr.mu.Unlock()

	return sr.finalize(ctx, b)
}

func (cr *cacheRecord) Metadata() *metadata.StorageItem {
	return cr.md
}

func (cr *cacheRecord) finalize(ctx context.Context, commit bool) error {
	mutable := cr.equalMutable
	if mutable == nil {
		return nil
	}
	if !commit {
		if HasCachePolicyRetain(mutable) {
			CachePolicyRetain(mutable)
			return mutable.Metadata().Commit()
		}
		return nil
	}
	err := cr.cm.Snapshotter.Commit(ctx, cr.ID(), mutable.ID())
	if err != nil {
		return errors.Wrapf(err, "failed to commit %s", mutable.ID())
	}
	mutable.dead = true
	go func() {
		cr.cm.mu.Lock()
		defer cr.cm.mu.Unlock()
		if err := mutable.remove(context.TODO(), true); err != nil {
			logrus.Error(err)
		}
	}()

	// TODO: rollback
	l, err := cr.cm.ManagerOpt.LeaseManager.Create(ctx, func(l *leases.Lease) error {
		l.ID = cr.ID()
		l.Labels = map[string]string{
			"containerd.io/gc.flat": time.Now().UTC().Format(time.RFC3339Nano),
		}
		return nil
	})
	if err != nil {
		return errors.Wrap(err, "failed to create lease")
	}

	if err := cr.cm.ManagerOpt.LeaseManager.AddResource(ctx, l, leases.Resource{
		ID:   cr.ID(),
		Type: "snapshots/" + cr.cm.ManagerOpt.Snapshotter.Name(),
	}); err != nil {
		return errors.Wrapf(err, "failed to add snapshot %s to lease", cr.ID())
	}

	cr.equalMutable = nil
	clearEqualMutable(cr.md)
	return cr.md.Commit()
}

func (sr *mutableRef) updateLastUsed() bool {
	return sr.triggerLastUsed
}

func (sr *mutableRef) commit(ctx context.Context) (*immutableRef, error) {
	if !sr.mutable || len(sr.refs) == 0 {
		return nil, errors.Wrapf(errInvalid, "invalid mutable ref %p", sr)
	}

	id := identity.NewID()
	md, _ := sr.cm.md.Get(id)
	rec := &cacheRecord{
		mu:           sr.mu,
		cm:           sr.cm,
		parent:       sr.parentRef(false),
		equalMutable: sr,
		refs:         make(map[ref]struct{}),
		md:           md,
	}

	if descr := GetDescription(sr.md); descr != "" {
		if err := queueDescription(md, descr); err != nil {
			return nil, err
		}
	}

	parentID := ""
	if rec.parent != nil {
		parentID = rec.parent.ID()
	}
	if err := initializeMetadata(rec, parentID); err != nil {
		return nil, err
	}

	sr.cm.records[id] = rec

	if err := sr.md.Commit(); err != nil {
		return nil, err
	}

	queueCommitted(md)
	setSize(md, sizeUnknown)
	setEqualMutable(md, sr.ID())
	if err := md.Commit(); err != nil {
		return nil, err
	}

	ref := rec.ref(true)
	sr.equalImmutable = ref
	return ref, nil
}

func (sr *mutableRef) updatesLastUsed() bool {
	return sr.triggerLastUsed
}

func (sr *mutableRef) Commit(ctx context.Context) (ImmutableRef, error) {
	sr.cm.mu.Lock()
	defer sr.cm.mu.Unlock()

	sr.mu.Lock()
	defer sr.mu.Unlock()

	return sr.commit(ctx)
}

func (sr *mutableRef) Release(ctx context.Context) error {
	sr.cm.mu.Lock()
	defer sr.cm.mu.Unlock()

	sr.mu.Lock()
	defer sr.mu.Unlock()

	return sr.release(ctx)
}

func (sr *mutableRef) release(ctx context.Context) error {
	delete(sr.refs, sr)
	if getCachePolicy(sr.md) != cachePolicyRetain {
		if sr.equalImmutable != nil {
			if getCachePolicy(sr.equalImmutable.md) == cachePolicyRetain {
				if sr.updateLastUsed() {
					updateLastUsed(sr.md)
					sr.triggerLastUsed = false
				}
				return nil
			}
			if err := sr.equalImmutable.remove(ctx, false); err != nil {
				return err
			}
		}
		if sr.parent != nil {
			if err := sr.parent.release(ctx); err != nil {
				return err
			}
		}
		return sr.remove(ctx, true)
	} else {
		if sr.updateLastUsed() {
			updateLastUsed(sr.md)
			sr.triggerLastUsed = false
		}
	}
	return nil
}

func setReadonly(mounts snapshot.Mountable) snapshot.Mountable {
	return &readOnlyMounter{mounts}
}

type readOnlyMounter struct {
	snapshot.Mountable
}

func (m *readOnlyMounter) Mount() ([]mount.Mount, func() error, error) {
	mounts, release, err := m.Mountable.Mount()
	if err != nil {
		return nil, nil, err
	}
	for i, m := range mounts {
		if m.Type == "overlay" {
			mounts[i].Options = readonlyOverlay(m.Options)
			continue
		}
		opts := make([]string, 0, len(m.Options))
		for _, opt := range m.Options {
			if opt != "rw" {
				opts = append(opts, opt)
			}
		}
		opts = append(opts, "ro")
		mounts[i].Options = opts
	}
	return mounts, release, nil
}

func readonlyOverlay(opt []string) []string {
	out := make([]string, 0, len(opt))
	upper := ""
	for _, o := range opt {
		if strings.HasPrefix(o, "upperdir=") {
			upper = strings.TrimPrefix(o, "upperdir=")
		} else if !strings.HasPrefix(o, "workdir=") {
			out = append(out, o)
		}
	}
	if upper != "" {
		for i, o := range out {
			if strings.HasPrefix(o, "lowerdir=") {
				out[i] = "lowerdir=" + upper + ":" + strings.TrimPrefix(o, "lowerdir=")
			}
		}
	}
	return out
}
