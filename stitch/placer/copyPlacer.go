package placer

import (
	"os"

	. "github.com/polydawn/go-errcat"

	"go.polydawn.net/go-timeless-api/rio"
	"go.polydawn.net/rio/fs"
	"go.polydawn.net/rio/fs/osfs"
	"go.polydawn.net/rio/fsOp"
)

var _ Placer = CopyPlacer

/*
	Makes files appear in place by plain ol' recursive copy.

	Whether you need a "writable" mode or not is ignored; you're getting one.
	The result filesystem will always be writable; it is not possible to make
	a read-only filesystem with this placer.
*/
func CopyPlacer(srcPath, dstPath fs.AbsolutePath, _ bool) (CleanupFunc, error) {
	// Determine desired type.
	srcStat, err := rootFs.LStat(srcPath.CoerceRelative())
	if err != nil {
		return nil, Errorf(rio.ErrLocalCacheProblem, "error placing with copy placer: %s", err)
	}
	switch srcStat.Type {
	case fs.Type_File: // pass
	case fs.Type_Dir: // pass
	default:
		return nil, Errorf(rio.ErrAssemblyInvalid, "placer: source may only be dir or plain file (%s is %s)", srcPath)
	}

	// Capture the parent dir mtime and defer its repair, because we're about to disrupt it.
	defer fsOp.RepairMtime(rootFs, dstPath.Dir().CoerceRelative())()

	// Remove any files already here -- this is to emulate the same behavior
	//  as would be seen with a mount (things masked just vanish).
	if err := os.RemoveAll(dstPath.String()); err != nil {
		return nil, Errorf(rio.ErrAssemblyInvalid, "error clearing copy placement area: %s", err)
	}

	// If plain file: handle that first and return early.
	//  The non-recursive case is much easier.
	if srcStat.Type == fs.Type_File {
		fmeta, body, err := fsOp.ScanFile(rootFs, srcPath.CoerceRelative())
		if err != nil {
			return nil, Errorf(rio.ErrLocalCacheProblem, "error placing with copy placer: %s", err)
		}
		defer body.Close()
		fmeta.Name = dstPath.CoerceRelative()
		return func() error {
			if err := os.Remove(dstPath.String()); err != nil {
				return Errorf(rio.ErrLocalCacheProblem, "error tearing down copy placement: %s", err)
			}
			return nil
		}, fsOp.PlaceFile(rootFs, *fmeta, body, false)
	}

	// For dirs, do a treewalk and copy.  Mtime repair required following every node.
	srcFs := osfs.New(srcPath)
	dstFs := osfs.New(dstPath)
	preVisit := func(filenode *fs.FilewalkNode) error {
		if filenode.Err != nil {
			return filenode.Err
		}
		fmeta, body, err := fsOp.ScanFile(srcFs, filenode.Info.Name)
		if err != nil {
			return err
		}
		if body != nil {
			defer body.Close()
		}
		return fsOp.PlaceFile(dstFs, *fmeta, body, false)
	}
	postVisit := func(filenode *fs.FilewalkNode) error {
		if filenode.Info.Type == fs.Type_Dir {
			if err := dstFs.SetTimesNano(filenode.Info.Name, filenode.Info.Mtime, fs.DefaultAtime); err != nil {
				return err
			}
		}
		return nil
	}
	if err := fs.Walk(srcFs, preVisit, postVisit); err != nil {
		return nil, err
	}

	// Return a cleanup func that does a recursive delete.
	return func() error {
		if err := os.RemoveAll(dstPath.String()); err != nil {
			return Errorf(rio.ErrLocalCacheProblem, "error tearing down copy placement: %s", err)
		}
		return nil
	}, nil
}