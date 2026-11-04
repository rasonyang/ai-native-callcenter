// SPDX-License-Identifier: Apache-2.0

// Package aicc checks the recorded prompts the switch plays when the bot is
// unavailable (see README.md).
package aicc

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	promptName = "bot-unavailable.wav"
	minLen     = 1500 * time.Millisecond
	maxLen     = 8 * time.Second
	maxEdge    = 200 * time.Millisecond
	// quiet is -45 dBFS of a 16-bit sample.
	quiet = 32768 * 0.0056234
)

type pcm struct {
	rate    int
	samples []int16
}

func readWAV(path string) (pcm, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return pcm{}, err
	}
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return pcm{}, fmt.Errorf("not a RIFF/WAVE file")
	}
	var out pcm
	haveFmt := false
	for p := 12; p+8 <= len(b); {
		id, size := string(b[p:p+4]), int(binary.LittleEndian.Uint32(b[p+4:p+8]))
		body := b[p+8:]
		if size > len(body) {
			return pcm{}, fmt.Errorf("chunk %q claims %d bytes, %d remain", id, size, len(body))
		}
		body = body[:size]
		switch id {
		case "fmt ":
			if size < 16 {
				return pcm{}, fmt.Errorf("fmt chunk is %d bytes", size)
			}
			if tag := binary.LittleEndian.Uint16(body[0:]); tag != 1 {
				return pcm{}, fmt.Errorf("format tag %d, want 1 (PCM, not extensible)", tag)
			}
			if ch := binary.LittleEndian.Uint16(body[2:]); ch != 1 {
				return pcm{}, fmt.Errorf("%d channels, want 1", ch)
			}
			out.rate = int(binary.LittleEndian.Uint32(body[4:]))
			if out.rate != 8000 {
				return pcm{}, fmt.Errorf("sample rate %d Hz, want 8000", out.rate)
			}
			if bits := binary.LittleEndian.Uint16(body[14:]); bits != 16 {
				return pcm{}, fmt.Errorf("%d bits per sample, want 16", bits)
			}
			haveFmt = true
		case "data":
			if !haveFmt {
				return pcm{}, fmt.Errorf("data chunk before fmt chunk")
			}
			out.samples = make([]int16, size/2)
			for i := range out.samples {
				out.samples[i] = int16(binary.LittleEndian.Uint16(body[2*i:]))
			}
			return out, nil
		}
		p += 8 + size + size%2
	}
	return pcm{}, fmt.Errorf("no data chunk")
}

func (p pcm) ms(n int) time.Duration {
	return time.Duration(n) * time.Second / time.Duration(p.rate)
}

func TestPromptsAreTelephoneAudio(t *testing.T) {
	for _, lang := range []string{"en", "zh"} {
		path := filepath.Join(lang, promptName)
		t.Run(lang, func(t *testing.T) {
			p, err := readWAV(path)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if d := p.ms(len(p.samples)); d < minLen || d > maxLen {
				t.Errorf("%s: duration %v outside [%v, %v]", path, d, minLen, maxLen)
			}
			lead := 0
			for lead < len(p.samples) && math.Abs(float64(p.samples[lead])) < quiet {
				lead++
			}
			trail := 0
			for trail < len(p.samples) && math.Abs(float64(p.samples[len(p.samples)-1-trail])) < quiet {
				trail++
			}
			if lead == len(p.samples) {
				t.Fatalf("%s: the whole file is silent", path)
			}
			if d := p.ms(lead); d > maxEdge {
				t.Errorf("%s: leading silence %v exceeds %v", path, d, maxEdge)
			}
			if d := p.ms(trail); d > maxEdge {
				t.Errorf("%s: trailing silence %v exceeds %v", path, d, maxEdge)
			}
		})
	}
}

// The switch and the Go side map a call's language to a directory with one
// fixed rule: zh to zh, anything else to en. A third directory would never be
// played, and a missing one would leave that language without a prompt.
func TestLanguageDirectoriesMatchTheFixedMap(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)
	if want := []string{"en", "zh"}; !reflect.DeepEqual(dirs, want) {
		t.Errorf("language directories are %v, want exactly %v", dirs, want)
	}
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		// Dotfiles (a Finder .DS_Store) never reach the image; .dockerignore drops them.
		var files []string
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), ".") {
				files = append(files, e.Name())
			}
		}
		if len(files) != 1 || files[0] != promptName {
			t.Errorf("%s/ holds %v, want only %s", d, files, promptName)
		}
	}
}
