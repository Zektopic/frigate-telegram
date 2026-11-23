package log

import (
	"io"
	"log"
	"os"

	"github.com/oldtyt/frigate-telegram/internal/config"
)

var (
	Warn  *log.Logger
	Debug *log.Logger
	Info  *log.Logger
	Error *log.Logger
	Trace *log.Logger
)

func LogFunc() {
	conf := config.New()
	Trace = log.New(io.Discard, "TRACE: ", log.Ldate|log.Ltime|log.Lshortfile)
	// Debug messages are dropped unless DEBUG is set. A logger writing to
	// io.Discard returns before formatting the message or looking up the
	// caller's file and line, which /dev/null would still do for every line.
	debugOut := io.Discard
	if conf.Debug {
		debugOut = os.Stdout
	}
	Debug = log.New(debugOut, "DEBUG: ", log.Ldate|log.Ltime|log.Lshortfile)
	Info = log.New(os.Stdout, "INFO: ", log.Ldate|log.Ltime|log.Lshortfile)
	Warn = log.New(os.Stdout, "WARNING: ", log.Ldate|log.Ltime|log.Lshortfile)
	Error = log.New(os.Stderr, "ERROR: ", log.Ldate|log.Ltime|log.Lshortfile)
}
