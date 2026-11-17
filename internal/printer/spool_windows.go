//go:build windows

package printer

import (
	"encoding/binary"
	"fmt"
	"image"
	"math"
	"strconv"
	"syscall"
	"unsafe"
)

// Printing through the Windows spooler with plain GDI calls: no cgo, no
// PDF, no temp files. The badge is rendered at exactly the pixel size of the
// printer's printable area and sent as one 1-bit bitmap.

var (
	winspool               = syscall.NewLazyDLL("winspool.drv")
	gdi32                  = syscall.NewLazyDLL("gdi32.dll")
	procOpenPrinter        = winspool.NewProc("OpenPrinterW")
	procClosePrinter       = winspool.NewProc("ClosePrinter")
	procDocumentProperties = winspool.NewProc("DocumentPropertiesW")
	procEnumPrinters       = winspool.NewProc("EnumPrintersW")
	procGetDefaultPrinter  = winspool.NewProc("GetDefaultPrinterW")
	procCreateDC           = gdi32.NewProc("CreateDCW")
	procDeleteDC           = gdi32.NewProc("DeleteDC")
	procGetDeviceCaps      = gdi32.NewProc("GetDeviceCaps")
	procStartDoc           = gdi32.NewProc("StartDocW")
	procEndDoc             = gdi32.NewProc("EndDoc")
	procAbortDoc           = gdi32.NewProc("AbortDoc")
	procStartPage          = gdi32.NewProc("StartPage")
	procEndPage            = gdi32.NewProc("EndPage")
	procStretchDIBits      = gdi32.NewProc("StretchDIBits")
)

const (
	capHorzRes = 8  // printable width in pixels
	capVertRes = 10 // printable height in pixels
	srcCopy    = 0x00CC0020

	// DEVMODEW field offsets and flags.
	dmFieldsOff, dmPaperSizeOff, dmPaperLengthOff, dmPaperWidthOff = 72, 78, 80, 82
	dmPaperFields                                                  = 0x2 | 0x4 | 0x8 // size, length, width
	dmOutBuffer, dmInBuffer                                        = 2, 8

	// Badges print rotated 180° on Windows, to suit how
	// stock feeds through the station printers. Kept so existing stations
	// print the same way up. Calibration knob: flip if badges come out inverted.
	rotate180 = true
)

type printerInfo4 struct {
	name, server *uint16
	attributes   uint32
}

type docInfo struct {
	size                   int32
	docName, output, dtype *uint16
	fwType                 uint32
}

func utf16PtrToString(p *uint16) string {
	if p == nil {
		return ""
	}
	n := 0
	for *(*uint16)(unsafe.Add(unsafe.Pointer(p), n*2)) != 0 {
		n++
	}
	return syscall.UTF16ToString(unsafe.Slice(p, n))
}

func listPrinters() (names []string, def string) {
	const local, connections = 2, 4
	var needed, count uint32
	procEnumPrinters.Call(local|connections, 0, 4, 0, 0, uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count)))
	if needed > 0 {
		buf := make([]byte, needed)
		ok, _, _ := procEnumPrinters.Call(local|connections, 0, 4, uintptr(unsafe.Pointer(&buf[0])), uintptr(needed),
			uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count)))
		if ok != 0 {
			for _, info := range unsafe.Slice((*printerInfo4)(unsafe.Pointer(&buf[0])), count) {
				names = append(names, utf16PtrToString(info.name))
			}
		}
	}
	var nameBuf [512]uint16
	size := uint32(len(nameBuf))
	if ok, _, _ := procGetDefaultPrinter.Call(uintptr(unsafe.Pointer(&nameBuf[0])), uintptr(unsafe.Pointer(&size))); ok != 0 {
		def = syscall.UTF16ToString(nameBuf[:])
	}
	return names, def
}

