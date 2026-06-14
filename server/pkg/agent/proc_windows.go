//go:build windows

package agent

import (
	"os/exec"
	"syscall"
)

// createNewConsole allocates a fresh console for the child process. Combined
// with HideWindow=true (STARTF_USESHOWWINDOW + SW_HIDE) the console window
// stays off-screen, and — critically — any grandchildren the agent spawns
// (tool subprocesses like bash, cmd, netstat, findstr) inherit this hidden
// console instead of each allocating their own visible one.
//
// Using CREATE_NO_WINDOW here instead would strip the console entirely,
// which forces Windows to allocate a new visible console per grandchild
// when the grandchild is a console-subsystem program that doesn't itself
// pass CREATE_NO_WINDOW — the exact popup storm reported in #1521.
const createNewConsole = 0x00000010

// createNoWindow suppresses the console entirely. Use only for agents that
// run in strict print mode (-p) and never spawn interactive grandchildren,
// where the absence of a console is required to force stdout through the pipe
// (programs using WriteConsole() bypass pipes and write to the console handle
// directly — CREATE_NO_WINDOW removes that handle).
const createNoWindow = 0x08000000

// hideAgentWindow configures cmd to suppress the console window on Windows
// while still giving descendant processes a hidden console to inherit.
// Stdio pipes set via cmd.StdoutPipe/StdinPipe keep working because
// STARTF_USESTDHANDLES takes precedence over the new console's stdio.
func hideAgentWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNewConsole
}

// hideAgentWindowNoConsole is like hideAgentWindow but uses CREATE_NO_WINDOW
// instead of CREATE_NEW_CONSOLE. This forces the child process to write stdout
// through the pipe rather than via WriteConsole() to a console handle. Use
// this for print-mode agents (agy -p) that do not spawn interactive
// grandchildren, where pipe capture is required and the popup-storm risk from
// #1521 does not apply.
func hideAgentWindowNoConsole(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
