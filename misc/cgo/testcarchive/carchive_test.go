// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package carchive_test

import (
	"bufio"
	"bytes"
	"debug/elf"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode"
)

// Program to run.
var bin []string

// C compiler with args (from $(go env CC) $(go env GOGCCFLAGS)).
var cc []string

// ".exe" on Windows.
var exeSuffix string

var GOOS, GOARCH, GOPATH string
var libgodir string

var testWork bool // If true, preserve temporary directories.

func TestMain(m *testing.M) {
	flag.BoolVar(&testWork, "testwork", false, "if true, log and preserve the test's temporary working directory")
	flag.Parse()
	if testing.Short() && os.Getenv("GO_BUILDER_NAME") == "" {
		fmt.Printf("SKIP - short mode and $GO_BUILDER_NAME not set\n")
		os.Exit(0)
	}
	log.SetFlags(log.Lshortfile)
	os.Exit(testMain(m))
}

func testMain(m *testing.M) int {
	// We need a writable GOPATH in which to run the tests.
	// Construct one in a temporary directory.
	var err error
	GOPATH, err = os.MkdirTemp("", "carchive_test")
	if err != nil {
		log.Panic(err)
	}
	if testWork {
		log.Println(GOPATH)
	} else {
		defer os.RemoveAll(GOPATH)
	}
	os.Setenv("GOPATH", GOPATH)

	// Copy testdata into GOPATH/src/testarchive, along with a go.mod file
	// declaring the same path.
	modRoot := filepath.Join(GOPATH, "src", "testcarchive")
	if err := overlayDir(modRoot, "testdata"); err != nil {
		log.Panic(err)
	}
	if err := os.Chdir(modRoot); err != nil {
		log.Panic(err)
	}
	os.Setenv("PWD", modRoot)
	if err := os.WriteFile("go.mod", []byte("module testcarchive\n"), 0666); err != nil {
		log.Panic(err)
	}

	GOOS = goEnv("GOOS")
	GOARCH = goEnv("GOARCH")
	bin = cmdToRun("./testp")

	ccOut := goEnv("CC")
	cc = []string{string(ccOut)}

	out := goEnv("GOGCCFLAGS")
	quote := '\000'
	start := 0
	lastSpace := true
	backslash := false
	s := string(out)
	for i, c := range s {
		if quote == '\000' && unicode.IsSpace(c) {
			if !lastSpace {
				cc = append(cc, s[start:i])
				lastSpace = true
			}
		} else {
			if lastSpace {
				start = i
				lastSpace = false
			}
			if quote == '\000' && !backslash && (c == '"' || c == '\'') {
				quote = c
				backslash = false
			} else if !backslash && quote == c {
				quote = '\000'
			} else if (quote == '\000' || quote == '"') && !backslash && c == '\\' {
				backslash = true
			} else {
				backslash = false
			}
		}
	}
	if !lastSpace {
		cc = append(cc, s[start:])
	}

	if GOOS == "aix" {
		// -Wl,-bnoobjreorder is mandatory to keep the same layout
		// in .text section.
		cc = append(cc, "-Wl,-bnoobjreorder")
	}
	libbase := GOOS + "_" + GOARCH
	if runtime.Compiler == "gccgo" {
		libbase = "gccgo_" + libgodir + "_fPIC"
	} else {
		switch GOOS {
		case "darwin", "ios":
			if GOARCH == "arm64" {
				libbase += "_shared"
			}
		case "dragonfly", "freebsd", "linux", "netbsd", "openbsd", "solaris", "illumos":
			libbase += "_shared"
		}
	}
	libgodir = filepath.Join(GOPATH, "pkg", libbase, "testcarchive")
	cc = append(cc, "-I", libgodir)

	if GOOS == "windows" {
		exeSuffix = ".exe"
	}

	return m.Run()
}

func goEnv(key string) string {
	out, err := exec.Command("go", "env", key).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			fmt.Fprintf(os.Stderr, "%s", ee.Stderr)
		}
		log.Panicf("go env %s failed:\n%s\n", key, err)
	}
	return strings.TrimSpace(string(out))
}

