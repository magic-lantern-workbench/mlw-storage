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

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

const mountInfoSample = `22 28 0:21 / /sys rw,nosuid,nodev,noexec,relatime shared:7 - sysfs sysfs rw
28 1 259:2 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw
60 28 259:1 / /host/media/root-disk rw,relatime - vfat /dev/nvme0n1p1 rw
301 28 8:17 / /host/media/msm/My\040USB rw,nosuid,nodev,relatime shared:200 - vfat /dev/sdb1 rw
302 28 8:17 / /host/media/msm/My\040USB-again rw,relatime - vfat /dev/sdb1 rw
303 28 7:3 / /host/media/msm/looped rw,relatime - ext4 /dev/loop3 rw
304 28 0:55 / /host/media/msm/share rw,relatime - nfs4 server:/export rw
305 28 8:33 / /elsewhere/disk rw,relatime - ext4 /dev/sdc1 rw
306 28 253:0 / /host/media/msm/crypt rw,relatime - ext4 /dev/mapper/luks-1 rw
`

func TestParseMountInfo(t *testing.T) {
	got := parseMountInfo([]byte(mountInfoSample))
	if len(got) != 9 {
		t.Fatalf("parsed %d entries, want 9", len(got))
	}
	m := got[3]
	if m.Major != 8 || m.Minor != 17 || m.MountPoint != "/host/media/msm/My USB" || m.FSType != "vfat" || m.Source != "/dev/sdb1" {
		t.Errorf("entry = %+v", m)
	}
	if got := parseMountInfo([]byte("garbage\n\n12 3 x\n")); len(got) != 0 {
		t.Errorf("garbage parsed as %v", got)
	}
}

