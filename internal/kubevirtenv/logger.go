package kubevirtenv

import (
	"fmt"
	"os"
)

// Logger receives diagnostic output. Callers choose CLI printing, Ginkgo By/GinkgoWriter, logr, etc.
type Logger interface {
	Step(msg string)
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	// WriteString writes raw output (e.g. progress dots) without an implicit newline.
	WriteString(s string)
}

// StdLogger prints steps to stdout and warnings to stderr (kubevirt-env CLI default).
type StdLogger struct{}

func (StdLogger) Step(msg string) { fmt.Println(msg) }

func (StdLogger) Infof(format string, args ...any) { fmt.Printf(format+"\n", args...) }

func (StdLogger) Warnf(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }

func (StdLogger) WriteString(s string) { fmt.Print(s) }
