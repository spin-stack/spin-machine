// SPDX-License-Identifier: Apache-2.0

package pressure

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// What the sender writes is what the host reads, and the lines are the ones the format documents.
func TestEveryLineReadsBackAsWritten(t *testing.T) {
	for _, c := range []struct {
		line Line
		text string
	}{
		{Hello{OOMKills: 3}, "spin-machine-pressure 1 oom=3\n"},
		{Stall{Resource: Memory, Threshold: 150 * time.Millisecond, Window: time.Second, Total: 8123 * time.Microsecond}, "stall memory 150000 1000000 8123\n"},
		{Stall{Resource: CPU, Total: 0}, "stall cpu 0 0 0\n"},
		{Stall{Resource: IO, Total: time.Second}, "stall io 0 0 1000000\n"},
		{OOM{Kills: 4}, "oom 4\n"},
	} {
		text := string(c.line.Append([]byte("x")))
		if text != "x"+c.text {
			t.Errorf("%#v is written %q, not %q", c.line, text, c.text)
		}
		got, err := Parse([]byte(strings.TrimSuffix(c.text, "\n")))
		if err != nil || !reflect.DeepEqual(got, c.line) {
			t.Errorf("%q reads as %#v, %v; want %#v", c.text, got, err, c.line)
		}
		if len(c.text) > MaxLine {
			t.Errorf("%q is longer than a line may be", c.text)
		}
	}
}

func TestALineThatIsNotOneIsRefused(t *testing.T) {
	for _, line := range []string{
		"", "  ", "hello",
		"spin-machine-pressure 1", "spin-machine-pressure 1 oom=3 more", "spin-machine-pressure 1 kills=3",
		"spin-machine-pressure 1 oom=x", "spin-machine-pressure 1 oom=-1", "spin-machine-pressure 1 oom=a",
		"stall memory 1 2", "stall swap 1 2 3", "stall cpu 1 2 x", "stall cpu -1 2 3", "stall cpu 1 -2 3",
		"stall cpu 1 2 -3", "stall cpu 1 2 3 4",
		"oom", "oom 1 2", "oom x", "oom -1", "oom a",
	} {
		if l, err := Parse([]byte(line)); err == nil || errors.Is(err, ErrVersion) {
			t.Errorf("%q reads as %#v, %v", line, l, err)
		}
	}
	for _, line := range []string{"spin-machine-pressure 2 oom=0", "spin-machine-pressure x oom=0"} {
		if _, err := Parse([]byte(line)); !errors.Is(err, ErrVersion) {
			t.Errorf("%q: %v, not a version this host does not read", line, err)
		}
	}
}

func TestAResourceIsNamedAsPSINamesIt(t *testing.T) {
	names := []string{}
	for _, r := range Resources() {
		names = append(names, r.String())
	}
	if got := strings.Join(names, " "); got != "cpu memory io" {
		t.Errorf("the resources are %q", got)
	}
	if got := Resource(9).String(); got != "resource(9)" {
		t.Errorf("an unknown resource is %q", got)
	}
}

func TestSomeTotalIsTheSomeLinesTotal(t *testing.T) {
	text := "some avg10=1.00 avg60=0.50 avg300=0.10 total=1234\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=99\n"
	if d, err := SomeTotal([]byte(text)); err != nil || d != 1234*time.Microsecond {
		t.Errorf("%v, %v", d, err)
	}
	if d, err := SomeTotal([]byte("\nsome total=5")); err != nil || d != 5*time.Microsecond {
		t.Errorf("a some line after an empty one: %v, %v", d, err)
	}
	for _, bad := range []string{"full avg10=0 total=1\n", "some avg10=1.00\n", "some total=x\n"} {
		if d, err := SomeTotal([]byte(bad)); err == nil {
			t.Errorf("%q has a some total of %v", bad, d)
		}
	}
}

// Arm writes the trigger PSI reads, NUL-terminated, and Total reads the file it armed.
func TestArmWritesTheTriggerAndTotalReadsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.pressure")
	if err := os.WriteFile(path, []byte("some avg10=0.00 avg60=0.00 avg300=0.00 total=0000042\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr, err := Arm(path, Memory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tr.File.Close() }()
	if tr.Resource != Memory {
		t.Errorf("armed for %v", tr.Resource)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "some 150000 1000000\x00") {
		t.Errorf("the trigger written is %q", b)
	}
	if err := os.WriteFile(path, []byte("some avg10=0.00 avg60=0.00 avg300=0.00 total=42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if d, err := tr.Total(); err != nil || d != 42*time.Microsecond {
		t.Errorf("Total is %v, %v", d, err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if d, err := tr.Total(); err == nil {
		t.Errorf("an empty file has a total of %v", d)
	}
}

func TestArmSaysWhatItCouldNotArm(t *testing.T) {
	dir := t.TempDir()
	if _, err := Arm(filepath.Join(dir, "none"), CPU); err == nil || !strings.Contains(err.Error(), "none") {
		t.Errorf("a file that is not there: %v", err)
	}
	if _, err := Arm("/dev/full", IO); err == nil || !strings.Contains(err.Error(), `arming "some 150000 1000000" on /dev/full`) {
		t.Errorf("a file that takes no trigger: %v", err)
	}
}
