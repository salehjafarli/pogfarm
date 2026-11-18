// Package render is the clipper's render core: it turns a moment of a stream
// into a finished vertical clip, and knows nothing about pogfarm.
//
// Two commands use it. clipper render (cmd/clipper) runs it by hand on one
// range; clipper work (#34) wraps it with the job loop and turns each
// api.Job into a Request. This package has its own Request and Result and
// never imports internal/clipper/work, the job contract in internal/api,
// or anything under internal/pogfarm; a test checks that.
//
// # Steps
//
//	r, err := render.New(cfg, render.Options{Logger: logger})
//	res, err := r.Render(ctx, render.Request{Input: "stream.mp4", Start: 10 * time.Second, End: 55 * time.Second}, workDir)
//
// Render works inside workDir, which the caller creates and owns (Render
// never deletes it), and runs these steps in order:
//
//  1. download: the exact range, by package download, from the platform or
//     a local file (source.mp4, or the input's own name when it is copied
//     whole);
//  2. detect: where the streamer's webcam is, by a Detector, written to
//     detection.json;
//  3. layout: the arrangement for the source's picture size and the
//     detection, by package layout;
//  4. transcribe: the words with times, by a transcribe.Transcriber
//     (WhisperX by default), written to transcript.json;
//  5. caption: the ASS subtitle file, by package caption (captions.ass);
//  6. encode: one ffmpeg run that writes clip.mp4;
//  7. verify: ffprobe checks the clip.
//
// Result carries the paths, the transcript, the clip's duration, picture
// size and file size, every decision (detector, detection, arrangement,
// crops, fallback reason, caption lines), the time per step and the ffmpeg
// version. Result.Summary formats it for people.
//
// # Webcam detection
//
// The Detector is chosen once, when the Renderer is built, from
// clipper.webcam_detection.enabled. Only OffDetector exists so far: it
// answers "no webcam" for every clip, so every clip uses blur fill. Until
// the real detector is built (#59), both values of the setting select it,
// and NewDetector logs that. A detector error never fails a render: it is
// logged, recorded in Result.DetectionError and treated as no webcam.
//
// # The final encode
//
// One ffmpeg run reads the downloaded range and writes the clip:
//
//   - -filter_complex: the layout's fragment from [0:v] to [layout], then
//     on [layout] the ass filter with the caption file (its path escaped
//     for the filter graph by EscapeFilterPath), fps=<frame_rate> for a
//     constant frame rate (clipper.render.frame_rate, default 30) and
//     format=yuv420p; the first audio stream goes through loudnorm in a
//     single pass (-14 LUFS, -1.5 dBTP true peak, LRA 11) and back to
//     48 kHz;
//   - video: libx264 with clipper.render.preset and crf, High profile,
//     yuv420p, -fps_mode cfr;
//   - the size cap: -maxrate and -bufsize from VideoBitrateCap, which plans
//     95 % of clipper.render.max_bytes (default 45 MB, under Telegram's
//     50 MB bot limit) over the clip's length, minus the audio. CRF sets the
//     quality, and the cap only lowers it on a long, busy clip. A clip so
//     long that the cap would fall below MinVideoBitrate fails with
//     ErrTooLong;
//   - audio: AAC, 128 kbit/s, 48 kHz stereo;
//   - -movflags +faststart, so playback starts before the file is loaded.
//
// ffmpeg reports progress on stdout (-progress pipe:1), and the render logs
// it every tenth of the clip. Its timeout is clipper.render.timeout_base
// plus timeout_per_second per second of the clip.
//
// # Checks
//
// ffprobe checks the clip: H.264 at 1080x1920 in yuv420p, an AAC audio
// stream, a duration within DurationTolerance of the range, and a size of
// at most max_bytes. A failed check is ErrVerification, which is
// retryable.
//
// # Errors and cancellation
//
// Errors a retry will not fix match ErrPermanent, which is
// download.ErrPermanent: an invalid request, an unknown platform, a missing
// input, a deleted or subscriber-only VOD, a source too small to lay out,
// and ErrTooLong. Everything else is worth retrying, possibly on another
// node. Every tool runs through internal/runner, so cancelling ctx kills
// the running tool and its children at once; the error then matches
// ctx.Err().
//
// # Running it by hand
//
// clipper render needs, in PATH or at the paths in the config file:
//
//   - ffmpeg and ffprobe, built with libass and libx264 (Debian's and
//     Ubuntu's packages are);
//   - WhisperX with its model (clipper.transcribe.model) and, for word
//     timing, the alignment model of the language;
//   - TwitchDownloaderCLI 1.56.5, only for --vod;
//   - the caption font (Montserrat ExtraBold by default); without it libass
//     uses another font.
//
// The clipper base image (deploy/clipper-base.Dockerfile) has all of them.
// The command needs no pogfarm address or secret: it loads the config with
// config.LoadClipperRender. From a file already on disk:
//
//	clipper render --input stream.mp4 --start 1h02m10s --end 1h02m55s -o clip.mp4
//
// From a Twitch VOD:
//
//	clipper render --vod 2412345678 --start 1h02m10s --end 1h02m55s
//
// Without -o the clip goes to the current directory, named after the source
// and the range (twitch-2412345678_1h02m10s-1h02m55s.mp4); the transcript
// is written next to it with the extension .json. Without --start and
// --end, --input renders the whole file. Times are also accepted as
// 1:02:10 or plain seconds.
//
// The command renders in a new directory under $TMPDIR and deletes it
// afterwards. With --keep-work it keeps it and prints its path as the last
// line of the summary. It then holds:
//
//	source.mp4       the downloaded range (or the input's copy)
//	detection.json   the detector's answer: {"detector":"off","webcam":false}
//	audio.wav        the audio WhisperX read
//	whisperx/        WhisperX's own output
//	captions.ass     the subtitle file burned in; open it with the source in
//	                 a player that reads ASS (mpv) to check the timing
//	transcript.json and clip.mp4 have been moved to the output.
//
// When a clip looks wrong, check detection.json and the arrangement in the
// summary for the layout, and captions.ass for the captions.
package render
