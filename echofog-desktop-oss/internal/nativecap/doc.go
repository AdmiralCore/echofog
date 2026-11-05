// Package nativecap is the app's own screen capture for calls. The webview's getDisplayMedia copies
// every frame through system memory and holds itself to half a CPU core, which keeps a shared screen
// at 30-50 fps; this package keeps the frames on the graphics card and gets a shared screen up to the
// monitor's refresh rate (60-144 fps at 1080p and 1440p).
//
// # The pipeline (Windows)
//
//	Windows.Graphics.Capture  ->  BGRA texture            wgc_windows.go
//	D3D11 video processor     ->  NV12 frame, scaled      d3d11_windows.go (converter)
//	Media Foundation H.264    ->  Annex-B access units    mfenc_windows.go (hardware async MFT)
//	temporal layers split     ->  frame + layer           layers.go
//	packetizer (pion)         ->  RTP packets             rtp.go
//	pacer                     ->  Options.Publish         rtp.go
//
// Options.Publish is the call's hook (the core's PublishCaptureRTP): the call sees the same H.264
// RTP the webview's encoder would have sent, so viewers, relays and old versions notice only the
// frame rate. Nothing but the compressed frame ever leaves the graphics card.
//
// # Who does what
//
//   - Start / Stream (stream_windows.go): one running stream. run is its goroutine; it owns the
//     device, the capture, the converter, the NV12 frames and the pacer, and ends with all of them
//     released. Stop waits for that.
//   - frameClock (nativecap.go): the beat that decides when a picture goes to the encoder, at the
//     stream's rate, whatever rate the capture runs at.
//   - ladder (ladder.go): the automatic quality. It steps the stream down the call's quality presets
//     when the bitrate does not cover the chosen one, and follows the viewers' screens (pickFPS).
//   - Sources (sources_windows.go) and Previews (previews_windows.go): what the picker lists and its
//     live thumbnails.
//   - StartAudio (audio_windows.go): the sound of the shared window's program (or of the computer, for
//     a monitor) as PCM, which the interface plays into the screen-sound track.
//   - com_windows.go: COM and WinRT through vtables, with no cgo and no Windows SDK.
//
// # Invariants worth knowing before changing anything
//
//   - Timestamps are capture times. A frame's RTP timestamp is when its picture was drawn
//     (SystemRelativeTime of the capture frame, on the QPC clock), not when it reached the encoder:
//     the viewer's player plays frames at the pace their timestamps say, and send-time stamps strayed
//     from that by a few milliseconds, which at 144 fps is most of a frame (ptsOf).
//   - An NV12 frame is not written while the encoder holds it (nv12Slot.busy, released in onFrame by
//     pts). Frames that find none free, or the encoder not asking for one, are skipped, not queued.
//   - A COM object lives on the stream's goroutine, the encoder's event goroutine, or is guarded by a
//     lock (encMu for the bitrate). An encoder is shut down and its event loop waited for before it
//     is released (mfEncoder.close): its threads work on their own.
//   - A quality change is a new encoder (size and frame rate are fixed for an encoder's life) opened
//     after the old one drained, with a keyframe first. The call's rate control changes only the
//     bitrate, on the running encoder.
//   - Keyframes leave at 2.5 times the bitrate, not in a burst (the pacer): a keyframe is 90-215 ms of
//     the bitrate whatever the encoder's rate buffer says, and a burst of it was dropped by the
//     routers on the way.
//   - SPS and PPS are in front of every IDR and in no other frame (some decoders give up on
//     parameter sets inside a non-IDR frame).
//
// # Measuring
//
// The tests that need a GPU, a screen or ffmpeg skip unless their environment variable is set (see
// each one's comment): TestStreamBench (the whole path, with rate and level changes),
// TestEncoderQualityBench (speed/quality/rate buffer, scored with ffmpeg's VMAF), TestCaptureBench,
// TestPreviewsBench, TestAudioBench and TestAudioWindowBench. testdata/gpu-bench.cmd runs the stream
// bench on another computer's graphics card.
//
// Off Windows the package compiles to stubs (stream_other.go): Available reports why there is no
// native capture, and the app shares the screen through the webview as before.
package nativecap