// createDC opens a device context for the printer, forcing the paper size
// from the layout so operators don't have to match it in Windows settings.
// Some label drivers only accept their own preset sizes; then this falls
// back to the printer's default page.
func createDC(name *uint16, widthMM, heightMM float64) uintptr {
	driver, _ := syscall.UTF16PtrFromString("WINSPOOL")
	var printer uintptr
	if ok, _, _ := procOpenPrinter.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&printer)), 0); ok != 0 {
		defer procClosePrinter.Call(printer)
		size, _, _ := procDocumentProperties.Call(0, printer, uintptr(unsafe.Pointer(name)), 0, 0, 0)
		if int32(size) > dmPaperWidthOff+2 {
			dm := make([]byte, size)
			got, _, _ := procDocumentProperties.Call(0, printer, uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&dm[0])), 0, dmOutBuffer)
			if int32(got) == 1 {
				le := binary.LittleEndian
				le.PutUint32(dm[dmFieldsOff:], le.Uint32(dm[dmFieldsOff:])|dmPaperFields)
				le.PutUint16(dm[dmPaperSizeOff:], 0) // 0 = use length/width, in tenths of a mm
				le.PutUint16(dm[dmPaperLengthOff:], uint16(math.Round(heightMM*10)))
				le.PutUint16(dm[dmPaperWidthOff:], uint16(math.Round(widthMM*10)))
				procDocumentProperties.Call(0, printer, uintptr(unsafe.Pointer(name)),
					uintptr(unsafe.Pointer(&dm[0])), uintptr(unsafe.Pointer(&dm[0])), dmInBuffer|dmOutBuffer)
				if hdc, _, _ := procCreateDC.Call(uintptr(unsafe.Pointer(driver)), uintptr(unsafe.Pointer(name)), 0, uintptr(unsafe.Pointer(&dm[0]))); hdc != 0 {
					return hdc
				}
			}
		}
	}
	hdc, _, _ := procCreateDC.Call(uintptr(unsafe.Pointer(driver)), uintptr(unsafe.Pointer(name)), 0, 0)
	return hdc
}

// dib packs img as a bottom-up 1-bit DIB (rows padded to 32 bits) plus its
// BITMAPINFO header with a black/white palette.
func dib(img *image.Gray) (bits, info []byte) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	stride := (w + 31) / 32 * 4
	bits = make([]byte, stride*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if img.Pix[y*img.Stride+x] < 128 {
				continue // black = bit 0
			}
			// Bottom-up storage flips rows; rotating 180° flips rows and
			// columns. Together: same row, mirrored column.
			row, col := h-1-y, x
			if rotate180 {
				row, col = y, w-1-x
			}
			bits[row*stride+col/8] |= 0x80 >> (col % 8)
		}
	}
	info = make([]byte, 48)
	le := binary.LittleEndian
	le.PutUint32(info[0:], 40)
	le.PutUint32(info[4:], uint32(w))
	le.PutUint32(info[8:], uint32(h))
	le.PutUint16(info[12:], 1) // planes
	le.PutUint16(info[14:], 1) // bits per pixel
	le.PutUint32(info[20:], uint32(len(bits)))
	le.PutUint32(info[32:], 2)                   // colours used
	copy(info[44:], []byte{0xFF, 0xFF, 0xFF, 0}) // palette[1] = white; palette[0] stays black
	return bits, info
}

func spool(printer string, widthMM, heightMM float64, render func(dpi float64) *image.Gray) (string, error) {
	name, err := syscall.UTF16PtrFromString(printer)
	if err != nil {
		return "", err
	}
	hdc := createDC(name, widthMM, heightMM)
	if hdc == 0 {
		return "", fmt.Errorf("could not open printer %q", printer)
	}
	defer procDeleteDC.Call(hdc)

	pageW, _, _ := procGetDeviceCaps.Call(hdc, capHorzRes)
	pageH, _, _ := procGetDeviceCaps.Call(hdc, capVertRes)
	// Fill the printable area: the resolution that maps the badge onto it 1:1.
	dpi := math.Min(float64(pageW)/(widthMM/25.4), float64(pageH)/(heightMM/25.4))
	if dpi < 72 || dpi > 1200 {
		dpi = 203
	}
	img := render(dpi)
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	bits, info := dib(img)

	docName, _ := syscall.UTF16PtrFromString("PrintDex badge")
	doc := docInfo{docName: docName}
	doc.size = int32(unsafe.Sizeof(doc))
	job, _, _ := procStartDoc.Call(hdc, uintptr(unsafe.Pointer(&doc)))
	if int32(job) <= 0 {
		return "", fmt.Errorf("printer %q refused the job", printer)
	}
	// On any failure cancel the half-sent job, or it sits in the spooler and
	// blocks every later print.
	if ok, _, _ := procStartPage.Call(hdc); int32(ok) <= 0 {
		procAbortDoc.Call(hdc)
		return "", fmt.Errorf("printer %q could not start a page", printer)
	}
	lines, _, _ := procStretchDIBits.Call(hdc,
		uintptr((int(pageW)-w)/2), uintptr((int(pageH)-h)/2), uintptr(w), uintptr(h),
		0, 0, uintptr(w), uintptr(h),
		uintptr(unsafe.Pointer(&bits[0])), uintptr(unsafe.Pointer(&info[0])), 0, srcCopy)
	if int32(lines) <= 0 {
		procAbortDoc.Call(hdc)
		return "", fmt.Errorf("printer %q rejected the badge image", printer)
	}
	procEndPage.Call(hdc)
	if ok, _, _ := procEndDoc.Call(hdc); int32(ok) <= 0 {
		return "", fmt.Errorf("printer %q did not accept the job", printer)
	}
	return strconv.Itoa(int(job)), nil
}
