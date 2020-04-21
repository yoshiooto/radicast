package main

import (
	"fmt"
	"os/exec"
	"regexp"
)

func lookConverterCommand() (string, error) {
	for _, p := range []string{"ffmpeg", "avconv"} {
		cmd, err := exec.LookPath(p)
		if err == nil {
			return cmd, nil
		}
	}
	return "", fmt.Errorf("not found converter cmd such also ffmpeg, avconv.")
}

func newConverterCmd(path, bitrate, output string, title string, author string) (*exec.Cmd, error) {

	switch {
	case regexp.MustCompile("ffmpeg$").MatchString(path):
		return newFfmpegCmd(path, bitrate, output, title, author), nil
	case regexp.MustCompile("avconv$").MatchString(path):
		return newAvconvCmd(path, bitrate, output), nil
	}

	return nil, fmt.Errorf("path should be ffmpeg or avconv")
}

func newFfmpegCmd(ffmpeg, bitrate, output string, title string, author string) *exec.Cmd {
	metatitle := fmt.Sprintf("title=%s", title)
	metaauthor := fmt.Sprintf("artist=%s", author)
	return exec.Command(
		ffmpeg,
		"-y",
		"-i", "-",
		"-vn",
		"-acodec", "copy",
		"-metadata", metatitle,
		"-metadata", metaauthor,
		"-metadata", "genre=radio",
		output,
	)
}

func newAvconvCmd(avconv, bitrate, output string) *exec.Cmd {
	return exec.Command(
		avconv,
		"-y",
		"-i", "-",
		"-vn",
		"-c:a", "copy",
		output,
	)
}
