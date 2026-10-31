package ext

import (
	"fmt"
	"io"
	"os"
	"sync"
)

var (
	debugMu      sync.Mutex
	debugOut     io.Writer
	progressOut  io.Writer = os.Stderr
	progressOpen bool
	progressCut  bool
)

func SetDebug(w io.Writer) {
	debugMu.Lock()
	defer debugMu.Unlock()
	debugOut = w
}

func Debugging() bool {
	debugMu.Lock()
	defer debugMu.Unlock()
	return debugOut != nil
}

func Debugf(format string, args ...any) {
	debugMu.Lock()
	defer debugMu.Unlock()
	if debugOut == nil {
		return
	}
	cutProgress()
	fmt.Fprintf(debugOut, "debug: "+format+"\n", args...)
}

func Warnf(format string, args ...any) {
	debugMu.Lock()
	defer debugMu.Unlock()
	cutProgress()
	fmt.Fprintf(progressOut, "warn: "+format+"\n", args...)
}

func StartProgress(msg string) {
	debugMu.Lock()
	defer debugMu.Unlock()
	fmt.Fprint(progressOut, msg)
	progressOpen = true
	progressCut = false
}

func EndProgress(msg string) {
	debugMu.Lock()
	defer debugMu.Unlock()
	if !progressCut || msg != "" {
		fmt.Fprintln(progressOut, msg)
	}
	progressOpen = false
	progressCut = false
}

func cutProgress() {
	if progressOpen {
		fmt.Fprintln(progressOut)
		progressOpen = false
		progressCut = true
	}
}
