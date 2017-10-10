package tartrans

import (
	"archive/tar"
	"context"
	"crypto/sha512"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/polydawn/refmt/misc"

	. "github.com/polydawn/go-errcat"
	"go.polydawn.net/go-timeless-api"
	"go.polydawn.net/go-timeless-api/rio"
	"go.polydawn.net/go-timeless-api/util"
	"go.polydawn.net/rio/config"
	"go.polydawn.net/rio/fs"
	"go.polydawn.net/rio/fs/osfs"
	"go.polydawn.net/rio/fsOp"
	"go.polydawn.net/rio/lib/treewalk"
	"go.polydawn.net/rio/transmat/mixins/cache"
	"go.polydawn.net/rio/transmat/mixins/filters"
	"go.polydawn.net/rio/transmat/mixins/fshash"
	"go.polydawn.net/rio/transmat/mixins/log"
	"go.polydawn.net/rio/transmat/util"
	"go.polydawn.net/rio/warehouse"
	"go.polydawn.net/rio/warehouse/impl/kvfs"
	"go.polydawn.net/rio/warehouse/impl/kvhttp"
)

var (
	_ rio.UnpackFunc = Unpack
)

func Unpack(
	ctx context.Context, // Long-running call.  Cancellable.
	wareID api.WareID, // What wareID to fetch for unpacking.
	path string, // Where to unpack the fileset (absolute path).
	filt api.FilesetFilters, // Optionally: filters we should apply while unpacking.
	placementMode rio.PlacementMode, // Optionally: a placement mode (default is "copy").
	warehouses []api.WarehouseAddr, // Warehouses we can try to fetch from.
	mon rio.Monitor, // Optionally: callbacks for progress monitoring.
) (_ api.WareID, err error) {
	if mon.Chan != nil {
		defer close(mon.Chan)
	}
	defer RequireErrorHasCategory(&err, rio.ErrorCategory(""))

	// Sanitize arguments.
	if wareID.Type != PackType {
		return api.WareID{}, Errorf(rio.ErrUsage, "this transmat implementation only supports packtype %q (not %q)", PackType, wareID.Type)
	}
	if placementMode == "" {
		placementMode = rio.Placement_Copy
	}
	// Wrap the direct unpack func with cache behavior; call that.
	return cache.Lrn2Cache(
		osfs.New(config.GetCacheBasePath()),
		unpack,
	)(ctx, wareID, path, filt, placementMode, warehouses, mon)
}

func unpack(
	ctx context.Context,
	wareID api.WareID,
	path string,
	filt api.FilesetFilters,
	placementMode rio.PlacementMode,
	warehouses []api.WarehouseAddr,
	mon rio.Monitor,
) (_ api.WareID, err error) {
	defer RequireErrorHasCategory(&err, rio.ErrorCategory(""))

	// Sanitize arguments.
	path2 := fs.MustAbsolutePath(path)
	filt2, err := apiutil.ProcessFilters(filt, apiutil.FilterPurposeUnpack)
	if err != nil {
		return api.WareID{}, Errorf(rio.ErrUsage, "invalid filter specification: %s", err)
	}

	// Pick a warehouse.
	//  With K/V warehouses, this takes the form of "pick the first one that answers".
	var reader io.ReadCloser
	for _, addr := range warehouses {
		// REVIEW ... Do I really have to parse this again?  is this sanely encapsulated?
		u, err := url.Parse(string(addr))
		if err != nil {
			return api.WareID{}, Errorf(rio.ErrUsage, "failed to parse URI: %s", err)
		}
		var whCtrl warehouse.BlobstoreController
		switch u.Scheme {
		case "file", "ca+file":
			whCtrl, err = kvfs.NewController(addr)
		case "http", "ca+http", "https", "ca+https":
			whCtrl, err = kvhttp.NewController(addr)
		default:
			return api.WareID{}, Errorf(rio.ErrUsage, "tar unpack doesn't support %q scheme (valid options are 'file', 'ca+file', 'http', 'ca+http', 'https', or 'ca+https')", u.Scheme)
		}
		switch Category(err) {
		case nil:
			// pass
		case rio.ErrWarehouseUnavailable:
			log.WarehouseUnavailable(mon, err, addr, wareID, "read")
			continue // okay!  skip to the next one.
		default:
			return api.WareID{}, err
		}
		reader, err = whCtrl.OpenReader(wareID)
		switch Category(err) {
		case nil:
			// pass
		case rio.ErrWareNotFound:
			log.WareNotFound(mon, err, addr, wareID)
			continue // okay!  skip to the next one.
		default:
			return api.WareID{}, err
		}
	}
	if reader == nil { // aka if no warehouses available:
		return api.WareID{}, Errorf(rio.ErrWarehouseUnavailable, "no warehouses were available!")
	}
	defer reader.Close()

	// Construct filesystem wrapper to use for all our ops.
	afs := osfs.New(path2)

	// Extract.
	gotWare, err := unpackTar(ctx, afs, filt2, reader)
	if err != nil {
		return gotWare, err
	}

	// Check for hash mismatch before returning, because that IS an error,
	//  but also return the hash we got either way.
	if gotWare != wareID {
		return gotWare, ErrorDetailed(
			rio.ErrWareHashMismatch,
			fmt.Sprintf("hash mismatch: expected %q, got %q", wareID, gotWare),
			map[string]string{
				"expected": wareID.String(),
				"actual":   gotWare.String(),
			},
		)
	}
	return gotWare, nil
}

