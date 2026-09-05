package pkudisk

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
)

func TestRegistrationLogFilterOnlySuppressesMissingOverview(t *testing.T) {
	var buf bytes.Buffer
	next := slog.NewTextHandler(&buf, nil)
	h := registrationLogFilter{
		Handler: next,
		message: `internal error: no overview data found for "pkudisk"`,
	}
	ctx := context.Background()

	if err := h.Handle(ctx, slog.NewRecord(time.Time{}, slog.LevelError, h.message, 0)); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("missing-overview message was forwarded: %q", buf.String())
	}

	const other = "another registration error"
	if err := h.Handle(ctx, slog.NewRecord(time.Time{}, slog.LevelError, other, 0)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), other) {
		t.Fatalf("unrelated registration error was suppressed: %q", buf.String())
	}
}

func TestRegisteredBackendHasExternalOverview(t *testing.T) {
	info, err := fs.Find("pkudisk")
	if err != nil {
		t.Fatal(err)
	}
	if info.Overview == nil || info.Overview.Backend != "pkudisk" || info.Overview.Tier != "External" {
		t.Fatalf("pkudisk overview = %#v", info.Overview)
	}
}
