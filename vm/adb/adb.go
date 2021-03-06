// Copyright 2015 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package adb

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/google/syzkaller/vm"
)

func init() {
	vm.Register("adb", ctor)
}

type instance struct {
	cfg     *vm.Config
	console string
	closed  chan bool
}

func ctor(cfg *vm.Config) (vm.Instance, error) {
	inst := &instance{
		cfg:    cfg,
		closed: make(chan bool),
	}
	closeInst := inst
	defer func() {
		if closeInst != nil {
			closeInst.Close()
		}
	}()
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if err := inst.findConsole(); err != nil {
		return nil, err
	}
	if err := inst.repair(); err != nil {
		return nil, err
	}
	// Remove temp files from previous runs.
	inst.adb("shell", "rm -Rf /data/syzkaller*")
	closeInst = nil
	return inst, nil
}

func validateConfig(cfg *vm.Config) error {
	if cfg.Bin == "" {
		cfg.Bin = "adb"
	}
	if !regexp.MustCompile("[0-9A-F]+").MatchString(cfg.Device) {
		return fmt.Errorf("invalid adb device id '%v'", cfg.Device)
	}
	return nil
}

var (
	consoleCacheMu sync.Mutex
	consoleCache   = make(map[string]string)
)

func (inst *instance) findConsole() error {
	// Case Closed Debugging using Suzy-Q:
	// https://chromium.googlesource.com/chromiumos/platform/ec/+/master/docs/case_closed_debugging.md
	consoleCacheMu.Lock()
	defer consoleCacheMu.Unlock()
	if inst.console = consoleCache[inst.cfg.Device]; inst.console != "" {
		return nil
	}
	out, err := exec.Command(inst.cfg.Bin, "devices", "-l").CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to execute 'adb devices -l': %v\n%v\n", err, string(out))
	}
	re := regexp.MustCompile(fmt.Sprintf("%v +device usb:([0-9]+)-([0-9]+)\\.([0-9]+) ", inst.cfg.Device))
	match := re.FindAllStringSubmatch(string(out), 1)
	if match == nil {
		return fmt.Errorf("can't find adb device '%v' in 'adb devices' output:\n%v\n", inst.cfg.Device, string(out))
	}
	bus, _ := strconv.ParseUint(match[0][1], 10, 64)
	port, _ := strconv.ParseUint(match[0][2], 10, 64)
	files, err := filepath.Glob(fmt.Sprintf("/sys/bus/usb/devices/%v-%v.2:1.1/ttyUSB*", bus, port))
	if err != nil || len(files) == 0 {
		return fmt.Errorf("can't find any ttyUDB devices for adb device '%v' on bus %v-%v", inst.cfg.Device, bus, port)
	}
	inst.console = "/dev/" + filepath.Base(files[0])
	consoleCache[inst.cfg.Device] = inst.console
	log.Printf("associating adb device %v with console %v", inst.cfg.Device, inst.console)
	return nil
}

func (inst *instance) Forward(port int) (string, error) {
	// If 35099 turns out to be busy, try to forward random ports several times.
	devicePort := 35099
	if err := inst.adb("reverse", fmt.Sprintf("tcp:%v", devicePort), fmt.Sprintf("tcp:%v", port)); err != nil {
		return "", err
	}
	return fmt.Sprintf("127.0.0.1:%v", devicePort), nil
}

func (inst *instance) adb(args ...string) error {
	if inst.cfg.Debug {
		log.Printf("executing adb %+v", args)
	}
	rpipe, wpipe, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("failed to create pipe: %v", err)
	}
	defer wpipe.Close()
	defer rpipe.Close()
	cmd := exec.Command(inst.cfg.Bin, append([]string{"-s", inst.cfg.Device}, args...)...)
	cmd.Stdout = wpipe
	cmd.Stderr = wpipe
	if err := cmd.Start(); err != nil {
		return err
	}
	wpipe.Close()
	done := make(chan bool)
	go func() {
		select {
		case <-time.After(time.Minute):
			if inst.cfg.Debug {
				log.Printf("adb hanged")
			}
			cmd.Process.Kill()
		case <-done:
		}
	}()
	if err := cmd.Wait(); err != nil {
		close(done)
		out, _ := ioutil.ReadAll(rpipe)
		if inst.cfg.Debug {
			log.Printf("adb failed: %v\n%s", err, out)
		}
		return fmt.Errorf("adb %+v failed: %v\n%s", args, err, out)
	}
	close(done)
	if inst.cfg.Debug {
		log.Printf("adb returned")
	}
	return nil
}

