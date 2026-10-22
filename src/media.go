// COPYRIGHT_BEGIN
// MIT License
//
// Copyright (c) 2026 Wizzer Works
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.
// COPYRIGHT_END

package main

// Detection of physical storage volumes (USB, external and internal disks)
// that are mounted while mlw-storage runs.
//
// The gateway runs in a container, so it learns about host mounts through a
// read-only bind mount of the host's media directory (for example /media) with
// rslave propagation: mounts made on the host appear inside that directory and
// in /proc/self/mountinfo. The kernel marks mountinfo with POLLPRI|POLLERR
// whenever the mount table changes, so a single goroutine sleeps in epoll_wait
// and uses no CPU until a mount or unmount happens.
//
// Only mounts below the media directory whose device is a real block device are
// reported; loop, RAM, zram and similar virtual devices are ignored (a
// device-mapper device such as an unlocked LUKS volume counts when it sits on a
// physical disk). A volume is only made available after an administrator
// enables it in the Web UI. The decision is remembered by filesystem UUID.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
)

type mediaConfig struct {
	Dir            string // media directory as seen in the container, e.g. /host/media
	HostDir        string // the same directory on the host, shown to administrators
	StateFile      string // remembered decisions; "" keeps them in memory only
	MountInfo      string // normally /proc/self/mountinfo
	Sys            string // normally /sys
	Udev           string // normally /run/udev (optional, adds label, UUID, model)
	IncludeVirtual bool   // testing aid: also report loop and other virtual devices
}

func (c mediaConfig) withDefaults() mediaConfig {
	c.Dir = strings.TrimRight(c.Dir, "/")
	if c.HostDir == "" {
		c.HostDir = c.Dir
	}
	c.HostDir = strings.TrimRight(c.HostDir, "/")
	if c.MountInfo == "" {
		c.MountInfo = "/proc/self/mountinfo"
	}
	if c.Sys == "" {
		c.Sys = "/sys"
	}
	if c.Udev == "" {
		c.Udev = "/run/udev"
	}
	return c
}

// Drive states shown to the administrator.
const (
	driveNew       = "new"       // detected, waiting for an administrator's decision
	driveAvailable = "available" // enabled by an administrator
	driveIgnored   = "ignored"   // administrator chose not to use it
)

// Drive is one physical volume, mounted now or remembered from before.
type Drive struct {
	Key        string // stable identity: uuid:<fs uuid>, or mp:<mount point>
	Name       string // label, else model, else device name
	Device     string // /dev/sdb1
	MountPoint string // path on the host
	FSType     string
	Bus        string // usb, nvme, ata, mmc, ...
	Model      string
	Removable  bool
	Size       uint64 // bytes of the filesystem, or of the device if unreadable
	Used, Free uint64
	HasUsage   bool
	Mounted    bool
	Manual     bool // added by an administrator rather than detected
	State      string
	Admin      string // who decided
	Decided    time.Time
}

// known is what is remembered about a decision.
type known struct {
	State      string    `json:"state"`
	Name       string    `json:"name"`
	Device     string    `json:"device"`
	MountPoint string    `json:"mount_point"`
	FSType     string    `json:"fs_type"`
	Bus        string    `json:"bus"`
	Model      string    `json:"model"`
	Size       uint64    `json:"size"`
	Manual     bool      `json:"manual,omitempty"`
	Admin      string    `json:"admin"`
	Decided    time.Time `json:"decided"`
}

type mediaStore struct {
	cfg    mediaConfig
	statfs func(path string, st *syscall.Statfs_t) error
	now    func() time.Time

	scans atomic.Int64 // number of mountinfo reads, for tests

	mu      sync.Mutex
	mounted map[string]*Drive // physical volumes mounted right now
	known   map[string]known  // remembered decisions
	version uint64
	subs    map[chan struct{}]struct{}
}

func newMediaStore(cfg mediaConfig) (*mediaStore, error) {
	s := &mediaStore{
		cfg:     cfg.withDefaults(),
		statfs:  syscall.Statfs,
		now:     time.Now,
		mounted: map[string]*Drive{},
		known:   map[string]known{},
		subs:    map[chan struct{}]struct{}{},
	}
	if s.cfg.Dir == "" {
		return nil, errors.New("media directory not set")
	}
	if s.cfg.StateFile != "" {
		b, err := os.ReadFile(s.cfg.StateFile)
		switch {
		case err == nil:
			if err := json.Unmarshal(b, &s.known); err != nil {
				return nil, fmt.Errorf("media state %s: %w", s.cfg.StateFile, err)
			}
		case !errors.Is(err, os.ErrNotExist):
			return nil, err
		}
	}
	return s, nil
}

