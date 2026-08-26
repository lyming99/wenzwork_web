//go:build !windows

package main

func defaultCommandTextFallbackEncoding() string { return "gb18030" }
