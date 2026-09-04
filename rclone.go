package main

import (
	"runtime/debug"
	"strings"

	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/cmd"
	_ "github.com/rclone/rclone/cmd/all"
	"github.com/rclone/rclone/fs"

	_ "github.com/rijuyuezhu/rclone-pkudisk/backend/pkudisk"
)

func init() {
	applyProjectVersion()
	disableUpstreamSelfUpdate()
}

func applyProjectVersion() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if version := projectReleaseVersion(info.Main.Version); version != "" {
		fs.Version = version
	}
}

func projectReleaseVersion(version string) string {
	if strings.HasPrefix(version, "v") && strings.Contains(version, "-pkudisk.") {
		return version
	}
	return ""
}

func disableUpstreamSelfUpdate() {
	for _, command := range cmd.Root.Commands() {
		if command.Name() == "selfupdate" {
			cmd.Root.RemoveCommand(command)
			return
		}
	}
}

func main() {
	cmd.Main()
}