func TestUnescapeMount(t *testing.T) {
	for in, want := range map[string]string{`a\040b`: "a b", `plain`: "plain", `x\134y`: `x\y`, `end\04`: `end\04`, `\`: `\`} {
		if got := unescapeMount(in); got != want {
			t.Errorf("unescapeMount(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeSys builds a minimal sysfs and udev tree and returns a config using it.
type fakeSys struct{ root string }

func (f fakeSys) write(t *testing.T, rel, content string) {
	t.Helper()
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f fakeSys) link(t *testing.T, rel, target string) {
	t.Helper()
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

func newFakeMedia(t *testing.T) (*mediaStore, fakeSys) {
	t.Helper()
	f := fakeSys{t.TempDir()}
	const usb = "sys/devices/pci0000:00/0000:00:14.0/usb1/1-2/1-2:1.0/host6/target6:0:0/6:0:0:0/block/sdb"
	f.write(t, usb+"/removable", "1\n")
	f.write(t, usb+"/device/model", "Portable SSD \n")
	f.write(t, usb+"/sdb1/partition", "1\n")
	f.write(t, usb+"/sdb1/size", "2048\n")
	f.link(t, "sys/dev/block/8:17", "../../devices/pci0000:00/0000:00:14.0/usb1/1-2/1-2:1.0/host6/target6:0:0/6:0:0:0/block/sdb/sdb1")
	// an internal NVMe partition
	const nvme = "sys/devices/pci0000:00/0000:00:1d.0/0000:04:00.0/nvme/nvme0/nvme0n1"
	f.write(t, nvme+"/removable", "0\n")
	f.write(t, nvme+"/nvme0n1p1/partition", "1\n")
	f.write(t, nvme+"/nvme0n1p1/size", "1024\n")
	f.link(t, "sys/dev/block/259:1", "../../devices/pci0000:00/0000:00:1d.0/0000:04:00.0/nvme/nvme0/nvme0n1/nvme0n1p1")
	// a loop device (virtual)
	f.write(t, "sys/devices/virtual/block/loop3/size", "100\n")
	f.link(t, "sys/dev/block/7:3", "../../devices/virtual/block/loop3")
	// a device-mapper device on top of sdb1 (unlocked LUKS) and one with no slaves
	f.write(t, "sys/devices/virtual/block/dm-0/size", "1500\n")
	f.link(t, "sys/devices/virtual/block/dm-0/slaves/sdb1", "../../../../pci0000:00/0000:00:14.0/usb1/1-2/1-2:1.0/host6/target6:0:0/6:0:0:0/block/sdb/sdb1")
	f.link(t, "sys/dev/block/253:0", "../../devices/virtual/block/dm-0")
	// udev data for the USB partition
	f.write(t, "udev/data/b8:17", "S:disk/by-uuid/ABCD-1234\nE:ID_FS_UUID=ABCD-1234\nE:ID_FS_LABEL=My_USB\nE:ID_FS_TYPE=vfat\nE:ID_BUS=usb\nE:ID_MODEL=Portable_SSD\n")

	s, err := newMediaStore(mediaConfig{
		Dir: "/host/media", HostDir: "/media",
		MountInfo: filepath.Join(f.root, "mountinfo"),
		Sys:       filepath.Join(f.root, "sys"), Udev: filepath.Join(f.root, "udev"),
		StateFile: filepath.Join(f.root, "media.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.statfs = func(string, *syscall.Statfs_t) error { return syscall.EACCES }
	return s, f
}

func TestScanFindsOnlyPhysicalVolumesUnderMediaDir(t *testing.T) {
	s, _ := newFakeMedia(t)
	got := s.scan([]byte(mountInfoSample))

	usb := got["uuid:ABCD-1234"]
	if usb == nil {
		t.Fatalf("USB volume not found in %v", keys(got))
	}
	if usb.Name != "My USB" || usb.Device != "/dev/sdb1" || usb.MountPoint != "/media/msm/My USB" || usb.FSType != "vfat" ||
		usb.Bus != "usb" || usb.Model != "Portable SSD" || !usb.Removable || usb.Size != 2048*512 || !usb.Mounted {
		t.Errorf("usb = %+v", usb)
	}
	// The internal partition mounted below the media dir has no UUID: keyed by mount point.
	if d := got["mp:/media/root-disk"]; d == nil || d.Bus != "nvme" || d.Removable {
		t.Errorf("internal disk = %+v", d)
	}
	// Not reported: the duplicate mount of the same volume, the loop device,
	// the NFS share, a physical disk outside the media dir, "/" itself.
	// The LUKS volume on top of sdb1 is reported through its physical slave.
	for k, d := range got {
		switch {
		case strings.Contains(d.MountPoint, "looped"), strings.Contains(d.MountPoint, "share"), strings.Contains(d.MountPoint, "elsewhere"), d.MountPoint == "/":
			t.Errorf("unexpected drive %s: %+v", k, d)
		}
	}
	if len(got) != 3 {
		t.Errorf("got %d drives %v, want 3 (usb, internal, luks)", len(got), keys(got))
	}
}

func keys(m map[string]*Drive) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	return k
}

func TestScanIncludeVirtual(t *testing.T) {
	s, _ := newFakeMedia(t)
	s.cfg.IncludeVirtual = true
	var loop *Drive
	for _, d := range s.scan([]byte(mountInfoSample)) {
		if strings.HasSuffix(d.MountPoint, "looped") {
			loop = d
		}
	}
	if loop == nil {
		t.Error("virtual device not reported with IncludeVirtual")
	}
}

func TestDecisionsLifecycleAndPersistence(t *testing.T) {
	s, f := newFakeMedia(t)
	s.update(s.scan([]byte(mountInfoSample)))
	if s.pending() != 3 {
		t.Fatalf("pending = %d, want 3", s.pending())
	}
	ch, cancel := s.subscribe()
	defer cancel()

	if err := s.decide("uuid:NOPE", driveAvailable, "admin"); err != errNoSuchDrive {
		t.Fatalf("decide on unknown drive: %v", err)
	}
	if err := s.decide("uuid:ABCD-1234", driveAvailable, "admin"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	default:
		t.Error("subscriber not woken by a decision")
	}
	if err := s.decide("mp:/media/root-disk", driveIgnored, "admin"); err != nil {
		t.Fatal(err)
	}
	if s.pending() != 1 {
		t.Errorf("pending = %d, want 1 (the LUKS volume)", s.pending())
	}
	states := map[string]string{}
	for _, d := range s.snapshot() {
		states[d.Key] = d.State
	}
	if states["uuid:ABCD-1234"] != driveAvailable || states["mp:/media/root-disk"] != driveIgnored {
		t.Errorf("states = %v", states)
	}

	// Unmounting: the enabled drive stays listed as not mounted; the ignored one disappears.
	s.update(map[string]*Drive{})
	var rows []Drive
	for _, d := range s.snapshot() {
		rows = append(rows, d)
	}
	if len(rows) != 1 || rows[0].Key != "uuid:ABCD-1234" || rows[0].Mounted || rows[0].State != driveAvailable || rows[0].Name != "My USB" {
		t.Fatalf("after unmount: %+v", rows)
	}

	// The decision survives a restart, and plugging the drive in again makes it available at once.
	s2, err := newMediaStore(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	s2.statfs = s.statfs
	s2.update(s2.scan([]byte(mountInfoSample)))
	for _, d := range s2.snapshot() {
		if d.Key == "uuid:ABCD-1234" && (d.State != driveAvailable || !d.Mounted) {
			t.Errorf("after restart: %+v", d)
		}
	}
	if err := s2.forget("uuid:ABCD-1234", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := s2.forget("uuid:ABCD-1234", "admin"); err != errNoSuchDrive {
		t.Errorf("second forget: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(f.root, "media.json")); strings.Contains(string(b), "ABCD-1234") {
		t.Errorf("forgotten drive still in state file: %s", b)
	}
}

func TestUsageFromStatfs(t *testing.T) {
	s, _ := newFakeMedia(t)
	s.statfs = func(path string, st *syscall.Statfs_t) error {
		if path != "/host/media/msm/My USB" {
			return syscall.EACCES // only the USB volume has readable usage; also checks the container path is used
		}
		st.Bsize, st.Blocks, st.Bfree, st.Bavail = 4096, 1000, 400, 350
		return nil
	}
	s.update(s.scan([]byte(mountInfoSample)))
	for _, d := range s.snapshot() {
		if d.Key == "uuid:ABCD-1234" {
			if !d.HasUsage || d.Size != 1000*4096 || d.Used != 600*4096 || d.Free != 350*4096 {
				t.Errorf("usage = %+v", d)
			}
			return
		}
	}
	t.Fatal("usb drive missing")
}

func TestUpdateOnlyNotifiesOnChange(t *testing.T) {
	s, _ := newFakeMedia(t)
	ch, cancel := s.subscribe()
	defer cancel()
	cur := s.scan([]byte(mountInfoSample))
	s.update(cur)
	<-ch
	s.update(s.scan([]byte(mountInfoSample)))
	select {
	case <-ch:
		t.Error("woken without a change")
	default:
	}
}

func TestStateFileCorrupt(t *testing.T) {
	f := t.TempDir()
	p := filepath.Join(f, "media.json")
	_ = os.WriteFile(p, []byte("{not json"), 0o600)
	if _, err := newMediaStore(mediaConfig{Dir: "/m", StateFile: p}); err == nil {
		t.Error("corrupt state file accepted")
	}
}

// TestWatchWakesOnMountChange mounts a tmpfs in a private mount namespace and
// checks that watch, sleeping in epoll_wait on that namespace's mountinfo, wakes
// up and rescans. It needs root (CAP_SYS_ADMIN) and is skipped otherwise.
func TestWatchWakesOnMountChange(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root")
	}
	s, _ := newFakeMedia(t)
	target := t.TempDir()
	ready, done := make(chan string), make(chan struct{})
	mountNow := make(chan struct{})
	go func() { // a thread with its own mount namespace
		runtime.LockOSThread()
		defer close(done)
		if err := syscall.Unshare(syscall.CLONE_NEWNS); err != nil {
			ready <- ""
			return
		}
		ready <- fmt.Sprintf("/proc/%d/task/%d/mountinfo", os.Getpid(), syscall.Gettid())
		<-mountNow
		if err := syscall.Mount("tmpfs", target, "tmpfs", 0, ""); err != nil {
			t.Errorf("mount: %v", err)
			return
		}
		_ = syscall.Unmount(target, 0)
	}()
	path := <-ready
	if path == "" {
		t.Skip("cannot create a mount namespace")
	}
	s.cfg.MountInfo = path

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- s.watch(ctx) }()
	waitScans := func(n int64) {
		t.Helper()
		for deadline := time.Now().Add(3 * time.Second); s.scans.Load() < n; time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("scans = %d, want at least %d", s.scans.Load(), n)
			}
		}
	}
	waitScans(1)
	time.Sleep(100 * time.Millisecond)
	if got := s.scans.Load(); got != 1 {
		t.Fatalf("watch rescanned %d times with no mount change", got)
	}
	close(mountNow) // mount and unmount: at least one wake-up
	<-done
	waitScans(2)
	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("watch: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watch did not stop on cancel")
	}
}

// TestWatchStopsOnCancel runs watch on the real mountinfo, which proves the
// epoll registration works there, and checks it blocks until cancelled.
func TestWatchStopsOnCancel(t *testing.T) {
	s, _ := newFakeMedia(t)
	s.cfg.MountInfo = "/proc/self/mountinfo"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.watch(ctx) }()
	for deadline := time.Now().Add(3 * time.Second); s.scans.Load() == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no initial scan")
		}
	}
	select {
	case err := <-done:
		t.Fatalf("watch returned early: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("watch: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watch did not stop on cancel")
	}
}
