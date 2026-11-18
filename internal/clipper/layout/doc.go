// Package layout decides how a 16:9 stream picture is arranged in a 9:16
// clip, framed on the streamer's face, and writes that arrangement as an
// ffmpeg filter graph fragment.
//
// It is pure logic: it runs no tool and touches no file. The render core
// (internal/clipper/render) calls it once per clip and joins the fragment
// with the caption burn-in in its single ffmpeg pass.
//
// # The three arrangements
//
// The output is always OutWidth x OutHeight (1080x1920). Face detection
// (package internal/clipper/detect) picks the arrangement; this package
// applies it:
//
//   - Stack: one steady face, small, gameplay around it. A band framed on
//     the face fills the top BandHeight (640, one third of the height); a
//     centre slice of the frame, the gameplay, fills the rest below it.
//   - FaceCrop: one steady face filling the picture. The whole clip is
//     framed on the face.
//   - BlurFill: no steady face, or an ambiguous one, or detection off or
//     failed. The whole frame is scaled to 1080 wide and centred vertically,
//     over a blurred, zoomed copy of itself that fills 1080x1920.
//
// # Framing
//
// The face is placed by rules relative to its own size, so a small face is
// zoomed in and a big one zoomed out, and every clip looks alike whatever
// the source. One crop is used for the whole clip; it never moves.
//
//   - Size: the face's height is clipper.layout.stack_face_share (0.45) of
//     the band in Stack, and crop_face_share (0.25) of the clip in FaceCrop.
//   - Eye line: the eyes, EyeLevel (0.4) of the way down the face box, sit
//     eye_line (one third) of the way down the framed area.
//   - Centring: the crop is centred on the face horizontally.
//   - Edges: the crop stays inside the frame; near an edge it shifts rather
//     than show black. A crop that would be larger than the frame is shrunk
//     to the largest that fits, and the face is then larger than its share.
//     When the shift moves the face, the face goes off-centre or off the eye
//     line, but it is never cut: the crop always holds the whole face when
//     it is large enough to. Only a face larger than the largest possible
//     crop is cut, evenly on both sides.
//
// # Detection is defined here
//
// Detection is the result of face detection: a face, with its Box as
// fractions of the source frame (so the same result means the same thing at
// 720p and 1080p) and the arrangement it calls for, or none with a Reason.
// Package detect imports these types from layout and does not define its
// own.
//
// A face box is not trusted blindly. The part outside the frame is cut off
// (clamped). A box with a zero, negative or non-finite size, one that lies
// wholly outside the frame, or one whose framed crop is smaller than
// MinCropPixels on a side falls back to BlurFill, and Result.Fallback says
// why. No input ever produces a crop outside the frame.
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