// ---- mountinfo and sysfs --------------------------------------------------

type mountEntry struct {
	Major, Minor int
	MountPoint   string
	FSType       string
	Source       string
}

// parseMountInfo parses /proc/self/mountinfo.
func parseMountInfo(data []byte) []mountEntry {
	var out []mountEntry
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		left, right, ok := strings.Cut(sc.Text(), " - ")
		if !ok {
			continue
		}
		lf, rf := strings.Fields(left), strings.Fields(right)
		if len(lf) < 6 || len(rf) < 2 {
			continue
		}
		maj, min, ok := strings.Cut(lf[2], ":")
		if !ok {
			continue
		}
		a, err1 := strconv.Atoi(maj)
		b, err2 := strconv.Atoi(min)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, mountEntry{Major: a, Minor: b, MountPoint: unescapeMount(lf[4]), FSType: rf[0], Source: unescapeMount(rf[1])})
	}
	return out
}

// unescapeMount decodes the \040-style octal escapes used in mountinfo.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// physicalRoot returns the sysfs directory of the physical device behind the
// block device major:minor, following device-mapper slaves, or false for loop,
// RAM and other virtual devices.
func (s *mediaStore) physicalRoot(real string, depth int) (string, bool) {
	if !strings.Contains(real, "/devices/virtual/") {
		return real, true
	}
	if depth >= 4 {
		return "", false
	}
	slaves, _ := os.ReadDir(filepath.Join(real, "slaves"))
	for _, e := range slaves {
		r, err := filepath.EvalSymlinks(filepath.Join(real, "slaves", e.Name()))
		if err != nil {
			continue
		}
		if p, ok := s.physicalRoot(r, depth+1); ok {
			return p, true
		}
	}
	return "", false
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// udevProps reads the udev database entry of a block device, if available.
func (s *mediaStore) udevProps(major, minor int) map[string]string {
	props := map[string]string{}
	b, err := os.ReadFile(filepath.Join(s.cfg.Udev, "data", fmt.Sprintf("b%d:%d", major, minor)))
	if err != nil {
		return props
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "E:") {
			if k, v, ok := strings.Cut(line[2:], "="); ok {
				props[k] = v
			}
		}
	}
	return props
}

// describe builds the Drive for a mount, or reports false if it is not a
// physical volume.
func (s *mediaStore) describe(m mountEntry) (*Drive, bool) {
	real, err := filepath.EvalSymlinks(filepath.Join(s.cfg.Sys, "dev", "block", fmt.Sprintf("%d:%d", m.Major, m.Minor)))
	if err != nil {
		return nil, false
	}
	phys, ok := s.physicalRoot(real, 0)
	if !ok && !s.cfg.IncludeVirtual {
		return nil, false
	}
	if !ok {
		phys = real
	}
	disk := phys
	if _, err := os.Stat(filepath.Join(phys, "partition")); err == nil {
		disk = filepath.Dir(phys)
	}
	u := s.udevProps(m.Major, m.Minor)

	d := &Drive{
		Device:    m.Source,
		FSType:    m.FSType,
		Removable: readTrim(filepath.Join(disk, "removable")) == "1",
		Mounted:   true,
	}
	d.MountPoint = s.hostPath(m.MountPoint)
	if v := u["ID_FS_TYPE"]; v != "" {
		d.FSType = v
	}
	d.Bus = u["ID_BUS"]
	if d.Bus == "" {
		switch p := phys; {
		case strings.Contains(p, "/usb"):
			d.Bus = "usb"
		case strings.Contains(p, "/nvme"):
			d.Bus = "nvme"
		case strings.Contains(p, "/mmc"):
			d.Bus = "mmc"
		case strings.Contains(p, "/ata"):
			d.Bus = "ata"
		}
	}
	d.Model = strings.ReplaceAll(u["ID_MODEL"], "_", " ")
	if d.Model == "" {
		d.Model = readTrim(filepath.Join(disk, "device", "model"))
	}
	if sectors, err := strconv.ParseUint(readTrim(filepath.Join(real, "size")), 10, 64); err == nil {
		d.Size = sectors * 512
	}
	label := strings.ReplaceAll(u["ID_FS_LABEL"], "_", " ")
	switch {
	case label != "":
		d.Name = label
	case d.Model != "":
		d.Name = d.Model
	default:
		d.Name = filepath.Base(m.Source)
	}
	if uuid := u["ID_FS_UUID"]; uuid != "" {
		d.Key = "uuid:" + uuid
	} else {
		d.Key = "mp:" + d.MountPoint
	}
	return d, true
}

