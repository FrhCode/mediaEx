package app

import "strings"

const (
	PlanDirect         = "direct"
	PlanRemux          = "remux"
	PlanTranscodeAudio = "transcode_audio"
	PlanTranscodeFull  = "transcode_full"
	PlanUnsupported    = "unsupported"
)

type PlaybackPlan struct {
	Strategy string
	Reason   string
}

func mp4Audio(codecs []string) bool {
	for _, c := range codecs {
		if c != "aac" {
			return false
		}
	}
	return true
}

func webmAudio(codecs []string) bool {
	for _, c := range codecs {
		if c != "vorbis" && c != "opus" {
			return false
		}
	}
	return true
}

func playbackPlan(format string, md Metadata) PlaybackPlan {
	if md.VideoCodec == "" || md.Width <= 0 || md.Height <= 0 {
		return PlaybackPlan{PlanUnsupported, "This file has no playable video stream."}
	}
	mp4Video := md.VideoCodec == "h264"
	webmVideo := md.VideoCodec == "vp8" || md.VideoCodec == "vp9"
	// A renamed file must not be trusted by extension alone: MP4/MOV probes always
	// name the mov/mp4 family, so a mismatch means the container lied. WebM and
	// Matroska share one demuxer and cannot be told apart, so WEBM stays on trust.
	probedMP4 := strings.Contains(md.Container, "mp4") || strings.Contains(md.Container, "mov")
	mp4Container := (format == "MP4" || format == "MOV") && probedMP4
	if mp4Container && mp4Video && mp4Audio(md.AudioCodecs) {
		return PlaybackPlan{Strategy: PlanDirect}
	}
	if format == "WEBM" && strings.Contains(md.Container, "webm") && webmVideo && webmAudio(md.AudioCodecs) {
		return PlaybackPlan{Strategy: PlanDirect}
	}
	if mp4Video && mp4Audio(md.AudioCodecs) {
		return PlaybackPlan{PlanRemux, "The " + format + " container is not browser-friendly; the video is being repackaged as MP4."}
	}
	if mp4Video {
		return PlaybackPlan{PlanTranscodeAudio, "The audio track is not browser-compatible and is being converted."}
	}
	return PlaybackPlan{PlanTranscodeFull, "The video codec (" + md.VideoCodec + ") is not browser-compatible and is being converted."}
}

// conversionArgs builds the FFmpeg invocation for one derivation. The input is
// always the read-only source on fd 3; the output is always a faststart MP4
// holding only the first video and (optionally) first audio stream.
func conversionArgs(strategy, target string) []string {
	args := []string{
		"-nostdin", "-v", "error",
		"-protocol_whitelist", "file,pipe,fd",
		"-format_whitelist", safeDemuxers,
		"-i", "/proc/self/fd/3",
		"-map", "0:v:0", "-map", "0:a:0?",
	}
	switch strategy {
	case PlanRemux:
		args = append(args, "-c:v", "copy", "-c:a", "copy")
	case PlanTranscodeAudio:
		args = append(args, "-c:v", "copy", "-c:a", "aac", "-b:a", "160k")
	case PlanTranscodeFull:
		// yuv420p requires even dimensions; truncate odd sources down a pixel.
		args = append(args,
			"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2",
			"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p",
			"-c:a", "aac", "-b:a", "160k")
	}
	return append(args, "-movflags", "+faststart", "-f", "mp4", "-y", target)
}
