package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// taskExecExitError lets the private helper preserve the child exit code
// without printing an unframed error through main. It is never returned by a
// public Agent command.
type taskExecExitError struct{ code int }

func (err taskExecExitError) Error() string {
	return fmt.Sprintf("task helper exited with code %d", err.code)
}

func runInternalTaskExec(arguments []string, stdout, stderr io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	flags := flag.NewFlagSet("internal-task-exec", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	stdinPath := flags.String("stdin-file", "", "private task input file")
	if err := flags.Parse(arguments); err != nil {
		return taskExecExitError{code: 125}
	}
	childArguments := flags.Args()
	if len(childArguments) == 0 || !filepath.IsAbs(childArguments[0]) || strings.IndexByte(childArguments[0], 0) >= 0 {
		return taskExecExitError{code: 125}
	}
	for _, argument := range childArguments[1:] {
		if strings.IndexByte(argument, 0) >= 0 {
			return taskExecExitError{code: 125}
		}
	}

	var input []byte
	if *stdinPath != "" {
		contents, err := readPrivateTaskInput(*stdinPath)
		if err != nil {
			_, _ = io.WriteString(stderr, "Unable to open the private task input.\n")
			return taskExecExitError{code: 126}
		}
		input = contents
		defer clear(input)
	}

	command, err := taskExecCommand(childArguments[0], childArguments[1:])
	if err != nil {
		_, _ = io.WriteString(stderr, "Unable to prepare the task runner.\n")
		return taskExecExitError{code: 126}
	}
	if input != nil {
		command.Stdin = bytes.NewReader(input)
	}
	stdoutPipe, err := command.StdoutPipe()
	if err != nil {
		return taskExecExitError{code: 126}
	}
	stderrPipe, err := command.StderrPipe()
	if err != nil {
		return taskExecExitError{code: 126}
	}
	if err := command.Start(); err != nil {
		_, _ = io.WriteString(stderr, "Unable to start the task runner.\n")
		return taskExecExitError{code: 127}
	}

	copyErrors := make(chan error, 2)
	go func() {
		_, copyErr := io.CopyBuffer(stdout, stdoutPipe, make([]byte, 32<<10))
		copyErrors <- copyErr
	}()
	go func() {
		_, copyErr := io.CopyBuffer(stderr, stderrPipe, make([]byte, 32<<10))
		copyErrors <- copyErr
	}()
	waitErr := command.Wait()
	stdoutCopyErr, stderrCopyErr := <-copyErrors, <-copyErrors
	if stdoutCopyErr != nil || stderrCopyErr != nil {
		_, _ = io.WriteString(stderr, "Unable to read task runner output.\n")
		if waitErr == nil {
			return taskExecExitError{code: 1}
		}
	}
	if waitErr == nil {
		return nil
	}
	var exitError *exec.ExitError
	if errors.As(waitErr, &exitError) {
		code := exitError.ExitCode()
		if code < 1 || code > 255 {
			code = 1
		}
		return taskExecExitError{code: code}
	}
	_, _ = io.WriteString(stderr, "Unable to wait for the task runner.\n")
	return taskExecExitError{code: 1}
}

func openPrivateTaskInput(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || strings.IndexByte(path, 0) >= 0 {
		return nil, errors.New("task input path is invalid")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 1 || info.Size() > int64(maximumTaskDefinitionBytes) {
		return nil, errors.New("task input file is invalid")
	}
	if err := verifyStateFileSecurity(path); err != nil {
		return nil, errors.New("task input file is not private")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || openedInfo.Size() != info.Size() {
		_ = file.Close()
		return nil, errors.New("task input file changed while opening")
	}
	return file, nil
}

func readPrivateTaskInput(path string) ([]byte, error) {
	file, err := openPrivateTaskInput(path)
	if err != nil {
		return nil, err
	}
	contents, readErr := io.ReadAll(io.LimitReader(file, int64(maximumTaskDefinitionBytes)+1))
	closeErr := file.Close()
	if readErr != nil {
		clear(contents)
		return nil, readErr
	}
	if closeErr != nil {
		clear(contents)
		return nil, closeErr
	}
	if len(contents) == 0 || len(contents) > maximumTaskDefinitionBytes {
		clear(contents)
		return nil, errors.New("task input file changed while reading")
	}
	return contents, nil
}
