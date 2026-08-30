package main

import (
	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/cmd"
	_ "github.com/rclone/rclone/cmd/all"

	_ "github.com/rijuyuezhu/rclone-pkudisk/backend/pkudisk"
)

func main() {
	cmd.Main()
}