func cmdToRun(name string) []string {
	execScript := "go_" + goEnv("GOOS") + "_" + goEnv("GOARCH") + "_exec"
	executor, err := exec.LookPath(execScript)
	if err != nil {
		return []string{name}
	}
	return []string{executor, name}
}

// genHeader writes a C header file for the C-exported declarations found in .go
// source files in dir.
//
// TODO(golang.org/issue/35715): This should be simpler.
func genHeader(t *testing.T, header, dir string) {
	t.Helper()

	// The 'cgo' command generates a number of additional artifacts,
	// but we're only interested in the header.
	// Shunt the rest of the outputs to a temporary directory.
	objDir, err := os.MkdirTemp(GOPATH, "_obj")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(objDir)

	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "tool", "cgo",
		"-objdir", objDir,
		"-exportheader", header)
	cmd.Args = append(cmd.Args, files...)
	t.Log(cmd.Args)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("%s", out)
		t.Fatal(err)
	}
}

func testInstall(t *testing.T, exe, libgoa, libgoh string, buildcmd ...string) {
	t.Helper()
	cmd := exec.Command(buildcmd[0], buildcmd[1:]...)
	t.Log(buildcmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("%s", out)
		t.Fatal(err)
	}
	if !testWork {
		defer func() {
			os.Remove(libgoa)
			os.Remove(libgoh)
		}()
	}

	ccArgs := append(cc, "-o", exe, "main.c")
	if GOOS == "windows" {
		ccArgs = append(ccArgs, "main_windows.c", libgoa, "-lntdll", "-lws2_32", "-lwinmm")
	} else {
		ccArgs = append(ccArgs, "main_unix.c", libgoa)
	}
	if runtime.Compiler == "gccgo" {
		ccArgs = append(ccArgs, "-lgo")
	}
	t.Log(ccArgs)
	if out, err := exec.Command(ccArgs[0], ccArgs[1:]...).CombinedOutput(); err != nil {
		t.Logf("%s", out)
		t.Fatal(err)
	}
	if !testWork {
		defer os.Remove(exe)
	}

	binArgs := append(cmdToRun(exe), "arg1", "arg2")
	cmd = exec.Command(binArgs[0], binArgs[1:]...)
	if runtime.Compiler == "gccgo" {
		cmd.Env = append(os.Environ(), "GCCGO=1")
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("%s", out)
		t.Fatal(err)
	}

	checkLineComments(t, libgoh)
}

var badLineRegexp = regexp.MustCompile(`(?m)^#line [0-9]+ "/.*$`)

// checkIsExecutable verifies that exe exists and has execute permission.
//
// (https://golang.org/issue/49693 notes failures with "no such file or
// directory", so we want to double-check that the executable actually exists
// immediately after we build it in order to better understand that failure
// mode.)
func checkIsExecutable(t *testing.T, exe string) {
	t.Helper()
	fi, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		// os.File doesn't check the "execute" permission on Windows files
		// and as a result doesn't set that bit in a file's permissions.
		// Assume that if the file exists it is “executable enough”.
		return
	}
	if fi.Mode()&0111 == 0 {
		t.Fatalf("%s is not executable: %0o", exe, fi.Mode()&fs.ModePerm)
	}
}

// checkLineComments checks that the export header generated by
// -buildmode=c-archive doesn't have any absolute paths in the #line
// comments. We don't want those paths because they are unhelpful for
// the user and make the files change based on details of the location
// of GOPATH.
func checkLineComments(t *testing.T, hdrname string) {
	hdr, err := os.ReadFile(hdrname)
	if err != nil {
		if !os.IsNotExist(err) {
			t.Error(err)
		}
		return
	}
	if line := badLineRegexp.Find(hdr); line != nil {
		t.Errorf("bad #line directive with absolute path in %s: %q", hdrname, line)
	}
}

