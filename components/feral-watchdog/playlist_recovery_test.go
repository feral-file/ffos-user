package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/feral-file/ffos-user/components/feral-watchdog/ff1config"

	"go.uber.org/zap"
)

type recordingNavigator struct {
	urls []string
}

func (r *recordingNavigator) Navigate(ctx context.Context, url string) error {
	r.urls = append(r.urls, url)
	if strings.HasPrefix(url, "http://127.0.0.1:8080") {
		return errors.New("player navigate fail")
	}
	return nil
}

type alwaysFailNavigator struct {
	urls []string
}

func (a *alwaysFailNavigator) Navigate(ctx context.Context, url string) error {
	a.urls = append(a.urls, url)
	return errors.New("navigate fail")
}

func TestResumePlaylist_LocalPlayerTCPWaitFails_ShowsLauncherMessage(t *testing.T) {
	t.Parallel()
	var nav recordingNavigator
	wait := func(context.Context) error { return errors.New("tcp down") }
	resumePlaylistAfterServiceRecovery(context.Background(), &nav, zap.NewNop(), "feral-setupd.service", ff1config.DefaultWebappURL, wait)
	if len(nav.urls) != 1 {
		t.Fatalf("urls=%v", nav.urls)
	}
	want := ff1config.LauncherMessageNavigateURL(ff1config.LocalPlayerUnavailableMessage)
	if nav.urls[0] != want {
		t.Fatalf("got %q want %q", nav.urls[0], want)
	}
}

func TestResumePlaylist_RemoteURL_DoesNotCallWaitTCP(t *testing.T) {
	t.Parallel()
	var nav recordingNavigator
	wait := func(context.Context) error {
		t.Fatal("wait should not run for remote URL")
		return nil
	}
	resumePlaylistAfterServiceRecovery(context.Background(), &nav, zap.NewNop(), "feral-setupd.service", "https://display.feralfile.com/", wait)
	if len(nav.urls) != 1 {
		t.Fatalf("urls=%v", nav.urls)
	}
	if nav.urls[0] != "https://display.feralfile.com/" {
		t.Fatalf("got %q", nav.urls[0])
	}
}

func TestResumePlaylist_LocalPlayerWaitOK_PlayerNavigateFails_ShowsLauncherMessage(t *testing.T) {
	t.Parallel()
	var nav recordingNavigator
	wait := func(context.Context) error { return nil }
	resumePlaylistAfterServiceRecovery(context.Background(), &nav, zap.NewNop(), "feral-setupd.service", ff1config.DefaultWebappURL, wait)
	if len(nav.urls) != playerNavigateRetries+1 {
		t.Fatalf("got %d urls %v", len(nav.urls), nav.urls)
	}
	for i := 0; i < playerNavigateRetries; i++ {
		if nav.urls[i] != ff1config.DefaultWebappURL {
			t.Fatalf("attempt %d got %q", i, nav.urls[i])
		}
	}
	want := ff1config.LauncherMessageNavigateURL(ff1config.LocalPlayerUnavailableMessage)
	if nav.urls[len(nav.urls)-1] != want {
		t.Fatalf("last url got %q", nav.urls[len(nav.urls)-1])
	}
}

func TestResumePlaylist_RemoteNavigateFails_NoLauncherFallback(t *testing.T) {
	t.Parallel()
	var nav alwaysFailNavigator
	resumePlaylistAfterServiceRecovery(context.Background(), &nav, zap.NewNop(), "feral-setupd.service", "https://display.feralfile.com/", nil)
	if len(nav.urls) != playerNavigateRetries {
		t.Fatalf("got %d urls %v", len(nav.urls), nav.urls)
	}
	for _, u := range nav.urls {
		if strings.HasPrefix(u, "file://") {
			t.Fatalf("unexpected file URL in remote failure path: %q", u)
		}
	}
}