func (s *mediaStore) hostPath(containerPath string) string {
	return s.cfg.HostDir + strings.TrimPrefix(containerPath, s.cfg.Dir)
}

// scan lists the physical volumes currently mounted below the media directory.
func (s *mediaStore) scan(data []byte) map[string]*Drive {
	out := map[string]*Drive{}
	for _, m := range parseMountInfo(data) {
		if !strings.HasPrefix(m.MountPoint, s.cfg.Dir+"/") {
			continue
		}
		var d *Drive
		if strings.HasPrefix(m.Source, "/dev/") {
			d, _ = s.describe(m)
		}
		if d == nil {
			d = s.manualDrive(m)
		}
		if d == nil {
			continue
		}
		if _, dup := out[d.Key]; dup { // the same volume mounted twice
			continue
		}
		out[d.Key] = d
	}
	return out
}

// ---- state ------------------------------------------------------------------

// update replaces the set of mounted drives and wakes subscribers if it changed.
func (s *mediaStore) update(cur map[string]*Drive) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := len(cur) != len(s.mounted)
	for k, d := range cur {
		old, ok := s.mounted[k]
		switch {
		case !ok:
			changed = true
			log.Printf("media: mounted %s (%s) at %s", d.Name, d.Device, d.MountPoint)
		case old.MountPoint != d.MountPoint || old.Device != d.Device:
			changed = true
		}
	}
	for k, d := range s.mounted {
		if _, ok := cur[k]; !ok {
			changed = true
			log.Printf("media: unmounted %s (%s) from %s", d.Name, d.Device, d.MountPoint)
		}
	}
	s.mounted = cur
	if changed {
		s.notifyLocked()
	}
}

func (s *mediaStore) notifyLocked() {
	s.version++
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default: // a wake-up is already pending
		}
	}
}

// subscribe returns a channel that receives a value after every change.
func (s *mediaStore) subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}

func (s *mediaStore) save() error {
	if s.cfg.StateFile == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.known, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.cfg.StateFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.cfg.StateFile)
}

var errNoSuchDrive = errors.New("that drive is not mounted")

// decide records an administrator's choice for a drive that is mounted now.
func (s *mediaStore) decide(key, state, admin string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.mounted[key]
	if !ok {
		kn, manual := s.known[key]
		if !manual || !kn.Manual { // a manual drive can be switched while unmounted
			return errNoSuchDrive
		}
		kn.State, kn.Admin, kn.Decided = state, admin, s.now().UTC()
		s.known[key] = kn
		if err := s.save(); err != nil {
			return err
		}
		log.Printf("media: %q marked %s by %q", kn.Name, state, admin)
		s.notifyLocked()
		return nil
	}
	s.known[key] = known{State: state, Manual: s.known[key].Manual, Name: d.Name, Device: d.Device, MountPoint: d.MountPoint, FSType: d.FSType,
		Bus: d.Bus, Model: d.Model, Size: d.Size, Admin: admin, Decided: s.now().UTC()}
	if err := s.save(); err != nil {
		return err
	}
	log.Printf("media: %q marked %s by %q", d.Name, state, admin)
	s.notifyLocked()
	return nil
}

// forget removes a remembered decision (and the row of a drive that is gone).
func (s *mediaStore) forget(key, admin string) error {
	s.mu.Lock()
	kn, ok := s.known[key]
	if !ok {
		s.mu.Unlock()
		return errNoSuchDrive
	}
	delete(s.known, key)
	err := s.save()
	if err == nil {
		log.Printf("media: forgot %q by %q", key, admin)
		s.notifyLocked()
	}
	s.mu.Unlock()
	if err == nil && kn.Manual {
		s.rescan() // the drive was only mounted as far as the manual entry went
	}
	return err
}

