package status

import (
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	playbackMetricsRegistry = prometheus.NewRegistry()

	artPlaybackDurationSecondsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "art_playback_duration_seconds_total",
		Help: "Total duration in seconds where artwork playback is active.",
	})

	playbackStartTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "playback_start_total",
		Help: "Total number of playback start attempts (displayPlaylist commands received).",
	})

	playbackStartFailuresTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "playback_start_failures_total",
		Help: "Total number of playback start failures.",
	})
)

func init() {
	playbackMetricsRegistry.MustRegister(artPlaybackDurationSecondsTotal)
	playbackMetricsRegistry.MustRegister(playbackStartTotal)
	playbackMetricsRegistry.MustRegister(playbackStartFailuresTotal)
}

func PlaybackMetricsGatherer() prometheus.Gatherer {
	return playbackMetricsRegistry
}

// RecordPlaybackAttempt increments the counter for every playback start attempt.
func RecordPlaybackAttempt() {
	playbackStartTotal.Inc()
}

// RecordPlaybackFailure increments the failure counter.
func RecordPlaybackFailure() {
	playbackStartFailuresTotal.Inc()
}

// PlaybackStartTotal returns the current value of the playback_start_total counter.
func PlaybackStartTotal() float64 {
	return counterValue(playbackStartTotal)
}

// PlaybackStartFailures returns the current value of the playback_start_failures_total counter.
func PlaybackStartFailures() float64 {
	return counterValue(playbackStartFailuresTotal)
}

// counterValue reads the current float64 value from a prometheus.Collector that
// exposes exactly one Counter metric.
func counterValue(c prometheus.Collector) float64 {
	ch := make(chan prometheus.Metric, 1)
	c.Collect(ch)
	m := <-ch
	pb := &dto.Metric{}
	if err := m.Write(pb); err != nil {
		return 0
	}
	if pb.Counter != nil {
		return pb.Counter.GetValue()
	}
	return 0
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
		artPlaybackDurationSecondsTotal.Add(deltaSeconds)
	}

	s.lastPlaybackSampleAt = now
	s.lastIsPlaying = isPlaying
}
