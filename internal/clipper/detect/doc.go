// Package detect finds the streamer's face in a clip, which decides how the
// clip is laid out (package internal/clipper/layout).
//
// The goal is a well-framed face, not the webcam overlay's border: there is
// no border search and no per-streamer setting. Everything is decided per
// clip, once, from a few frames.
//
// # Steps
//
//	d, err := detect.New(cfg.WebcamDetection, detect.Options{})
//	rep, err := d.Detect(ctx, "source.mp4", 45*time.Second, workDir)
//	// rep.Detection is a layout.Detection
//
// Detect runs these steps:
//
//  1. Frames: clipper.webcam_detection.frames (8) frames spread evenly
//     across the clip (FrameTimes) are written to workDir/detect/ as
//     full-size JPEGs, one ffmpeg run each. One frame is not enough: the
//     streamer may look away in it, and games contain faces too.
//  2. Faces: a FaceFinder looks for faces in the frames. The real one,
//     YuNet, runs the clipper image's detect-faces program
//     (deploy/clipper-base/detect-faces: OpenCV's YuNet detector) once on
//     all of them, through internal/runner. Go never embeds Python.
//  3. Decision: Decide, which is pure Go, keeps faces above min_confidence
//     (0.7) and min_face_height (3% of the frame height), groups them across
//     frames by position (a face may move max_movement, half its own
//     height, from the group's median), and calls a group steady when it is
//     in steady_share (60%, so 5 of 8) of the frames. The webcam face stays
//     put; faces in the game move or disappear.
//  4. Several steady faces: the tallest is the streamer's when it is at
//     least dominance (1.5) times as tall as the next; otherwise the clip is
//     ambiguous. Size is the box's height throughout.
//  5. Arrangement: a steady face taller than face_crop_height (20%) of the
//     frame fills the picture and gets layout.FaceCrop; a smaller one gets
//     layout.Stack. A corner webcam face is usually 5 to 10% of the height.
//  6. Position: the face's median box across the frames it was found in.
//     One crop is used for the whole clip; there is no tracking.
//
// No steady face gives layout.NoFace with layout.ReasonNoFace, an ambiguous
// one layout.ReasonAmbiguous; Detection.Detail says why in words.
//
// The whole detection is bounded by clipper.webcam_detection.timeout. Every
// failure (ffmpeg, the detector, a timeout, no readable frame) is an error,
// which the render core turns into "no face" (layout.ReasonError): detection
// never fails a clip.
//
// # The detector program
//
// detect-faces takes image paths and prints one JSON document with each
// image's size and faces (pixel boxes with a confidence). It exits 0 when
// every image was read, 1 when some were not (their entries carry an error;
// the others are used), and 2 when its arguments or its model are wrong.
// YuNet asks it to report faces down to a confidence of 0.5 (or
// min_confidence, if lower), so near misses show in detection.json, and
// passes --model from clipper.webcam_detection.model. Nothing is fetched
// from the network at job time: the model is a file baked into the image.
//
// # Startup check
//
// Check runs before clipper render does anything, and will run before
// clipper work claims a job. With detection enabled it requires the model
// file and a clean run of detect-faces on a small generated PNG, so a
// missing program, Python, OpenCV or model stops the clipper with a message
// that names it and says to install it or set
// clipper.webcam_detection.enabled: false. Disabled, it checks nothing.
//
// # Running it by hand
//
// The clipper image has detect-faces at /opt/pogfarm/bin/detect-faces and
// the model at the default path. On a Mac, scripts/setup-clipper-macos.sh
// installs OpenCV and the model and writes the matching settings. Then
//
//	clipper render --input stream.mp4 --start 1:02:10 --end 1:02:55 --keep-work
//
// leaves detect.jpg (a frame with every face, the face chosen and the crops
// drawn on it) and detection.json (every frame's faces, the groups and the
// decision) in the work directory.
package detect
