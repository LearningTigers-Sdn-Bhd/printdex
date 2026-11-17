// Package printer finds the station's printer and sends badge bitmaps to it.
//
// The platform files provide listPrinters and spool: GDI on Windows, CUPS
// elsewhere.
package printer

import (
	"errors"
	"image"
	"os"
	"strings"
	"sync"
)

// ErrNoPrinter means no printer is installed or connected.
var ErrNoPrinter = errors.New("no printer found; connect a printer and try again")

// Station is the printer this PC prints badges on.
type Station struct {
	mu   sync.Mutex
	name string

	jobs sync.Mutex // one job at a time, so badges come out in scan order
}

// New picks the station's printer.
func New() *Station {
	s := &Station{}
	s.Refresh()
	return s
}

// Refresh re-reads the installed printers and picks one: PRINTER_NAME if
// set, else the only printer, else the system default, else the first.
func (s *Station) Refresh() (installed []string, systemDefault string) {
	installed, systemDefault = listPrinters()
	if installed == nil {
		installed = []string{}
	}
	name := strings.TrimSpace(os.Getenv("PRINTER_NAME"))
	switch {
	case name != "":
	case len(installed) == 1:
		name = installed[0]
	case systemDefault != "":
		name = systemDefault
	case len(installed) > 0:
		name = installed[0]
	}
	s.mu.Lock()
	s.name = name
	s.mu.Unlock()
	return installed, systemDefault
}

// Name is the chosen printer, "" if there is none. A printer plugged in
// after start-up is picked up here.
func (s *Station) Name() string {
	if name := s.current(); name != "" {
		return name
	}
	s.Refresh()
	return s.current()
}

func (s *Station) current() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.name
}

// Print sends one page of the given paper size to the printer. render is
// called with the resolution to draw at, so the bitmap maps onto the
// printer's dots one to one. It returns the spooler's job id.
func (s *Station) Print(widthMM, heightMM float64, render func(dpi float64) *image.Gray) (jobID string, err error) {
	name := s.Name()
	if name == "" {
		return "", ErrNoPrinter
	}
	s.jobs.Lock()
	defer s.jobs.Unlock()
	return spool(name, widthMM, heightMM, render)
}
