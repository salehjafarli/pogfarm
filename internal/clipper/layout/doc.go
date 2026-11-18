// Package layout decides how a 16:9 stream picture is arranged in a 9:16
// clip, and writes that arrangement as an ffmpeg filter graph fragment.
//
// It is pure logic: it runs no tool and touches no file. The render core
// (internal/clipper/render) calls it once per clip and joins the fragment
// with the caption burn-in in its single ffmpeg pass.
//
// # The two arrangements
//
// The output is always OutWidth x OutHeight (1080x1920). There is one rule
// and no other choice:
//
//   - Webcam found: Stack. The webcam region is scaled and cropped to fill
//     a band across the top; its share of the height is
//     clipper.layout.webcam_share (default one third), the only layout
//     setting. A centre slice of the frame, the gameplay, is scaled and
//     cropped to fill the rest below it.
//   - No webcam: BlurFill. The whole frame is scaled to 1080 wide and
//     centred vertically, over a blurred, zoomed copy of itself that fills
//     1080x1920.
//
// # Detection is defined here
//
// Detection is the result of webcam detection (issue #59, package
// internal/clipper/detect): either no webcam, or a webcam Region given as
// fractions of the source frame, so the same result means the same thing at
// 720p and 1080p. Package detect imports this type from layout and does not
// define its own. Until it exists, the render core passes NoWebcam() for
// every clip, so every clip uses BlurFill.
//
// A webcam region is not trusted blindly. The part outside the frame is cut
// off (clamped). A region with a zero, negative or non-finite size, one that
// lies wholly outside the frame, or one that leaves less than
// MinWebcamPixels on a side after it is fitted to the band falls back to
// BlurFill, and Result.Fallback says why. No input ever produces a crop
// outside the frame.
//
// # The fragment
//
// Result.Filter reads the source video from the label [0:v] (InputLabel)
// and writes the laid-out picture to [layout] (OutputLabel). Intermediate
// labels all start with "layout_", so they do not clash with the caller's
// own. The caller appends its own filters, for example:
//
//	<Result.Filter>;[layout]subtitles=captions.ass[v]
//
// The fragment ends with setsar=1. It does not set the pixel format; the
// encoder settings choose yuv420p. Every crop and scale size it writes is
// even, as yuv420p requires, and crop offsets are even too, so chroma is
// not shifted.
package layout
