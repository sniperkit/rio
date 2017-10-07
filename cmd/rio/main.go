package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"

	. "github.com/polydawn/go-errcat"
	"github.com/polydawn/refmt"
	"github.com/polydawn/refmt/json"
	"gopkg.in/alecthomas/kingpin.v2"

	"go.polydawn.net/go-timeless-api"
	"go.polydawn.net/go-timeless-api/rio"
)

func main() {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	go CancelOnInterrupt(cancel)
	exitCode := Main(ctx, os.Args, os.Stdin, os.Stdout, os.Stderr)
	os.Exit(exitCode)
}

// Blocks until a sigint is received, then calls cancel.
func CancelOnInterrupt(cancel context.CancelFunc) {
	signalChan := make(chan os.Signal)
	signal.Notify(signalChan, os.Interrupt)
	<-signalChan
	cancel()
	close(signalChan)
}

// Holder type which makes it easier for us to inspect
//  the args parser result in test code before running logic.
type behavior struct {
	parsedArgs interface{}
	action     func() error
}

type format string

const (
	format_Dumb = "dumb"
	format_Json = "json"
)

func Main(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	bhv := Parse(ctx, args, stdin, stdout, stderr)
	err := bhv.action()
	return rio.ExitCodeForError(err)
}

func Parse(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) behavior {
	// CLI boilerplate.
	app := kingpin.New("rio", "Repeatable I/O.")
	app.HelpFlag.Short('h')
	app.UsageWriter(stderr)
	app.ErrorWriter(stderr)
	app.Terminate(func(int) {})

	// Output control helper.
	//  Declared early because we reference it in action thunks;
	//  however its format field may not end up set until much lower in the file.
	oc := &outputController{"", stdout, stderr}

	// Args struct defs and flag declarations.
	bhvs := map[string]*behavior{}
	baseArgs := struct {
		Format string
	}{}
	app.Flag("format", "Output api format").
		Default(format_Dumb).
		EnumVar(&baseArgs.Format,
			format_Dumb, format_Json)
	{
		cmd := app.Command("pack", "Pack a Fileset into a Ware.")
		args := struct {
			PackType            string             // Pack type
			Path                string             // Pack target path, abs or rel
			Filters             api.FilesetFilters // Filters for pack
			TargetWarehouseAddr string             // Warehouse address to push to
		}{}
		cmd.Arg("pack", "Pack type").
			Required().
			StringVar(&args.PackType)
		cmd.Arg("path", "Target path").
			Required().
			StringVar(&args.Path)
		cmd.Flag("target", "Warehouse in which to place the ware").
			StringVar(&args.TargetWarehouseAddr)
		cmd.Flag("uid", "Set UID filter [keep, <int>]").
			StringVar(&args.Filters.Uid)
		cmd.Flag("gid", "Set GID filter [keep, <int>]").
			StringVar(&args.Filters.Gid)
		cmd.Flag("mtime", "Set mtime filter [keep, <@UNIX>, <RFC3339>]. Will be set to a date if not specified.").
			StringVar(&args.Filters.Mtime)
		cmd.Flag("sticky", "Keep setuid, setgid, and sticky bits [keep, zero]").
			Default("keep").
			EnumVar(&args.Filters.Sticky,
				"keep", "zero")
		bhvs[cmd.FullCommand()] = &behavior{&args, func() error {
			packFunc, err := demuxPackTool(args.PackType)
			if err != nil {
				return err
			}
			resultWareID, err := packFunc(
				ctx,
				api.PackType(args.PackType),
				args.Path,
				args.Filters,
				api.WarehouseAddr(args.TargetWarehouseAddr),
				rio.Monitor{},
			)
			if err != nil {
				return err
			}
			oc.EmitResult(resultWareID, nil)
			return nil
		}}
	}
	{
		cmd := app.Command("unpack", "Unpack a Ware into a Fileset on your local filesystem.")
		args := struct {
			WareID               string             // Ware id string "<kind>:<hash>"
			Path                 string             // Unpack target path, may be abs or rel
			Filters              api.FilesetFilters // Filters for unpack
			PlacementMode        string             // Placement mode enum
			SourcesWarehouseAddr []string           // Warehouse address to fetch from
		}{}
		cmd.Arg("path", "Target path").
			Required().
			StringVar(&args.Path)
		cmd.Arg("ware", "Ware ID").
			Required().
			StringVar(&args.WareID)
		cmd.Flag("placer", "Placement mode to use [copy, direct, mount, none]").
			EnumVar(&args.PlacementMode,
				string(rio.Placement_Copy), string(rio.Placement_Direct), string(rio.Placement_Mount), string(rio.Placement_None))
		cmd.Flag("source", "Warehouses from which to fetch the ware").
			StringsVar(&args.SourcesWarehouseAddr)
		cmd.Flag("uid", "Set UID filter [keep, mine, <int>]").
			Default("mine").
			StringVar(&args.Filters.Uid)
		cmd.Flag("gid", "Set GID filter [keep, mine, <int>]").
			Default("mine").
			StringVar(&args.Filters.Gid)
		cmd.Flag("mtime", "Set mtime filter [keep, <@UNIX>, <RFC3339>]").
			Default("keep").
			StringVar(&args.Filters.Mtime)
		cmd.Flag("sticky", "Keep setuid, setgid, and sticky bits [keep, zero]").
			Default("zero").
			EnumVar(&args.Filters.Sticky,
				"keep", "zero")
		bhvs[cmd.FullCommand()] = &behavior{&args, func() error {
			wareID, err := api.ParseWareID(args.WareID)
			if err != nil {
				return err
			}
			unpackFunc, err := demuxUnpackTool(string(wareID.Type))
			if err != nil {
				return err
			}
			resultWareID, err := unpackFunc(
				ctx,
				wareID,
				args.Path,
				args.Filters,
				rio.PlacementMode(args.PlacementMode),
				convertWarehouseSlice(args.SourcesWarehouseAddr),
				rio.Monitor{},
			)
			if err != nil {
				return err
			}
			oc.EmitResult(resultWareID, nil)
			return nil
		}}
	}
	{
		cmd := app.Command("scan", "Scan some existing data stream see if it's a known packed format, and compute its WareID if so.  (Mostly used for importing tars from the interweb.)")
		args := struct {
			PackType            string             // Pack type
			Filters             api.FilesetFilters // Filters for pack
			SourceWarehouseAddr string             // Warehouse address of data to scan
		}{}
		cmd.Arg("pack", "Pack type").
			Required().
			StringVar(&args.PackType)
		cmd.Flag("source", "Address to of the data to scan.").
			StringVar(&args.SourceWarehouseAddr)
		cmd.Flag("uid", "Set UID filter [keep, <int>]").
			StringVar(&args.Filters.Uid)
		cmd.Flag("gid", "Set GID filter [keep, <int>]").
			StringVar(&args.Filters.Gid)
		cmd.Flag("mtime", "Set mtime filter [keep, <@UNIX>, <RFC3339>]. Will be set to a date if not specified.").
			StringVar(&args.Filters.Mtime)
		cmd.Flag("sticky", "Keep setuid, setgid, and sticky bits [keep, zero]").
			Default("keep").
			EnumVar(&args.Filters.Sticky,
				"keep", "zero")
		bhvs[cmd.FullCommand()] = &behavior{&args, func() error {
			scanFunc, err := demuxScanTool(string(args.PackType))
			if err != nil {
				return err
			}
			resultWareID, err := scanFunc(
				ctx,
				api.PackType(args.PackType),
				args.Filters,
				rio.Placement_Direct,
				api.WarehouseAddr(args.SourceWarehouseAddr),
				rio.Monitor{},
			)
			if err != nil {
				return err
			}
			oc.EmitResult(resultWareID, nil)
			return nil
		}}
	}
	// Okay now let's be clear: actually all of these behaviors should, end of day,
	//  actually send their errors through our output control.
	//  We still also return it, both so you can write tests around this
	//  method as a whole, and so the top of the program can choose an exit code!
	for _, bhv := range bhvs {
		_action := bhv.action
		bhv.action = func() error {
			err := _action()
			if err != nil {
				oc.EmitResult(api.WareID{}, err)
			}
			return err
		}
	}

	// Parse!
	parsedCmdStr, err := app.Parse(args[1:])
	oc.format = format(baseArgs.Format)
	if err != nil {
		return behavior{
			parsedArgs: err,
			action: func() error {
				err := Errorf(rio.ErrUsage, "error parsing args: %s", err)
				oc.EmitResult(api.WareID{}, err)
				return err
			},
		}
	}
	// Return behavior named by the command and subcommand strings.
	if bhv, ok := bhvs[parsedCmdStr]; ok {
		return *bhv
	}
	panic("unreachable, cli parser must error on unknown commands")
}

type outputController struct {
	format         format
	stdout, stderr io.Writer
}

func (oc outputController) EmitResult(wareID api.WareID, err error) {
	result := &rio.Event_Result{}
	result.WareID = wareID
	result.SetError(err)
	evt := rio.Event{Result: result}
	switch oc.format {
	case "", format_Dumb:
		if err != nil {
			fmt.Fprintln(oc.stderr, err)
		} else {
			fmt.Fprintln(oc.stdout, wareID)
		}
	case format_Json:
		if err != nil {
			fmt.Fprintln(oc.stderr, err)
		}
		marshaller := refmt.NewMarshallerAtlased(json.EncodeOptions{}, oc.stdout, rio.Atlas)
		err := marshaller.Marshal(evt)
		if err != nil {
			panic(err)
		}
	default:
		panic(fmt.Errorf("rio: invalid format %s", oc.format))
	}
}

func convertWarehouseSlice(slice []string) []api.WarehouseAddr {
	result := make([]api.WarehouseAddr, len(slice))
	for idx, item := range slice {
		result[idx] = api.WarehouseAddr(item)
	}
	return result
}
