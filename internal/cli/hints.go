package cli

import (
	"fmt"
	"strings"

	"github.com/pilef-co/bqto/ext"
)

func readyForward(localPort int, host string, port int, instanceRef, vmName, expiry string) string {
	target := fmt.Sprintf("%s:%d", host, port)
	name := ""
	if instanceRef != "" && instanceRef != target {
		name = fmt.Sprintf(" (instance %s)", instanceRef)
	}
	return fmt.Sprintf("ready: 127.0.0.1:%d → %s%s, bastion %s, %s", localPort, target, name, vmName, expiry)
}

func readyLines(t target, ready ext.Ready, vmName, expiry string, inStack bool) []string {
	head := func(s string) string {
		if inStack {
			return "  " + strings.ReplaceAll(s, "\n", "\n  ")
		}
		return s
	}
	bastionPart := fmt.Sprintf(", bastion %s, %s", vmName, expiry)
	if inStack {
		bastionPart = ""
	}

	var lines []string
	switch {
	case t.fwd.RemoteHost == "":
		lines = append(lines, head(fmt.Sprintf("ready: SOCKS5 proxy 127.0.0.1:%d → %s%s", ready.LocalPort, t.spec.Summary, bastionPart)))
	case inStack:
		lines = append(lines, head(fmt.Sprintf("ready: 127.0.0.1:%d → %s:%d (%s %s)", ready.LocalPort, t.fwd.RemoteHost, t.fwd.RemotePort, t.service, t.instanceRef)))
	default:
		lines = append(lines, head(readyForward(ready.LocalPort, t.fwd.RemoteHost, t.fwd.RemotePort, t.instanceRef, vmName, expiry)))
	}
	if t.spec.Hints != nil {
		for _, h := range t.spec.Hints(ready) {
			lines = append(lines, head(h))
		}
	}
	for _, a := range ready.Started.After {
		lines = append(lines, head(a))
	}
	return lines
}
