//go:build !windows

package printer

import (
	"bytes"
	"fmt"
	"github.com/Sienz16/printdex/internal/badge"
	"image"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

var (
	lpstatPrinter = regexp.MustCompile(`(?m)^printer (\S+)`)
	lpstatDefault = regexp.MustCompile(`system default destination: (\S+)`)
	lpJobID       = regexp.MustCompile(`request id is (\S+)`)
)

// printDPI is the printer's native resolution. CUPS doesn't report it
// reliably, so it's a knob: 203 suits the common thermal label printers,
// set PRINT_DPI=300 for a 300 dpi head.
func printDPI() float64 {
	if dpi, err := strconv.ParseFloat(os.Getenv("PRINT_DPI"), 64); err == nil && dpi >= 72 && dpi <= 1200 {
		return dpi
	}
	return 203
}

func listPrinters() (names []string, def string) {
	if out, err := exec.Command("lpstat", "-p").Output(); err == nil {
		for _, m := range lpstatPrinter.FindAllStringSubmatch(string(out), -1) {
			names = append(names, m[1])
		}
	}
	if out, err := exec.Command("lpstat", "-d").Output(); err == nil {
		if m := lpstatDefault.FindStringSubmatch(string(out)); m != nil {
			def = m[1]
		}
	}
	return names, def
}

// printImage renders at the printer's resolution and hands CUPS a 1-bit PNG
// on stdin: no temp files, and nothing left for the driver to dither.
func spool(printer string, _, _ float64, render func(dpi float64) *image.Gray) (string, error) {
	dpi := printDPI()
	cmd := exec.Command("lp", "-d", printer, "-o", "Darkness=15")
	cmd.Stdin = bytes.NewReader(badge.PNG(render(dpi), dpi))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("lp: %s", strings.TrimSpace(string(out)+" "+err.Error()))
	}
	if m := lpJobID.FindStringSubmatch(string(out)); m != nil {
		return m[1], nil
	}
	return strings.TrimSpace(string(out)), nil
}