func unpackTar(
	ctx context.Context,
	afs fs.FS,
	filt apiutil.FilesetFilters,
	reader io.Reader,
) (_ api.WareID, err error) {
	defer RequireErrorHasCategory(&err, rio.ErrorCategory(""))

	// Wrap input stream with decompression as necessary.
	//  Which kind of decompression to use can be autodetected by magic bytes.
	reader2, err := Decompress(reader)
	if err != nil {
		return api.WareID{}, Errorf(rio.ErrWareCorrupt, "corrupt tar compression: %s", err)
	}

	// Convert the raw byte reader to a tar stream.
	tr := tar.NewReader(reader2)

	// Allocate bucket for keeping each metadata entry and content hash;
	// the full tree hash will be computed from this at the end.
	bucket := &fshash.MemoryBucket{}

	// Iterate over each tar entry, mutating filesystem as we go.
	for {
		fmeta := fs.Metadata{}
		thdr, err := tr.Next()

		// Check for done.
		if err == io.EOF {
			break // sucess!  end of archive.
		}
		if err != nil {
			return api.WareID{}, Errorf(rio.ErrWareCorrupt, "corrupt tar: %s", err)
		}
		if ctx.Err() != nil {
			return api.WareID{}, Errorf(rio.ErrCancelled, "cancelled")
		}

		// Reshuffle metainfo to our default format.
		if err := TarHdrToMetadata(thdr, &fmeta); err != nil {
			return api.WareID{}, err
		}
		if strings.HasPrefix(fmeta.Name.String(), "..") {
			return api.WareID{}, Errorf(rio.ErrWareCorrupt, "corrupt tar: paths that use '../' to leave the base dir are invalid")
		}

		// Apply filters.
		filters.Apply(filt, &fmeta)

		// Infer parents, if necessary.  The tar format allows implicit parent dirs.
		//
		// Note that if any of the implicitly conjured dirs is specified later, unpacking won't notice,
		// but bucket hashing iteration will (correctly) blow up for repeat entries.
		// It may well be possible to construct a tar like that, but it's already well established that
		// tars with repeated filenames are just asking for trouble and shall be rejected without
		// ceremony because they're just a ridiculous idea.
		for parent := fmeta.Name.Dir(); parent != (fs.RelPath{}); parent = parent.Dir() {
			_, err := afs.LStat(parent)
			// if it already exists, move along; if the error is anything interesting, let PlaceFile decide how to deal with it
			if err == nil || !os.IsNotExist(err) {
				continue
			}
			// if we're missing a dir, conjure a node with defaulted values (same as we do for "./")
			conjuredFmeta := fshash.DefaultDirMetadata()
			conjuredFmeta.Name = parent
			if err := fsOp.PlaceFile(afs, conjuredFmeta, nil, false); err != nil {
				return api.WareID{}, Errorf(rio.ErrInoperablePath, "error while unpacking: %s", err)
			}
			bucket.AddRecord(conjuredFmeta, nil)
		}

		// Place the file.
		switch fmeta.Type {
		case fs.Type_File:
			reader := &util.HashingReader{tr, sha512.New384()}
			if err := fsOp.PlaceFile(afs, fmeta, reader, false); err != nil {
				return api.WareID{}, Errorf(rio.ErrInoperablePath, "error while unpacking: %s", err)
			}
			bucket.AddRecord(fmeta, reader.Hasher.Sum(nil))
		default:
			if err := fsOp.PlaceFile(afs, fmeta, nil, false); err != nil {
				return api.WareID{}, Errorf(rio.ErrInoperablePath, "error while unpacking: %s", err)
			}
			bucket.AddRecord(fmeta, nil)
		}
	}

	// Cleanup dir times with a post-order traversal over the bucket.
	//  Files and dirs placed inside dirs cause the parent's mtime to update, so we have to re-pave them.
	if err := treewalk.Walk(bucket.Iterator(), nil, func(node treewalk.Node) error {
		record := node.(fshash.RecordIterator).Record()
		if record.Metadata.Type != fs.Type_Dir {
			return nil
		}
		return afs.SetTimesNano(record.Metadata.Name, record.Metadata.Mtime, fs.DefaultAtime)
	}); err != nil {
		return api.WareID{}, Errorf(rio.ErrInoperablePath, "error while unpacking: %s", err)
	}
	// Bucket processing may have created a root node if missing.  If so, make sure we apply its props (all of them, not just time).
	if err := fsOp.PlaceFile(afs, bucket.Root().Metadata, nil, false); err != nil {
		return api.WareID{}, Errorf(rio.ErrInoperablePath, "error while unpacking: %s", err)
	}

	// Hash the thing!
	hash := fshash.HashBucket(bucket, sha512.New384)

	return api.WareID{"tar", misc.Base58Encode(hash)}, nil
}
