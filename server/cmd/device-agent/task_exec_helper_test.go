package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTaskExecHelperChild(t *testing.T) {
	if os.Getenv("TASK_EXEC_HELPER_CHILD") != "1" {
		return
	}
	contents, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(91)
	}
	_, _ = fmt.Fprintf(os.Stdout, "stdout<%s>", contents)
	_, _ = fmt.Fprint(os.Stderr, "stderr<private>")
	os.Exit(0)
}

func TestTaskExecHelperBinaryChild(t *testing.T) {
	if os.Getenv("TASK_EXEC_HELPER_BINARY") != "1" {
		return
	}
	_, _ = os.Stdout.Write([]byte{0xff, 0x00, 0x80})
	_, _ = os.Stderr.Write([]byte{0xfe, 0x01})
	os.Exit(0)
}

func TestInternalTaskExecUsesPrivateStdinAndKeepsStreamsSeparate(t *testing.T) {
	directory := t.TempDir()
	promptPath := filepath.Join(directory, "prompt.md")
	prompt := []byte("private prompt\nsecond line\n")
	if err := os.WriteFile(promptPath, prompt, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TASK_EXEC_HELPER_CHILD", "1")
	var stdout, stderr bytes.Buffer
	err := runInternalTaskExec([]string{
		"--stdin-file", promptPath, "--", os.Args[0], "-test.run=^TestTaskExecHelperChild$",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runInternalTaskExec() error = %v; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if got, want := stdout.String(), "stdout<"+string(prompt)+">"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got := stderr.String(); got != "stderr<private>" {
		t.Fatalf("stderr = %q", got)
	}
	if strings.Contains(stdout.String()+stderr.String(), "WENZWORK_TASK_FRAME_V1") {
		t.Fatalf("legacy frame leaked: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestInternalTaskExecCopiesMalformedBytesWithoutFraming(t *testing.T) {
	t.Setenv("TASK_EXEC_HELPER_BINARY", "1")
	var stdout, stderr bytes.Buffer
	err := runInternalTaskExec([]string{"--", os.Args[0], "-test.run=^TestTaskExecHelperBinaryChild$"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.Bytes(), []byte{0xff, 0x00, 0x80}; !bytes.Equal(got, want) {
		t.Fatalf("stdout bytes = %x, want %x", got, want)
	}
	if got, want := stderr.Bytes(), []byte{0xfe, 0x01}; !bytes.Equal(got, want) {
		t.Fatalf("stderr bytes = %x, want %x", got, want)
	}
}

func TestInternalTaskExecRejectsNonPrivateInputAndPreservesExitCode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runInternalTaskExec([]string{"--stdin-file", "relative.md", "--", os.Args[0]}, &stdout, &stderr)
	var exit taskExecExitError
	if !errorsAsTaskExecExit(err, &exit) || exit.code != 126 {
		t.Fatalf("relative stdin error = %#v", err)
	}
	if stdout.Len() != 0 || !bytes.Contains(stderr.Bytes(), []byte("private task input")) {
		t.Fatalf("relative stdin output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestScriptRunnerExecutesPrivateShellInputAndPreservesExitCode(t *testing.T) {
	command := "printf 'script-out\\n'; printf 'script-err\\n' >&2; exit 7\n"
	if runtime.GOOS == "windows" {
		command = "echo script-out\r\necho script-err 1>&2\r\nexit /b 7\r\n"
	}
	path := filepath.Join(t.TempDir(), "script.prompt.md")
	if err := os.WriteFile(path, []byte(command), 0o600); err != nil {
		t.Fatal(err)
	}
	task := taskV2Record{Definition: taskV2Definition{
		Kind: "script", Config: []byte(`{"command":"placeholder","cwdChoice":"workspace"}`),
	}}
	invocation, err := prepareScriptTaskRunner(task)
	if err != nil {
		t.Fatal(err)
	}
	arguments := []string{"--stdin-file", path, "--", invocation.Executable}
	arguments = append(arguments, invocation.Arguments...)
	var stdout, stderr bytes.Buffer
	err = runInternalTaskExec(arguments, &stdout, &stderr)
	var exit taskExecExitError
	if !errorsAsTaskExecExit(err, &exit) || exit.code != 7 {
		t.Fatalf("script exit = %#v; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "script-out") || !strings.Contains(stderr.String(), "script-err") {
		t.Fatalf("script streams stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func errorsAsTaskExecExit(err error, target *taskExecExitError) bool {
	if err == nil {
		return false
	}
	value, ok := err.(taskExecExitError)
	if ok {
		*target = value
	}
	return ok
}
