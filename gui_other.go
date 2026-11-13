//go:build !windows

package main

import "fmt"

// 非 Windows 平台不提供 GUI, 无参数时退化为命令行处理当前目录
func guiAvailable() bool { return false }

func ensureConsole() bool { return false }

func runGUI() { fmt.Println("GUI 仅支持 Windows, 请使用命令行参数") }
