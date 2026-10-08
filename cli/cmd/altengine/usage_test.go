package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestMain lets a test run the real main() in a child process, to see its exit code and which
// stream it wrote to.
func TestMain(m *testing.M) {
	if args := os.Getenv("ALTENGINE_TEST_MAIN"); args != "" {
		os.Args = append([]string{"altengine"}, strings.Fields(args)...)
		if args == "-" {
			os.Args = os.Args[:1]
		}
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runMain(t *testing.T, args string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "ALTENGINE_TEST_MAIN="+args)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, out.String(), errb.String()
}

// Asking for help is a success on stdout; a wrong command line is a failure on stderr. Every
// subcommand group used to answer --help with "unknown subcommand" and exit 1.
func TestHelpExitsZeroOnStdout(t *testing.T) {
	for _, args := range []string{"help", "--help", "functions --help", "static -h", "automation help"} {
		code, out, errOut := runMain(t, args)
		if code != 0 || !strings.Contains(out, "usage") && !strings.Contains(out, "Usage") || errOut != "" {
			t.Errorf("%q: exit %d, stdout %d bytes, stderr %q", args, code, len(out), errOut)
		}
	}
	for _, args := range []string{"-", "bogus", "functions", "static nope", "automation nope"} {
		code, out, errOut := runMain(t, args)
		if code != 1 || out != "" || errOut == "" {
			t.Errorf("%q: exit %d, stdout %q, stderr %d bytes", args, code, out, len(errOut))
		}
	}
}

func TestUsageNamesEveryAutomationSubcommand(t *testing.T) {
	_, out, _ := runMain(t, "help")
	if !strings.Contains(out, "activate") {
		t.Error("top-level usage omits automation activate")
	}
	_, out, _ = runMain(t, "static -h")
	if strings.Contains(out, "functions instance") {
		t.Error("static help describes a functions instance")
	}
}
