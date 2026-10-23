//go:build !windows

package main

import (
	"fmt"

	"github.com/urfave/cli/v2"
)

// runTray has no icon surface outside Windows; the background daemon is the
// equivalent there and keeps the binary CGO-free.
func runTray(_ *cli.Context) error {
	fmt.Println("Tray mode is Windows-only. Use '9router-go start' for a background run.")
	return nil
}