func TestInstall(t *testing.T) {
	if !testWork {
		defer os.RemoveAll(filepath.Join(GOPATH, "pkg"))
	}

	libgoa := "libgo.a"
	if runtime.Compiler == "gccgo" {
		libgoa = "liblibgo.a"
	}

	// Generate the p.h header file.
	//
	// 'go install -i -buildmode=c-archive ./libgo' would do that too, but that
	// would also attempt to install transitive standard-library dependencies to
	// GOROOT, and we cannot assume that GOROOT is writable. (A non-root user may
	// be running this test in a GOROOT owned by root.)
	genHeader(t, "p.h", "./p")

	testInstall(t, "./testp1"+exeSuffix,
		filepath.Join(libgodir, libgoa),
		filepath.Join(libgodir, "libgo.h"),
		"go", "install", "-buildmode=c-archive", "./libgo")

	// Test building libgo other than installing it.
	// Header files are now present.
	testInstall(t, "./testp2"+exeSuffix, "libgo.a", "libgo.h",
		"go", "build", "-buildmode=c-archive", filepath.Join(".", "libgo", "libgo.go"))

	testInstall(t, "./testp3"+exeSuffix, "libgo.a", "libgo.h",
		"go", "build", "-buildmode=c-archive", "-o", "libgo.a", "./libgo")
}

func TestEarlySignalHandler(t *testing.T) {
	switch GOOS {
	case "darwin", "ios":
		switch GOARCH {
		case "arm64":
			t.Skipf("skipping on %s/%s; see https://golang.org/issue/13701", GOOS, GOARCH)
		}
	case "windows":
		t.Skip("skipping signal test on Windows")
	}

	if !testWork {
		defer func() {
			os.Remove("libgo2.a")
			os.Remove("libgo2.h")
			os.Remove("testp" + exeSuffix)
			os.RemoveAll(filepath.Join(GOPATH, "pkg"))
		}()
	}

	cmd := exec.Command("go", "build", "-buildmode=c-archive", "-o", "libgo2.a", "./libgo2")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("%s", out)
		t.Fatal(err)
	}
	checkLineComments(t, "libgo2.h")

	ccArgs := append(cc, "-o", "testp"+exeSuffix, "main2.c", "libgo2.a")
	if runtime.Compiler == "gccgo" {
		ccArgs = append(ccArgs, "-lgo")
	}
	if out, err := exec.Command(ccArgs[0], ccArgs[1:]...).CombinedOutput(); err != nil {
		t.Logf("%s", out)
		t.Fatal(err)
	}

	darwin := "0"
	if runtime.GOOS == "darwin" {
		darwin = "1"
	}
	cmd = exec.Command(bin[0], append(bin[1:], darwin)...)

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("%s", out)
		t.Fatal(err)
	}
}

func TestSignalForwarding(t *testing.T) {
	checkSignalForwardingTest(t)

	if !testWork {
		defer func() {
			os.Remove("libgo2.a")
			os.Remove("libgo2.h")
			os.Remove("testp" + exeSuffix)
			os.RemoveAll(filepath.Join(GOPATH, "pkg"))
		}()
	}

	cmd := exec.Command("go", "build", "-buildmode=c-archive", "-o", "libgo2.a", "./libgo2")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("%s", out)
		t.Fatal(err)
	}
	checkLineComments(t, "libgo2.h")

	ccArgs := append(cc, "-o", "testp"+exeSuffix, "main5.c", "libgo2.a")
	if runtime.Compiler == "gccgo" {
		ccArgs = append(ccArgs, "-lgo")
	}
	if out, err := exec.Command(ccArgs[0], ccArgs[1:]...).CombinedOutput(); err != nil {
		t.Logf("%s", out)
		t.Fatal(err)
	}

	cmd = exec.Command(bin[0], append(bin[1:], "1")...)

	out, err := cmd.CombinedOutput()
	t.Logf("%v\n%s", cmd.Args, out)
	expectSignal(t, err, syscall.SIGSEGV)

	// SIGPIPE is never forwarded on darwin. See golang.org/issue/33384.
	if runtime.GOOS != "darwin" && runtime.GOOS != "ios" {
		// Test SIGPIPE forwarding
		cmd = exec.Command(bin[0], append(bin[1:], "3")...)

		out, err = cmd.CombinedOutput()
		if len(out) > 0 {
			t.Logf("%s", out)
		}
		expectSignal(t, err, syscall.SIGPIPE)
	}
}