func (inst *instance) repair() error {
	// Give the device up to 5 minutes to come up (it can be rebooting after a previous crash).
	time.Sleep(3 * time.Second)
	for i := 0; i < 300; i++ {
		time.Sleep(time.Second)
		if inst.adb("shell", "pwd") == nil {
			return nil
		}
	}
	// If it does not help, reboot.
	// adb reboot episodically hangs, so we use a more reliable way.
	// Ignore errors because all other adb commands hang as well
	// and the binary can already be on the device.
	inst.adb("push", inst.cfg.Executor, "/data/syz-executor")
	if err := inst.adb("shell", "/data/syz-executor", "reboot"); err != nil {
		return err
	}
	// Now give it another 5 minutes.
	time.Sleep(10 * time.Second)
	var err error
	for i := 0; i < 300; i++ {
		time.Sleep(time.Second)
		if err = inst.adb("shell", "pwd"); err == nil {
			return nil
		}
	}
	return fmt.Errorf("instance is dead and unrepairable: %v", err)
}

func (inst *instance) Close() {
	close(inst.closed)
	os.RemoveAll(inst.cfg.Workdir)
}

func (inst *instance) Copy(hostSrc string) (string, error) {
	vmDst := filepath.Join("/data", filepath.Base(hostSrc))
	if err := inst.adb("push", hostSrc, vmDst); err != nil {
		return "", err
	}
	return vmDst, nil
}

func (inst *instance) Run(timeout time.Duration, command string) (<-chan []byte, <-chan error, error) {
	catRpipe, catWpipe, err := vm.LongPipe()
	if err != nil {
		return nil, nil, err
	}

	cat := exec.Command("cat", inst.console)
	cat.Stdout = catWpipe
	cat.Stderr = catWpipe
	if err := cat.Start(); err != nil {
		catRpipe.Close()
		catWpipe.Close()
		return nil, nil, fmt.Errorf("failed to start cat %v: %v", inst.console, err)

	}
	catWpipe.Close()
	catDone := make(chan error, 1)
	go func() {
		err := cat.Wait()
		if inst.cfg.Debug {
			log.Printf("cat exited: %v", err)
		}
		catDone <- fmt.Errorf("cat exited: %v", err)
	}()

	adbRpipe, adbWpipe, err := vm.LongPipe()
	if err != nil {
		cat.Process.Kill()
		catRpipe.Close()
		return nil, nil, err
	}
	if inst.cfg.Debug {
		log.Printf("starting: adb shell %v", command)
	}
	adb := exec.Command(inst.cfg.Bin, "-s", inst.cfg.Device, "shell", "cd /data; "+command)
	adb.Stdout = adbWpipe
	adb.Stderr = adbWpipe
	if err := adb.Start(); err != nil {
		cat.Process.Kill()
		catRpipe.Close()
		adbRpipe.Close()
		adbWpipe.Close()
		return nil, nil, fmt.Errorf("failed to start adb: %v", err)
	}
	adbWpipe.Close()
	adbDone := make(chan error, 1)
	go func() {
		err := adb.Wait()
		if inst.cfg.Debug {
			log.Printf("adb exited: %v", err)
		}
		adbDone <- fmt.Errorf("adb exited: %v", err)
	}()

	var tee io.Writer
	if inst.cfg.Debug {
		tee = os.Stdout
	}
	merger := vm.NewOutputMerger(tee)
	merger.Add(catRpipe)
	merger.Add(adbRpipe)

	errc := make(chan error, 1)
	signal := func(err error) {
		select {
		case errc <- err:
		default:
		}
	}

	go func() {
		select {
		case <-time.After(timeout):
			signal(vm.TimeoutErr)
			cat.Process.Kill()
			adb.Process.Kill()
		case <-inst.closed:
			if inst.cfg.Debug {
				log.Printf("instance closed")
			}
			signal(fmt.Errorf("instance closed"))
			cat.Process.Kill()
			adb.Process.Kill()
		case err := <-catDone:
			signal(err)
			adb.Process.Kill()
		case err := <-adbDone:
			signal(err)
			cat.Process.Kill()
		}
		merger.Wait()
	}()
	return merger.Output, errc, nil
}
