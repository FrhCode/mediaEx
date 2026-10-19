package app

import (
	"strings"
	"testing"
)

func md(container, video string, audio ...string) Metadata {
	return Metadata{Width: 1920, Height: 1080, Duration: 30, Container: container, VideoCodec: video, AudioCodecs: audio}
}

func TestPlaybackPlan(t *testing.T) {
	const (
		mp4Probe  = "mov,mp4,m4a,3gp,3g2,mj2"
		mkvProbe  = "matroska,webm"
		aviProbe  = "avi"
		mpegProbe = "mpeg"
	)
	for _, tc := range []struct {
		name     string
		format   string
		md       Metadata
		strategy string
	}{
		{"mp4 h264 aac", "MP4", md(mp4Probe, "h264", "aac"), PlanDirect},
		{"mp4 h264 mp3", "MP4", md(mp4Probe, "h264", "mp3"), PlanTranscodeAudio},
		{"mp4 h264 no audio", "MP4", md(mp4Probe, "h264"), PlanDirect},
		{"mov h264 aac", "MOV", md(mp4Probe, "h264", "aac"), PlanDirect},
		{"webm vp9 opus", "WEBM", md(mkvProbe, "vp9", "opus"), PlanDirect},
		{"webm vp8 vorbis", "WEBM", md(mkvProbe, "vp8", "vorbis"), PlanDirect},
		{"mkv h264 aac", "MKV", md(mkvProbe, "h264", "aac"), PlanRemux},
		{"avi h264 aac", "AVI", md(aviProbe, "h264", "aac"), PlanRemux},
		{"mkv renamed to mp4", "MP4", md(mkvProbe, "h264", "aac"), PlanRemux},
		{"mp4 h264 ac3", "MP4", md(mp4Probe, "h264", "ac3"), PlanTranscodeAudio},
		{"mp4 h264 dts", "MP4", md(mp4Probe, "h264", "dts"), PlanTranscodeAudio},
		{"mkv h264 truehd", "MKV", md(mkvProbe, "h264", "truehd"), PlanTranscodeAudio},
		{"mkv mpeg2 ac3", "MKV", md(mkvProbe, "mpeg2video", "ac3"), PlanTranscodeFull},
		{"avi msmpeg4 mp3", "AVI", md(aviProbe, "msmpeg4v3", "mp3"), PlanTranscodeFull},
		{"mp4 hevc aac", "MP4", md(mp4Probe, "hevc", "aac"), PlanTranscodeFull},
		{"webm av1 opus", "WEBM", md(mkvProbe, "av1", "opus"), PlanTranscodeFull},
		{"mkv vp9 opus", "MKV", md(mkvProbe, "vp9", "opus"), PlanTranscodeFull},
		{"mpeg mpeg2 mp2", "MPEG", md(mpegProbe, "mpeg2video", "mp2"), PlanTranscodeFull},
		{"audio only", "MP4", Metadata{Container: mp4Probe, AudioCodecs: []string{"aac"}}, PlanUnsupported},
		{"no streams", "MP4", Metadata{Container: mp4Probe}, PlanUnsupported},
		{"zero dimensions", "MP4", Metadata{Container: mp4Probe, VideoCodec: "h264"}, PlanUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := playbackPlan(tc.format, tc.md)
			if plan.Strategy != tc.strategy {
				t.Fatalf("got %q want %q (reason %q)", plan.Strategy, tc.strategy, plan.Reason)
			}
			if plan.Strategy == PlanDirect && plan.Reason != "" {
				t.Fatalf("direct plan carries a reason: %q", plan.Reason)
			}
			if plan.Strategy != PlanDirect && plan.Reason == "" {
				t.Fatal("non-direct plan must explain itself")
			}
		})
	}
}

func TestConversionArgs(t *testing.T) {
	joined := func(args []string) string { return strings.Join(args, " ") }
	for _, strategy := range []string{PlanRemux, PlanTranscodeAudio, PlanTranscodeFull} {
		args := conversionArgs(strategy, "/out/tmp.mp4")
		j := joined(args)
		for _, want := range []string{"-protocol_whitelist file,pipe,fd", "-format_whitelist " + safeDemuxers, "-i /proc/self/fd/3", "-map 0:v:0", "-map 0:a:0?", "-movflags +faststart", "-f mp4"} {
			if !strings.Contains(j, want) {
				t.Fatalf("%s args missing %q: %s", strategy, want, j)
			}
		}
		if args[len(args)-1] != "/out/tmp.mp4" {
			t.Fatalf("target must be the final argument: %s", j)
		}
	}
	if j := joined(conversionArgs(PlanRemux, "/o.mp4")); !strings.Contains(j, "-c:v copy") || !strings.Contains(j, "-c:a copy") {
		t.Fatal("remux must copy both streams: " + j)
	}
	if j := joined(conversionArgs(PlanTranscodeAudio, "/o.mp4")); !strings.Contains(j, "-c:v copy") || !strings.Contains(j, "-c:a aac") {
		t.Fatal("audio transcode must copy video and encode aac: " + j)
	}
	full := joined(conversionArgs(PlanTranscodeFull, "/o.mp4"))
	for _, want := range []string{"-c:v libx264", "-pix_fmt yuv420p", "scale=trunc(iw/2)*2:trunc(ih/2)*2", "-c:a aac"} {
		if !strings.Contains(full, want) {
			t.Fatalf("full transcode args missing %q: %s", want, full)
		}
	}
}