func TestSignalForwardingExternal(t *testing.T) {
	if GOOS == "freebsd" || GOOS == "aix" {
		t.Skipf("skipping on %s/%s; signal always goes to the Go runtime", GOOS, GOARCH)
	} else if GOOS == "darwin" && GOARCH == "amd64" {
		t.Skipf("skipping on %s/%s: runtime does not permit SI_USER SIGSEGV", GOOS, GOARCH)
	}
	checkSignalForwardingTest(t)

	if !testWork {
		defer func() {
			os.Remove("libgo2.a")
			os.Remove("libgo2.h")
			os.Remove("testp" + exeSuffix)
			os.RemoveAll(filepath.Join(GOPATH, "pkg"))
		}()
	}

	cmd := exec.Command("go", "build", "-buildmode=c-archive", "-o", "libgo2.a", "./libgo2")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("%s", out)
		t.Fatal(err)
	}
	checkLineComments(t, "libgo2.h")

	ccArgs := append(cc, "-o", "testp"+exeSuffix, "main5.c", "libgo2.a")
	if runtime.Compiler == "gccgo" {
		ccArgs = append(ccArgs, "-lgo")
	}
	if out, err := exec.Command(ccArgs[0], ccArgs[1:]...).CombinedOutput(); err != nil {
		t.Logf("%s", out)
		t.Fatal(err)
	}

	// We want to send the process a signal and see if it dies.
	// Normally the signal goes to the C thread, the Go signal
	// handler picks it up, sees that it is running in a C thread,
	// and the program dies. Unfortunately, occasionally the
	// signal is delivered to a Go thread, which winds up
	// discarding it because it was sent by another program and
	// there is no Go handler for it. To avoid this, run the
	// program several times in the hopes that it will eventually
	// fail.
	const tries = 20
	for i := 0; i < tries; i++ {
		cmd = exec.Command(bin[0], append(bin[1:], "2")...)

		stderr, err := cmd.StderrPipe()
		if err != nil {
			t.Fatal(err)
		}
		defer stderr.Close()

		r := bufio.NewReader(stderr)

		err = cmd.Start()

		if err != nil {
			t.Fatal(err)
		}

		// Wait for trigger to ensure that the process is started.
		ok, err := r.ReadString('\n')

		// Verify trigger.
		if err != nil || ok != "OK\n" {
			t.Fatalf("Did not receive OK signal")
		}

		// Give the program a chance to enter the sleep function.
		time.Sleep(time.Millisecond)

		cmd.Process.Signal(syscall.SIGSEGV)

		err = cmd.Wait()

		if err == nil {
			continue
		}

		if expectSignal(t, err, syscall.SIGSEGV) {
			return
		}
	}

	t.Errorf("program succeeded unexpectedly %d times", tries)
}

// checkSignalForwardingTest calls t.Skip if the SignalForwarding test
// doesn't work on this platform.
func checkSignalForwardingTest(t *testing.T) {
	switch GOOS {
	case "darwin", "ios":
		switch GOARCH {
		case "arm64":
			t.Skipf("skipping on %s/%s; see https://golang.org/issue/13701", GOOS, GOARCH)
		}
	case "windows":
		t.Skip("skipping signal test on Windows")
	}
}

// expectSignal checks that err, the exit status of a test program,
// shows a failure due to a specific signal. Returns whether we found
// the expected signal.
func expectSignal(t *testing.T, err error, sig syscall.Signal) bool {
	if err == nil {
		t.Error("test program succeeded unexpectedly")
	} else if ee, ok := err.(*exec.ExitError); !ok {
		t.Errorf("error (%v) has type %T; expected exec.ExitError", err, err)
	} else if ws, ok := ee.Sys().(syscall.WaitStatus); !ok {
		t.Errorf("error.Sys (%v) has type %T; expected syscall.WaitStatus", ee.Sys(), ee.Sys())
	} else if !ws.Signaled() || ws.Signal() != sig {
		t.Errorf("got %v; expected signal %v", ee, sig)
	} else {
		return true
	}
	return false
}

