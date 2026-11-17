// Package update keeps an installed copy current, the way RfiDex does: each
// release publishes a latest.json naming a signed build per platform. The app
// downloads its build, verifies the signature against the public key baked in
// at build time, swaps its own executable and relaunches.
//
// No installer runs, so an update takes seconds and needs no admin rights
// (the app is installed per-user).
package update

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const repo = "Sienz16/printdex"

var (
	releasePage = "https://github.com/" + repo + "/releases/latest"
	manifestURL = releasePage + "/download/latest.json"
	assetPrefix = "https://github.com/" + repo + "/releases/download/" // builds may only come from here
)

const maxDownload = 500 << 20

// Status is the updater's state, polled by the dashboard.
type Status struct {
	Phase           string `json:"phase"` // idle, checking, downloading, ready, applying, error
	Progress        int    `json:"progress"`
	Detail          string `json:"detail"`
	Latest          string `json:"latest"`
	UpdateAvailable bool   `json:"update_available"`
	ReleaseURL      string `json:"release_url"`
	Error           string `json:"error"`
	CurrentVersion  string `json:"current_version"`
	UpdateSupported bool   `json:"update_supported"`
}

// manifest is a release's latest.json.
type manifest struct {
	Version   string           `json:"version"`
	Platforms map[string]build `json:"platforms"` // keyed "<os>-<arch>"
}

type build struct {
	URL       string `json:"url"`
	Signature string `json:"signature"` // base64 ed25519 signature of the file
}

type Updater struct {
	version   string
	publicKey string // base64 ed25519 key; empty disables self-update

	mu     sync.Mutex
	status Status
	build  build // the newer build found by Check
}

func New(version, publicKey string) *Updater {
	return &Updater{version: version, publicKey: publicKey, status: Status{
		Phase:           "idle",
		CurrentVersion:  version,
		UpdateSupported: publicKey != "" && version != "dev", // dev and unkeyed builds stay put
	}}
}

func (u *Updater) Status() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.status
}

// set changes the status under the lock and returns the result.
func (u *Updater) set(change func(*Status)) Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	change(&u.status)
	return u.status
}

func (u *Updater) fail(message string) Status {
	return u.set(func(s *Status) { s.Phase, s.Error, s.Progress = "error", message, 0 })
}

// Check asks the release page whether a newer build exists for this platform.
func (u *Updater) Check() Status {
	u.set(func(s *Status) { s.Phase, s.Error = "checking", "" })
	release, err := fetchManifest()
	if err != nil {
		return u.fail("Couldn't check for updates: " + err.Error())
	}
	found, ok := release.Platforms[runtime.GOOS+"-"+runtime.GOARCH]
	return u.set(func(s *Status) {
		u.build = found
		s.Phase, s.Latest, s.ReleaseURL = "idle", release.Version, releasePage
		s.UpdateAvailable = ok && IsNewer(release.Version, u.version)
	})
}

