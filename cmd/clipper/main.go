// Command clipper is the render service: it runs on every node.
//
//	clipper render ...   render one clip by hand (see clipper render -h)
//	clipper work ...     the job loop (not built yet, #34)
//	clipper version      print the version
//
// main only parses the subcommand and its flags and calls the code that
// runs it: render.go for clipper render, work.go for clipper work.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/salehjafarli/pogfarm/internal/clipper/download"
	"github.com/salehjafarli/pogfarm/internal/clipper/transcribe"
	"github.com/salehjafarli/pogfarm/internal/config"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// Exit codes.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

const usage = `usage:
  clipper render (--vod <id> [--platform twitch] | --input <file>) [--start <time>] [--end <time>]
                 [-o <out.mp4>] [--keep-work] [--config <file>]
  clipper work [--config <file>]
  clipper version

Times are written 1h02m10s, 62m10s, 1:02:10 or plain seconds (3730).
Run "clipper render -h" for the render flags.
`

// env is what a subcommand reads and writes besides its flags. Tests
// replace the transcriber, because WhisperX is not installed where they run.
type env struct {
	stdout, stderr io.Writer
	// transcriber replaces WhisperX when it is not nil.
	transcriber transcribe.Transcriber
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], env{stdout: os.Stdout, stderr: os.Stderr})
	stop()
	os.Exit(code)
}

// run dispatches to the subcommand named by args[0] and returns the exit
// code.
func run(ctx context.Context, args []string, e env) int {
	if len(args) == 0 {
		fmt.Fprint(e.stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "version", "-version", "--version":
		fmt.Fprintf(e.stdout, "clipper %s\n", version)
		return exitOK
	case "help", "-h", "-help", "--help":
		fmt.Fprint(e.stdout, usage)
		return exitOK
	case "render":
		f, err := parseRenderFlags(args[1:], e.stderr)
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		if err != nil {
			fmt.Fprintln(e.stderr, "clipper render:", err)
			return exitUsage
		}
		if err := runRender(ctx, f, e); err != nil {
			fmt.Fprintln(e.stderr, "clipper render:", err)
			return exitFail
		}
		return exitOK
	case "work":
		fs := flag.NewFlagSet("clipper work", flag.ContinueOnError)
		fs.SetOutput(e.stderr)
		configPath := fs.String("config", "", "config file (default: the path in $"+config.EnvConfigPath+")")
		if err := fs.Parse(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return exitOK
			}
			return exitUsage
		}
		if err := runWork(ctx, *configPath, e); err != nil {
			fmt.Fprintln(e.stderr, "clipper work:", err)
			return exitFail
		}
		return exitOK
	}
	fmt.Fprintf(e.stderr, "clipper: unknown command %q\n\n%s", args[0], usage)
	return exitUsage
}

// renderFlags are the flags of clipper render, as given.
type renderFlags struct {
	vod, platform, input string
	start, end           string
	output               string
	keepWork             bool
	configPath           string
}

// parseRenderFlags parses the flags of clipper render. It checks only that
// they parse and that no argument is left over; runRender checks what they
// mean.
func parseRenderFlags(args []string, stderr io.Writer) (renderFlags, error) {
	var f renderFlags
	fs := flag.NewFlagSet("clipper render", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&f.vod, "vod", "", "VOD id to render a range of (needs --start and --end)")
	fs.StringVar(&f.platform, "platform", download.PlatformTwitch, "platform of --vod")
	fs.StringVar(&f.input, "input", "", "local video file to render instead of a VOD")
	fs.StringVar(&f.start, "start", "", "start of the range: 1h02m10s, 1:02:10 or seconds")
	fs.StringVar(&f.end, "end", "", "end of the range, written like --start")
	fs.StringVar(&f.output, "o", "", "output file (default: a name built from the source and the range, in the current directory)")
	fs.BoolVar(&f.keepWork, "keep-work", false, "keep the work directory and print its path")
	fs.StringVar(&f.configPath, "config", "", "config file (default: the path in $"+config.EnvConfigPath+")")
	if err := fs.Parse(args); err != nil {
		return renderFlags{}, err
	}
	if fs.NArg() > 0 {
		return renderFlags{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return f, nil
}
