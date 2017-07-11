package metadata

import (
	"context"
	"encoding/binary"
	"io"

	"github.com/boltdb/bolt"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/filters"
	"github.com/containerd/containerd/namespaces"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

type contentStore struct {
	content.Store
	db *bolt.DB
}

// NewContentStore returns a namespaced content store using an existing
// content store interface.
func NewContentStore(db *bolt.DB, cs content.Store) content.Store {
	return &contentStore{
		Store: cs,
		db:    db,
	}
}

func (cs *contentStore) Info(ctx context.Context, dgst digest.Digest) (content.Info, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return content.Info{}, err
	}

	var info content.Info
	if err := view(ctx, cs.db, func(tx *bolt.Tx) error {
		bkt := getBlobBucket(tx, ns, dgst)
		if bkt == nil {
			return errors.Wrapf(errdefs.ErrNotFound, "content digest %v", dgst)
		}

		info.Digest = dgst
		return readInfo(&info, bkt)
	}); err != nil {
		return content.Info{}, err
	}

	return info, nil
}

func (cs *contentStore) Walk(ctx context.Context, fn content.WalkFunc) error {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return err
	}

	// TODO: Batch results to keep from reading all info into memory
	var infos []content.Info
	if err := view(ctx, cs.db, func(tx *bolt.Tx) error {
		bkt := getBlobsBucket(tx, ns)
		if bkt == nil {
			return nil
		}

		return bkt.ForEach(func(k, v []byte) error {
			dgst, err := digest.Parse(string(k))
			if err != nil {
				return nil
			}
			info := content.Info{
				Digest: dgst,
			}
			if err := readInfo(&info, bkt.Bucket(k)); err != nil {
				return err
			}
			infos = append(infos, info)
			return nil
		})
	}); err != nil {
		return err
	}

	for _, info := range infos {
		if err := fn(info); err != nil {
			return err
		}
	}

	return nil
}

func (cs *contentStore) Delete(ctx context.Context, dgst digest.Digest) error {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return err
	}

	return update(ctx, cs.db, func(tx *bolt.Tx) error {
		bkt := getBlobBucket(tx, ns, dgst)
		if bkt == nil {
			return errors.Wrapf(errdefs.ErrNotFound, "content digest %v", dgst)
		}

		// Just remove local reference, garbage collector is responsible for
		// cleaning up on disk content
		return getBlobsBucket(tx, ns).Delete([]byte(dgst.String()))
	})
}

func (cs *contentStore) ListStatuses(ctx context.Context, fs ...string) ([]content.Status, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}

	filter, err := filters.ParseAll(fs...)
	if err != nil {
		return nil, err
	}

	brefs := map[string]string{}
	if err := view(ctx, cs.db, func(tx *bolt.Tx) error {
		bkt := getIngestBucket(tx, ns)
		if bkt == nil {
			return nil
		}

		return bkt.ForEach(func(k, v []byte) error {
			// TODO(dmcgowan): match name and potentially labels here
			brefs[string(k)] = string(v)
			return nil
		})
	}); err != nil {
		return nil, err
	}

	statuses := make([]content.Status, 0, len(brefs))
	for k, bref := range brefs {
		status, err := cs.Store.Status(ctx, bref)
		if err != nil {
			if errdefs.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		status.Ref = k

		if filter.Match(adaptContentStatus(status)) {
			statuses = append(statuses, status)
		}
	}

	return statuses, nil

}

func getRef(tx *bolt.Tx, ns, ref string) string {
	bkt := getIngestBucket(tx, ns)
	if bkt == nil {
		return ""
	}
	v := bkt.Get([]byte(ref))
	if len(v) == 0 {
		return ""
	}
	return string(v)
}

func (cs *contentStore) Status(ctx context.Context, ref string) (content.Status, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return content.Status{}, err
	}

	var bref string
	if err := view(ctx, cs.db, func(tx *bolt.Tx) error {
		bref = getRef(tx, ns, ref)
		if bref == "" {
			return errors.Wrapf(errdefs.ErrNotFound, "reference %v", ref)
		}

		return nil
	}); err != nil {
		return content.Status{}, err
	}

	return cs.Store.Status(ctx, bref)
}

func (cs *contentStore) Abort(ctx context.Context, ref string) error {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return err
	}

	return update(ctx, cs.db, func(tx *bolt.Tx) error {
		bkt := getIngestBucket(tx, ns)
		if bkt == nil {
			return errors.Wrapf(errdefs.ErrNotFound, "reference %v", ref)
		}
		bref := string(bkt.Get([]byte(ref)))
		if bref == "" {
			return errors.Wrapf(errdefs.ErrNotFound, "reference %v", ref)
		}
		if err := bkt.Delete([]byte(ref)); err != nil {
			return err
		}

		return cs.Store.Abort(ctx, bref)
	})

}