func TestOsSignal(t *testing.T) {
	switch GOOS {
	case "windows":
		t.Skip("skipping signal test on Windows")
	}

	if !testWork {
		defer func() {
			os.Remove("libgo3.a")
			os.Remove("libgo3.h")
			os.Remove("testp" + exeSuffix)
			os.RemoveAll(filepath.Join(GOPATH, "pkg"))
		}()
	}

	cmd := exec.Command("go", "build", "-buildmode=c-archive", "-o", "libgo3.a", "./libgo3")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("%s", out)
		t.Fatal(err)
	}
	checkLineComments(t, "libgo3.h")

	ccArgs := append(cc, "-o", "testp"+exeSuffix, "main3.c", "libgo3.a")
	if runtime.Compiler == "gccgo" {
		ccArgs = append(ccArgs, "-lgo")
	}
	if out, err := exec.Command(ccArgs[0], ccArgs[1:]...).CombinedOutput(); err != nil {
		t.Logf("%s", out)
		t.Fatal(err)
	}

	if out, err := exec.Command(bin[0], bin[1:]...).CombinedOutput(); err != nil {
		t.Logf("%s", out)
		t.Fatal(err)
	}
}

func TestSigaltstack(t *testing.T) {
	switch GOOS {
	case "windows":
		t.Skip("skipping signal test on Windows")
	}

	if !testWork {
		defer func() {
			os.Remove("libgo4.a")
			os.Remove("libgo4.h")
			os.Remove("testp" + exeSuffix)
			os.RemoveAll(filepath.Join(GOPATH, "pkg"))
		}()
	}

	cmd := exec.Command("go", "build", "-buildmode=c-archive", "-o", "libgo4.a", "./libgo4")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("%s", out)
		t.Fatal(err)
	}
	checkLineComments(t, "libgo4.h")

	ccArgs := append(cc, "-o", "testp"+exeSuffix, "main4.c", "libgo4.a")
	if runtime.Compiler == "gccgo" {
		ccArgs = append(ccArgs, "-lgo")
	}
	if out, err := exec.Command(ccArgs[0], ccArgs[1:]...).CombinedOutput(); err != nil {
		t.Logf("%s", out)
		t.Fatal(err)
	}

	if out, err := exec.Command(bin[0], bin[1:]...).CombinedOutput(); err != nil {
		t.Logf("%s", out)
		t.Fatal(err)
	}
}

const testar = `#!/usr/bin/env bash
while [[ $1 == -* ]] >/dev/null; do
  shift
done
echo "testar" > $1
echo "testar" > PWD/testar.ran
`

func TestExtar(t *testing.T) {
	switch GOOS {
	case "windows":
		t.Skip("skipping signal test on Windows")
	}
	if runtime.Compiler == "gccgo" {
		t.Skip("skipping -extar test when using gccgo")
	}
	if runtime.GOOS == "ios" {
		t.Skip("shell scripts are not executable on iOS hosts")
	}

	if !testWork {
		defer func() {
			os.Remove("libgo4.a")
			os.Remove("libgo4.h")
			os.Remove("testar")
			os.Remove("testar.ran")
			os.RemoveAll(filepath.Join(GOPATH, "pkg"))
		}()
	}

	os.Remove("testar")
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(testar, "PWD", dir, 1)
	if err := os.WriteFile("testar", []byte(s), 0777); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "build", "-buildmode=c-archive", "-ldflags=-extar="+filepath.Join(dir, "testar"), "-o", "libgo4.a", "./libgo4")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("%s", out)
		t.Fatal(err)
	}
	checkLineComments(t, "libgo4.h")

	if _, err := os.Stat("testar.ran"); err != nil {
		if os.IsNotExist(err) {
			t.Error("testar does not exist after go build")
		} else {
			t.Errorf("error checking testar: %v", err)
		}
	}
}