func fetchManifest() (manifest, error) {
	var m manifest
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Get(manifestURL)
	if err != nil {
		return m, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return m, fmt.Errorf("release server answered %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m); err != nil || m.Version == "" {
		return m, errors.New("release information is not readable")
	}
	return m, nil
}

var versionNumber = regexp.MustCompile(`\d+(?:\.\d+)*`)

// IsNewer compares dotted versions numerically, so "v1.10.0" beats "1.9.3".
// A version with no number in it (a "dev" build) is older than any release.
func IsNewer(latest, current string) bool {
	parts := func(v string) []int {
		var nums []int
		for _, p := range strings.Split(versionNumber.FindString(v), ".") {
			n, _ := strconv.Atoi(p)
			nums = append(nums, n)
		}
		return nums
	}
	a, b := parts(latest), parts(current)
	for i := 0; i < max(len(a), len(b)); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// Download fetches and verifies the build found by Check, in the background.
// Poll Status for progress; the phase ends at "ready" or "error".
func (u *Updater) Download() Status {
	u.mu.Lock()
	status, found := u.status, u.build
	u.mu.Unlock()
	switch {
	case status.Phase == "downloading" || status.Phase == "ready":
		return status
	case !status.UpdateSupported:
		return u.fail("This build can't update itself. Install the latest release instead.")
	case !status.UpdateAvailable || !strings.HasPrefix(found.URL, assetPrefix):
		return u.fail("No update to download. Check for updates first.")
	}
	started := u.set(func(s *Status) { s.Phase, s.Progress, s.Error = "downloading", 0, "" })
	go func() {
		if err := u.download(found); err != nil {
			u.fail("Update failed: " + err.Error())
			return
		}
		u.set(func(s *Status) { s.Phase, s.Progress, s.Detail = "ready", 100, "Restart to finish updating." })
	}()
	return started
}

// download keeps the new build in memory until its signature checks out, and
// only then writes it beside the running executable as "<exe>.new".
func (u *Updater) download(b build) error {
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Get(b.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download answered %d", resp.StatusCode)
	}
	var data bytes.Buffer
	chunk := make([]byte, 256<<10)
	for {
		n, err := resp.Body.Read(chunk)
		data.Write(chunk[:n])
		if resp.ContentLength > 0 {
			u.set(func(s *Status) { s.Progress = int(int64(data.Len()) * 100 / resp.ContentLength) })
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if data.Len() > maxDownload {
			return errors.New("download is too large")
		}
	}
	if err := Verify(u.publicKey, data.Bytes(), b.Signature); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return os.WriteFile(exe+".new", data.Bytes(), 0o755)
}

// Apply swaps the downloaded build in and relaunches; this process exits.
// A running executable can be renamed on Windows and macOS, just not
// overwritten, so the old build is moved aside and deleted on next start.
func (u *Updater) Apply() Status {
	if u.Status().Phase != "ready" {
		return u.Status()
	}
	if err := swapExecutable(); err != nil {
		return u.fail("Couldn't install the update: " + err.Error())
	}
	go func() {
		time.Sleep(400 * time.Millisecond) // let the dashboard get its answer first
		if err := Relaunch(); err != nil {
			u.fail("Updated, but couldn't restart. Close and reopen PrintDex.")
			return
		}
		os.Exit(0)
	}()
	return u.set(func(s *Status) { s.Phase = "applying" })
}

func swapExecutable() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	_ = os.Remove(exe + ".old")
	if err := os.Rename(exe, exe+".old"); err != nil {
		return err
	}
	if err := os.Rename(exe+".new", exe); err != nil {
		_ = os.Rename(exe+".old", exe) // put the working build back
		return err
	}
	return nil
}

// CleanUp removes what an earlier update left beside the executable.
func CleanUp() {
	if exe, err := os.Executable(); err == nil {
		_ = os.Remove(exe + ".old")
		_ = os.Remove(exe + ".new")
	}
}

// Relaunch starts a fresh copy of this program. The caller then exits.
func Relaunch() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return exec.Command(exe, os.Args[1:]...).Start()
}

// --- signing ---

// Verify checks data against a base64 ed25519 signature and public key.
func Verify(publicKey string, data []byte, signature string) error {
	key, keyErr := base64.StdEncoding.DecodeString(publicKey)
	sig, sigErr := base64.StdEncoding.DecodeString(strings.TrimSpace(signature))
	if keyErr != nil || sigErr != nil || len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, data, sig) {
		return errors.New("the download's signature is not valid")
	}
	return nil
}

// GenerateKey makes a new signing key pair, both base64.
func GenerateKey() (publicKey, privateKey string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	return base64.StdEncoding.EncodeToString(pub), base64.StdEncoding.EncodeToString(priv), err
}

// Sign returns the base64 signature of a file, made with a base64 private key.
func Sign(privateKey, path string) (string, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(privateKey))
	if err != nil || len(key) != ed25519.PrivateKeySize {
		return "", errors.New("the private key is missing or not valid")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ed25519.Sign(key, data)), nil
}
