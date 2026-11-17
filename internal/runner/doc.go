// Package runner runs external tools (TwitchDownloaderCLI, ffmpeg, the
// transcriber) as subprocesses with a timeout, cancellation and captured logs.
//
// A command is started directly, never through a shell, in its own process
// group. When the context is cancelled or the timeout passes, the whole group
// is killed, so helpers the tool started die with it. Stdout and stderr are
// captured with a size cap that keeps the tail, and a failed run returns an
// *Error carrying the command name, the exit code and the last lines of
// stderr.
package runner