func TestPIE(t *testing.T) {
	switch GOOS {
	case "windows", "darwin", "ios", "plan9":
		t.Skipf("skipping PIE test on %s", GOOS)
	}

	if !testWork {
		defer func() {
			os.Remove("testp" + exeSuffix)
			os.RemoveAll(filepath.Join(GOPATH, "pkg"))
		}()
	}

	// Generate the p.h header file.
	//
	// 'go install -i -buildmode=c-archive ./libgo' would do that too, but that
	// would also attempt to install transitive standard-library dependencies to
	// GOROOT, and we cannot assume that GOROOT is writable. (A non-root user may
	// be running this test in a GOROOT owned by root.)
	genHeader(t, "p.h", "./p")

	cmd := exec.Command("go", "install", "-buildmode=c-archive", "./libgo")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("%s", out)
		t.Fatal(err)
	}

	libgoa := "libgo.a"
	if runtime.Compiler == "gccgo" {
		libgoa = "liblibgo.a"
	}

	ccArgs := append(cc, "-fPIE", "-pie", "-o", "testp"+exeSuffix, "main.c", "main_unix.c", filepath.Join(libgodir, libgoa))
	if runtime.Compiler == "gccgo" {
		ccArgs = append(ccArgs, "-lgo")
	}
	if out, err := exec.Command(ccArgs[0], ccArgs[1:]...).CombinedOutput(); err != nil {
		t.Logf("%s", out)
		t.Fatal(err)
	}

	binArgs := append(bin, "arg1", "arg2")
	cmd = exec.Command(binArgs[0], binArgs[1:]...)
	if runtime.Compiler == "gccgo" {
		cmd.Env = append(os.Environ(), "GCCGO=1")
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("%s", out)
		t.Fatal(err)
	}

	if GOOS != "aix" {
		f, err := elf.Open("testp" + exeSuffix)
		if err != nil {
			t.Fatal("elf.Open failed: ", err)
		}
		defer f.Close()
		if hasDynTag(t, f, elf.DT_TEXTREL) {
			t.Errorf("%s has DT_TEXTREL flag", "testp"+exeSuffix)
		}
	}
}

func hasDynTag(t *testing.T, f *elf.File, tag elf.DynTag) bool {
	ds := f.SectionByType(elf.SHT_DYNAMIC)
	if ds == nil {
		t.Error("no SHT_DYNAMIC section")
		return false
	}
	d, err := ds.Data()
	if err != nil {
		t.Errorf("can't read SHT_DYNAMIC contents: %v", err)
		return false
	}
	for len(d) > 0 {
		var t elf.DynTag
		switch f.Class {
		case elf.ELFCLASS32:
			t = elf.DynTag(f.ByteOrder.Uint32(d[:4]))
			d = d[8:]
		case elf.ELFCLASS64:
			t = elf.DynTag(f.ByteOrder.Uint64(d[:8]))
			d = d[16:]
		}
		if t == tag {
			return true
		}
	}
	return false
}

func TestSIGPROF(t *testing.T) {
	switch GOOS {
	case "windows", "plan9":
		t.Skipf("skipping SIGPROF test on %s", GOOS)
	case "darwin", "ios":
		t.Skipf("skipping SIGPROF test on %s; see https://golang.org/issue/19320", GOOS)
	}

	t.Parallel()

	if !testWork {
		defer func() {
			os.Remove("testp6" + exeSuffix)
			os.Remove("libgo6.a")
			os.Remove("libgo6.h")
		}()
	}

	cmd := exec.Command("go", "build", "-buildmode=c-archive", "-o", "libgo6.a", "./libgo6")
	out, err := cmd.CombinedOutput()
	t.Logf("%v\n%s", cmd.Args, out)
	if err != nil {
		t.Fatal(err)
	}
	checkLineComments(t, "libgo6.h")

	ccArgs := append(cc, "-o", "testp6"+exeSuffix, "main6.c", "libgo6.a")
	if runtime.Compiler == "gccgo" {
		ccArgs = append(ccArgs, "-lgo")
	}
	out, err = exec.Command(ccArgs[0], ccArgs[1:]...).CombinedOutput()
	t.Logf("%v\n%s", ccArgs, out)
	if err != nil {
		t.Fatal(err)
	}
	checkIsExecutable(t, "./testp6"+exeSuffix)

	argv := cmdToRun("./testp6")
	cmd = exec.Command(argv[0], argv[1:]...)
	out, err = cmd.CombinedOutput()
	t.Logf("%v\n%s", argv, out)
	if err != nil {
		t.Fatal(err)
	}
}

