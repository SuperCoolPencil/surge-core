package main

import (
	"context"
	"fmt"
	"os"

	"github.com/surge-downloader/surge-core/pkg/surgecore"
)

func main() {
	dir, _ := os.MkdirTemp("", "surge-smoke-*")
	defer os.RemoveAll(dir)

	d, err := surgecore.New(surgecore.Config{
		StateDir:    dir,
		Connections: 8,
	})
	if err != nil {
		fmt.Println("FAIL: New():", err)
		os.Exit(1)
	}
	defer d.Close()

	ch, err := d.Get(context.Background(), surgecore.AddRequest{
		URL:         "https://speed.cloudflare.com/__down?bytes=10000",
		Destination: dir,
	})
	if err != nil {
		fmt.Println("FAIL: Get():", err)
		os.Exit(1)
	}

	for ev := range ch {
		switch ev.Type {
		case surgecore.EventProgress:
			if ev.Progress != nil {
				fmt.Printf("Progress: %d / %d bytes\n", ev.Progress.BytesDownloaded, ev.Progress.TotalBytes)
			}
		case surgecore.EventComplete:
			fmt.Println("OK: Download complete:", ev.Path)
		case surgecore.EventError:
			fmt.Println("FAIL: Download error:", ev.Error)
			os.Exit(1)
		}
	}
}
