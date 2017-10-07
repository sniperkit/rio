package main

import (
	. "github.com/polydawn/go-errcat"

	"go.polydawn.net/go-timeless-api/rio"
	"go.polydawn.net/rio/transmat/tar"
)

func demuxPackTool(packType string) (rio.PackFunc, error) {
	switch packType {
	case "tar":
		return tartrans.Pack, nil
	default:
		return nil, Errorf(rio.ErrUsage, "unsupported packtype %q", packType)
	}
}

func demuxUnpackTool(packType string) (rio.UnpackFunc, error) {
	switch packType {
	case "tar":
		return tartrans.Unpack, nil
	default:
		return nil, Errorf(rio.ErrUsage, "unsupported packtype %q", packType)
	}
}

func demuxScanTool(packType string) (rio.ScanFunc, error) {
	switch packType {
	case "tar":
		return tartrans.Scan, nil
	default:
		return nil, Errorf(rio.ErrUsage, "unsupported packtype %q", packType)
	}
}