// TestCompileWithoutShared tests that if we compile code without the
// -shared option, we can put it into an archive. When we use the go
// tool with -buildmode=c-archive, it passes -shared to the compiler,
// so we override that. The go tool doesn't work this way, but Bazel
// will likely do it in the future. And it ought to work. This test
// was added because at one time it did not work on PPC Linux.
func TestCompileWithoutShared(t *testing.T) {
	// For simplicity, reuse the signal forwarding test.
	checkSignalForwardingTest(t)

	if !testWork {
		defer func() {
			os.Remove("libgo2.a")
			os.Remove("libgo2.h")
		}()
	}

	cmd := exec.Command("go", "build", "-buildmode=c-archive", "-gcflags=-shared=false", "-o", "libgo2.a", "./libgo2")
	out, err := cmd.CombinedOutput()
	t.Logf("%v\n%s", cmd.Args, out)
	if err != nil {
		t.Fatal(err)
	}
	checkLineComments(t, "libgo2.h")

	exe := "./testnoshared" + exeSuffix

	// In some cases, -no-pie is needed here, but not accepted everywhere. First try
	// if -no-pie is accepted. See #22126.
	ccArgs := append(cc, "-o", exe, "-no-pie", "main5.c", "libgo2.a")
	if runtime.Compiler == "gccgo" {
		ccArgs = append(ccArgs, "-lgo")
	}
	out, err = exec.Command(ccArgs[0], ccArgs[1:]...).CombinedOutput()
	t.Logf("%v\n%s", ccArgs, out)

	// If -no-pie unrecognized, try -nopie if this is possibly clang
	if err != nil && bytes.Contains(out, []byte("unknown")) && !strings.Contains(cc[0], "gcc") {
		ccArgs = append(cc, "-o", exe, "-nopie", "main5.c", "libgo2.a")
		out, err = exec.Command(ccArgs[0], ccArgs[1:]...).CombinedOutput()
		t.Logf("%v\n%s", ccArgs, out)
	}

	// Don't use either -no-pie or -nopie
	if err != nil && bytes.Contains(out, []byte("unrecognized")) {
		ccArgs = append(cc, "-o", exe, "main5.c", "libgo2.a")
		out, err = exec.Command(ccArgs[0], ccArgs[1:]...).CombinedOutput()
		t.Logf("%v\n%s", ccArgs, out)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !testWork {
		defer os.Remove(exe)
	}

	binArgs := append(cmdToRun(exe), "1")
	out, err = exec.Command(binArgs[0], binArgs[1:]...).CombinedOutput()
	t.Logf("%v\n%s", binArgs, out)
	expectSignal(t, err, syscall.SIGSEGV)

	// SIGPIPE is never forwarded on darwin. See golang.org/issue/33384.
	if runtime.GOOS != "darwin" && runtime.GOOS != "ios" {
		binArgs := append(cmdToRun(exe), "3")
		out, err = exec.Command(binArgs[0], binArgs[1:]...).CombinedOutput()
		t.Logf("%v\n%s", binArgs, out)
		expectSignal(t, err, syscall.SIGPIPE)
	}
}

// Test that installing a second time recreates the header file.
func TestCachedInstall(t *testing.T) {
	if !testWork {
		defer os.RemoveAll(filepath.Join(GOPATH, "pkg"))
	}

	h := filepath.Join(libgodir, "libgo.h")

	buildcmd := []string{"go", "install", "-buildmode=c-archive", "./libgo"}

	cmd := exec.Command(buildcmd[0], buildcmd[1:]...)
	t.Log(buildcmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("%s", out)
		t.Fatal(err)
	}

	if _, err := os.Stat(h); err != nil {
		t.Errorf("libgo.h not installed: %v", err)
	}

	if err := os.Remove(h); err != nil {
		t.Fatal(err)
	}

	cmd = exec.Command(buildcmd[0], buildcmd[1:]...)
	t.Log(buildcmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("%s", out)
		t.Fatal(err)
	}

	if _, err := os.Stat(h); err != nil {
		t.Errorf("libgo.h not installed in second run: %v", err)
	}
}

// Issue 35294.
func TestManyCalls(t *testing.T) {
	t.Parallel()

	if !testWork {
		defer func() {
			os.Remove("testp7" + exeSuffix)
			os.Remove("libgo7.a")
			os.Remove("libgo7.h")
		}()
	}

	cmd := exec.Command("go", "build", "-buildmode=c-archive", "-o", "libgo7.a", "./libgo7")
	out, err := cmd.CombinedOutput()
	t.Logf("%v\n%s", cmd.Args, out)
	if err != nil {
		t.Fatal(err)
	}
	checkLineComments(t, "libgo7.h")

	ccArgs := append(cc, "-o", "testp7"+exeSuffix, "main7.c", "libgo7.a")
	if runtime.Compiler == "gccgo" {
		ccArgs = append(ccArgs, "-lgo")
	}
	out, err = exec.Command(ccArgs[0], ccArgs[1:]...).CombinedOutput()
	t.Logf("%v\n%s", ccArgs, out)
	if err != nil {
		t.Fatal(err)
	}
	checkIsExecutable(t, "./testp7"+exeSuffix)

	argv := cmdToRun("./testp7")
	cmd = exec.Command(argv[0], argv[1:]...)
	sb := new(strings.Builder)
	cmd.Stdout = sb
	cmd.Stderr = sb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	timer := time.AfterFunc(time.Minute,
		func() {
			t.Error("test program timed out")
			cmd.Process.Kill()
		},
	)
	defer timer.Stop()

	err = cmd.Wait()
	t.Logf("%v\n%s", cmd.Args, sb)
	if err != nil {
		t.Error(err)
	}
}

// Issue 49288.
func TestPreemption(t *testing.T) {
	if runtime.Compiler == "gccgo" {
		t.Skip("skipping asynchronous preemption test with gccgo")
	}

	t.Parallel()

	if !testWork {
		defer func() {
			os.Remove("testp8" + exeSuffix)
			os.Remove("libgo8.a")
			os.Remove("libgo8.h")
		}()
	}

	cmd := exec.Command("go", "build", "-buildmode=c-archive", "-o", "libgo8.a", "./libgo8")
	out, err := cmd.CombinedOutput()
	t.Logf("%v\n%s", cmd.Args, out)
	if err != nil {
		t.Fatal(err)
	}
	checkLineComments(t, "libgo8.h")

	ccArgs := append(cc, "-o", "testp8"+exeSuffix, "main8.c", "libgo8.a")
	out, err = exec.Command(ccArgs[0], ccArgs[1:]...).CombinedOutput()
	t.Logf("%v\n%s", ccArgs, out)
	if err != nil {
		t.Fatal(err)
	}
	checkIsExecutable(t, "./testp8"+exeSuffix)

	argv := cmdToRun("./testp8")
	cmd = exec.Command(argv[0], argv[1:]...)
	sb := new(strings.Builder)
	cmd.Stdout = sb
	cmd.Stderr = sb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	timer := time.AfterFunc(time.Minute,
		func() {
			t.Error("test program timed out")
			cmd.Process.Kill()
		},
	)
	defer timer.Stop()

	err = cmd.Wait()
	t.Logf("%v\n%s", cmd.Args, sb)
	if err != nil {
		t.Error(err)
	}
}
