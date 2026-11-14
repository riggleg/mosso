package server

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/riggleg/mosso/internal/claude"
	"github.com/riggleg/mosso/internal/vercel"
)

// TestAttachmentVisionEndToEnd checks, with real accounts, that Claude can
// see an uploaded image: it writes a solid orange PNG into a real sandbox
// the way an upload does, prompts Claude with the same note a message gets,
// and expects it to name the color, which it can only do by looking at the
// pixels (the filename says "image.png"). Opt in with:
//
//	MOSSO_E2E=1 go test ./internal/server -run TestAttachmentVisionEndToEnd -v -timeout 20m
func TestAttachmentVisionEndToEnd(t *testing.T) {
	if os.Getenv("MOSSO_E2E") != "1" {
		t.Skip("set MOSSO_E2E=1 to run against real Vercel and Claude accounts")
	}
	cfg, err := vercel.ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := claude.LoadToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	vc := vercel.NewClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	sb, err := vc.Create(ctx, vercel.NewSandboxName(), vercel.CreateOptions{Timeout: 20 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dctx, c := context.WithTimeout(context.Background(), 2*time.Minute)
		defer c()
		if err := vc.Destroy(dctx, *sb); err != nil {
			t.Errorf("destroy sandbox %s: %v (check the Vercel dashboard)", sb.Name, err)
		}
	})
	if _, err := claude.Install(ctx, vc, sb.SessionID); err != nil {
		t.Fatal(err)
	}

	img := image.NewRGBA(image.Rect(0, 0, 200, 200))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{R: 255, G: 128, B: 0, A: 255}}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if res, err := vc.Exec(ctx, sb.SessionID, "mkdir -p "+attachmentsDir); err != nil || res.ExitCode != 0 {
		t.Fatalf("mkdir: %v %+v", err, res)
	}
	a := attachment{ID: newAttachmentID(), Name: "image.png", Size: int64(buf.Len()), Type: "image/png", Image: true}
	if err := vc.WriteFiles(ctx, sb.SessionID, attachmentsDir, []vercel.File{{Name: a.ID + "/" + a.Name, Data: buf.Bytes()}}); err != nil {
		t.Fatal(err)
	}
	turn, err := claude.Send(ctx, vc, sb.SessionID, token, claude.Request{
		Prompt: claudePrompt("What is the dominant color of the attached image? Reply with only the color name in English.", []attachment{a}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("result %q, %d events", turn.Result, len(turn.Events))
	if turn.IsError {
		t.Fatalf("claude reported an error: %s", turn.Result)
	}
	if r := strings.ToLower(turn.Result); !strings.Contains(r, "orange") {
		t.Errorf("Claude answered %q, want orange: it did not see the image", turn.Result)
	}
}
