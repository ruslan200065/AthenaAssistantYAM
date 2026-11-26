// Command athena bridges a Yandex Station to Athena Core via Luna.
package main

import (
	"fmt"
	"os"
	"runtime/debug"
)

const usage = `usage:
  athena run [flags]          station daemon: detect "Афина", recognize, ask Athena
  athena train [flags]        train a word from WAV files
  athena scan model.json... audio.wav
`

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"run"}
	}
	// Minis have ~120 MB free next to maind; training holds ~45 MB live, so
	// keep the GC from letting the heap double on top of that.
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(64 << 20)
	}
	var err error
	switch args[0] {
	case "run":
		err = cmdRun(args[1:])
	case "train":
		err = cmdTrain(args[1:])
	case "scan":
		err = cmdScan(args[1:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "athena:", err)
		os.Exit(1)
	}
}
