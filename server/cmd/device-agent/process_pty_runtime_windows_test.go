//go:build windows

package main

import (
	"testing"
)

func TestWindowsSupervisedPTYAttributesHideConsoleWindow(t *testing.T) {
	attributes := windowsSupervisedPTYAttributes()
	if attributes == nil || !attributes.HideWindow {
		t.Fatalf("ConPTY startup attributes = %#v, want HideWindow=true", attributes)
	}
	if attributes.CreationFlags&windowsCreateNoWindow != 0 {
		t.Fatalf("ConPTY startup flags = %#x, CREATE_NO_WINDOW breaks pseudo-console output", attributes.CreationFlags)
	}
}
