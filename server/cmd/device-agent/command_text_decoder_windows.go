//go:build windows

package main

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func defaultCommandTextFallbackEncoding() string {
	codePage := windows.GetACP()
	if codePage == 0 || codePage == 65001 {
		return "gb18030"
	}
	return fmt.Sprintf("windows-acp:%d", codePage)
}