// inputError is a problem with what the administrator typed, safe to show.
type inputError string

func (e inputError) Error() string { return string(e) }

var (
	fsTypeRE = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,24}$`)
	devRE    = regexp.MustCompile(`^/dev/[A-Za-z0-9._/-]{1,60}$`)
)

// manualDrive describes a mount that detection does not report, for example a
// FUSE or exFAT volume, if an administrator added it by hand.
func (s *mediaStore) manualDrive(m mountEntry) *Drive {
	key := "mp:" + s.hostPath(m.MountPoint)
	s.mu.Lock()
	kn, ok := s.known[key]
	s.mu.Unlock()
	if !ok || !kn.Manual {
		return nil
	}
	d := &Drive{Key: key, Name: kn.Name, Device: kn.Device, FSType: kn.FSType, Bus: kn.Bus, Model: kn.Model,
		MountPoint: s.hostPath(m.MountPoint), Mounted: true, Manual: true}
	if d.Device == "" {
		d.Device = m.Source
	}
	if d.FSType == "" {
		d.FSType = m.FSType
	}
	return d
}

// rescan reads the mount table now, so a change made through the UI shows at once.
func (s *mediaStore) rescan() {
	if b, err := os.ReadFile(s.cfg.MountInfo); err == nil {
		s.scans.Add(1)
		s.update(s.scan(b))
	}
}

// addManual records a volume that is mounted below the media directory but that
// detection did not offer. hostPath is the mount point on the host. Device and
// fsType are optional and default to what the kernel reports for the mount.
func (s *mediaStore) addManual(name, hostPath, device, fsType string, enable bool, admin string) error {
	name = strings.TrimSpace(name)
	switch {
	case name == "" || len(name) > 64 || strings.ContainsFunc(name, unicode.IsControl):
		return inputError("Enter a name of up to 64 characters.")
	case device != "" && (!devRE.MatchString(device) || strings.Contains(device, "..")):
		return inputError("The device must be a path below /dev, such as /dev/sdb1.")
	case fsType != "" && !fsTypeRE.MatchString(fsType):
		return inputError("The file system type may contain only letters, digits and . _ + -")
	}
	hostPath = filepath.Clean(strings.TrimSpace(hostPath))
	if !strings.HasPrefix(hostPath, s.cfg.HostDir+"/") {
		return inputError("The mount point must be a directory below " + s.cfg.HostDir + ".")
	}
	cpath := s.cfg.Dir + strings.TrimPrefix(hostPath, s.cfg.HostDir)
	if real, err := filepath.EvalSymlinks(cpath); err != nil {
		return inputError("That directory does not exist.")
	} else if real != cpath && !strings.HasPrefix(real, s.cfg.Dir+"/") {
		return inputError("The mount point must not lead outside " + s.cfg.HostDir + ".")
	}
	data, err := os.ReadFile(s.cfg.MountInfo)
	if err != nil {
		return err
	}
	var m *mountEntry
	for _, e := range parseMountInfo(data) {
		if e.MountPoint == cpath {
			e := e
			m = &e // the last mount on a path is the visible one
		}
	}
	if m == nil {
		return inputError("Nothing is mounted at that path. Mount the drive first.")
	}
	if device == "" {
		device = m.Source
	}
	if fsType == "" {
		fsType = m.FSType
	}
	key := "mp:" + hostPath

	s.mu.Lock()
	if _, dup := s.known[key]; dup {
		s.mu.Unlock()
		return inputError("That mount point is already configured.")
	}
	for _, d := range s.mounted {
		if d.MountPoint == hostPath {
			s.mu.Unlock()
			return inputError("That drive was detected automatically. Enable it in the list.")
		}
	}
	state := driveIgnored
	if enable {
		state = driveAvailable
	}
	s.known[key] = known{State: state, Manual: true, Name: name, Device: device, MountPoint: hostPath, FSType: fsType,
		Admin: admin, Decided: s.now().UTC()}
	if err := s.save(); err != nil {
		delete(s.known, key)
		s.mu.Unlock()
		return err
	}
	log.Printf("media: %q added manually at %s by %q", name, hostPath, admin)
	s.notifyLocked()
	s.mu.Unlock()
	s.rescan()
	return nil
}

// snapshot returns every drive to show: those mounted now (with their state),
// and enabled drives that are currently absent. Usage is read on demand.
func (s *mediaStore) snapshot() []Drive {
	s.mu.Lock()
	var out []Drive
	for k, d := range s.mounted {
		c := *d
		c.State = driveNew
		if kn, ok := s.known[k]; ok {
			c.State, c.Admin, c.Decided, c.Manual = kn.State, kn.Admin, kn.Decided, kn.Manual
		}
		out = append(out, c)
	}
	for k, kn := range s.known {
		if _, here := s.mounted[k]; here || (kn.State != driveAvailable && !kn.Manual) {
			continue
		}
		out = append(out, Drive{Key: k, Manual: kn.Manual, Name: kn.Name, Device: kn.Device, MountPoint: kn.MountPoint, FSType: kn.FSType,
			Bus: kn.Bus, Model: kn.Model, Size: kn.Size, State: kn.State, Admin: kn.Admin, Decided: kn.Decided})
	}
	s.mu.Unlock()

	for i := range out {
		if !out[i].Mounted {
			continue
		}
		var st syscall.Statfs_t
		cp := s.cfg.Dir + strings.TrimPrefix(out[i].MountPoint, s.cfg.HostDir)
		if err := s.statfs(cp, &st); err == nil && st.Blocks > 0 {
			bs := uint64(st.Bsize)
			out[i].Size, out[i].Used, out[i].Free, out[i].HasUsage = st.Blocks*bs, (st.Blocks-st.Bfree)*bs, st.Bavail*bs, true
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Mounted != out[j].Mounted {
			return out[i].Mounted
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// pending counts mounted drives still waiting for a decision.
func (s *mediaStore) pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k := range s.mounted {
		if _, ok := s.known[k]; !ok {
			n++
		}
	}
	return n
}

// ---- watching ---------------------------------------------------------------

// watch blocks until ctx is cancelled, rescanning whenever the mount table
// changes. It sleeps in epoll_wait on /proc/self/mountinfo (POLLPRI|POLLERR),
// so it costs no CPU while nothing is mounted or unmounted.
func (s *mediaStore) watch(ctx context.Context) error {
	fd, err := syscall.Open(s.cfg.MountInfo, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	ep, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		return err
	}
	defer syscall.Close(ep)
	var pipe [2]int
	if err := syscall.Pipe2(pipe[:], syscall.O_CLOEXEC|syscall.O_NONBLOCK); err != nil {
		return err
	}
	defer syscall.Close(pipe[0])
	defer syscall.Close(pipe[1])

	stop := make(chan struct{})
	defer close(stop)
	go func() { // wake epoll_wait on shutdown
		select {
		case <-ctx.Done():
			_, _ = syscall.Write(pipe[1], []byte{1})
		case <-stop:
		}
	}()

	// Reading the file re-arms the notification, so always read before waiting.
	read := func() error {
		if _, err := syscall.Seek(fd, 0, 0); err != nil {
			return err
		}
		var data []byte
		buf := make([]byte, 64*1024)
		for {
			n, err := syscall.Read(fd, buf)
			if err != nil {
				return err
			}
			if n == 0 {
				break
			}
			data = append(data, buf[:n]...)
		}
		s.scans.Add(1)
		s.update(s.scan(data))
		return nil
	}
	if err := read(); err != nil {
		return err
	}
	if err := syscall.EpollCtl(ep, syscall.EPOLL_CTL_ADD, fd,
		&syscall.EpollEvent{Events: syscall.EPOLLPRI | syscall.EPOLLERR, Fd: int32(fd)}); err != nil {
		return fmt.Errorf("watch %s: %w", s.cfg.MountInfo, err)
	}
	if err := syscall.EpollCtl(ep, syscall.EPOLL_CTL_ADD, pipe[0],
		&syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(pipe[0])}); err != nil {
		return err
	}
	events := make([]syscall.EpollEvent, 2)
	for {
		n, err := syscall.EpollWait(ep, events, -1)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return err
		}
		for _, ev := range events[:n] {
			if int(ev.Fd) == pipe[0] {
				return nil
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		if err := read(); err != nil {
			return err
		}
	}
}
