//go:build !linux

package main

import "os/exec"

// setToolProcAttr does nothing here: Pdeathsig exists only on Linux, where
// the bot is deployed (see ffmpeg_linux.go).
func setToolProcAttr(*exec.Cmd) {}