func (cs *contentStore) Writer(ctx context.Context, ref string, size int64, expected digest.Digest) (content.Writer, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}

	var w content.Writer
	if err := update(ctx, cs.db, func(tx *bolt.Tx) error {
		if expected != "" {
			cbkt := getBlobBucket(tx, ns, expected)
			if cbkt != nil {
				return errors.Wrapf(errdefs.ErrAlreadyExists, "content %v", expected)
			}
		}

		bkt, err := createIngestBucket(tx, ns)
		if err != nil {
			return err
		}

		if len(bkt.Get([]byte(ref))) > 0 {
			return errors.Wrapf(errdefs.ErrUnavailable, "ref %v is currently in use", ref)
		}

		sid, err := bkt.NextSequence()
		if err != nil {
			return err
		}

		bref := createKey(sid, ns, ref)
		if err := bkt.Put([]byte(ref), []byte(bref)); err != nil {
			return err
		}

		// Do not use the passed in expected value here since it was
		// already checked against the user metadata. If the content
		// store has the content, it must still be written before
		// linked into the given namespace. It is possible in the future
		// to allow content which exists in content store but not
		// namespace to be linked here and returned an exist error, but
		// this would require more configuration to make secure.
		w, err = cs.Store.Writer(ctx, bref, size, "")
		return err
	}); err != nil {
		return nil, err
	}

	// TODO: keep the expected in the writer to use on commit
	// when no expected is provided there.
	return &namespacedWriter{
		Writer:    w,
		ref:       ref,
		namespace: ns,
		db:        cs.db,
	}, nil
}

type namespacedWriter struct {
	content.Writer
	ref       string
	namespace string
	db        *bolt.DB
}

func (nw *namespacedWriter) Commit(size int64, expected digest.Digest) error {
	return nw.db.Update(func(tx *bolt.Tx) error {
		bkt := getIngestBucket(tx, nw.namespace)
		if bkt != nil {
			if err := bkt.Delete([]byte(nw.ref)); err != nil {
				return err
			}
		}
		return nw.commit(tx, size, expected)
	})
}

func (nw *namespacedWriter) commit(tx *bolt.Tx, size int64, expected digest.Digest) error {
	status, err := nw.Writer.Status()
	if err != nil {
		return err
	}
	if size != 0 && size != status.Offset {
		return errors.Errorf("%q failed size validation: %v != %v", nw.ref, status.Offset, size)
	}
	size = status.Offset

	actual := nw.Writer.Digest()

	if err := nw.Writer.Commit(size, expected); err != nil {
		if !errdefs.IsAlreadyExists(err) {
			return err
		}
		if getBlobBucket(tx, nw.namespace, actual) != nil {
			return errors.Wrapf(errdefs.ErrAlreadyExists, "content %v", actual)
		}
	}

	bkt, err := createBlobBucket(tx, nw.namespace, actual)
	if err != nil {
		return err
	}

	sizeEncoded, err := encodeSize(size)
	if err != nil {
		return err
	}

	timeEncoded, err := status.UpdatedAt.MarshalBinary()
	if err != nil {
		return err
	}

	for _, v := range [][2][]byte{
		{bucketKeyCreatedAt, timeEncoded},
		{bucketKeySize, sizeEncoded},
	} {
		if err := bkt.Put(v[0], v[1]); err != nil {
			return err
		}
	}

	return nil
}

func (cs *contentStore) Reader(ctx context.Context, dgst digest.Digest) (io.ReadCloser, error) {
	if err := cs.checkAccess(ctx, dgst); err != nil {
		return nil, err
	}
	return cs.Store.Reader(ctx, dgst)
}

func (cs *contentStore) ReaderAt(ctx context.Context, dgst digest.Digest) (io.ReaderAt, error) {
	if err := cs.checkAccess(ctx, dgst); err != nil {
		return nil, err
	}
	return cs.Store.ReaderAt(ctx, dgst)
}

func (cs *contentStore) checkAccess(ctx context.Context, dgst digest.Digest) error {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return err
	}

	return view(ctx, cs.db, func(tx *bolt.Tx) error {
		bkt := getBlobBucket(tx, ns, dgst)
		if bkt == nil {
			return errors.Wrapf(errdefs.ErrNotFound, "content digest %v", dgst)
		}
		return nil
	})
}

func readInfo(info *content.Info, bkt *bolt.Bucket) error {
	return bkt.ForEach(func(k, v []byte) error {
		switch string(k) {
		case string(bucketKeyCreatedAt):
			if err := info.CommittedAt.UnmarshalBinary(v); err != nil {
				return err
			}
		case string(bucketKeySize):
			info.Size, _ = binary.Varint(v)
		}
		// TODO: Read labels
		return nil
	})
}
