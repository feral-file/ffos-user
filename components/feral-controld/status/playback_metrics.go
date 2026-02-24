package status

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	playbackMetricsRegistry = prometheus.NewRegistry()

	ffArtPlaybackDurationSecondsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ff_art_playback_duration_seconds_total",
		Help: "Total duration in seconds where artwork playback is active.",
	})
)

func init() {
	playbackMetricsRegistry.MustRegister(ffArtPlaybackDurationSecondsTotal)
}

func PlaybackMetricsGatherer() prometheus.Gatherer {
	return playbackMetricsRegistry
}

func isArtworkPlaying(playerStatus *PlayerStatus) bool {
	if playerStatus == nil || !playerStatus.Ok {
		return false
	}

	if playerStatus.IsPaused == nil {
		return false
	}

	return !*playerStatus.IsPaused
}

func (s *poller) updateArtPlaybackMetrics(isPlaying bool, now time.Time) {
	if !s.playbackSampleInitialized {
		s.lastPlaybackSampleAt = now
		s.lastIsPlaying = isPlaying
		s.playbackSampleInitialized = true
		return
	}

	// Accumulate only while previous sampled state is active artwork playback.
	if s.lastIsPlaying {
		deltaSeconds := now.Sub(s.lastPlaybackSampleAt).Seconds()
		ffArtPlaybackDurationSecondsTotal.Add(deltaSeconds)
	}

	s.lastPlaybackSampleAt = now
	s.lastIsPlaying = isPlaying
}
