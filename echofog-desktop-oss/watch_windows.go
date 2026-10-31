//go:build windows && !dev

package main

import (
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/windows"
)

// watch runs the window in a second copy of this program and waits for it: a crash kills a process
// before it can show anything, so only another process can tell the user what happened. It
// returns false when this process should run the window itself (ECHOFOG_NO_WATCH is set, or the
// copy did not start), true once the window is gone for good.
func watch() bool {
	if os.Getenv("ECHOFOG_NO_WATCH") != "" {
		return false
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	return watchLoop(exe, os.Args[1:], askRestart)
}

// watchLoop starts exe with args to run the window and starts it again, without args (a link is
// not opened twice), each time it crashed and ask says so. A copy that ends any other way, or is
// killed, is not a crash: crash.log did not grow.
func watchLoop(exe string, args []string, ask func(CrashReport) bool) bool {
	for first := true; ; first = false {
		log := crashLogPath()
		before := fileSize(log)
		cmd := exec.Command(exe, append([]string{appChildFlag}, args...)...)
		if err := cmd.Start(); err != nil {
			return !first
		}
		if err := cmd.Wait(); err == nil {
			return true
		}
		text := readCrashFrom(log, before)
		if text == "" || !ask(crashReport(text)) {
			return true
		}
		args = nil
	}
}

// askRestart shows the crash in a system dialog and asks whether to start EchoFog again.
func askRestart(r CrashReport) bool {
	ru := systemSpeaksRussian()
	var b strings.Builder
	if ru {
		b.WriteString("EchoFog закрылся из-за ошибки.\n\n")
	} else {
		b.WriteString("EchoFog closed because of an error.\n\n")
	}
	b.WriteString(clip(r.Reason, 300))
	if r.Where != "" {
		b.WriteString("\n" + clip(r.Where, 300))
	}
	if ru {
		b.WriteString("\n\nПодробности сохранены в " + crashLogPath() + ". При следующем запуске приложение предложит отчёт об ошибке.\n\nЗапустить EchoFog снова?")
	} else {
		b.WriteString("\n\nThe details are in " + crashLogPath() + ". The next start offers a bug report.\n\nStart EchoFog again?")
	}
	text, err := windows.UTF16PtrFromString(b.String())
	if err != nil {
		return false
	}
	title, _ := windows.UTF16PtrFromString("EchoFog")
	ret, _ := windows.MessageBox(0, text, title, windows.MB_YESNO|windows.MB_ICONERROR|windows.MB_SETFOREGROUND|windows.MB_TOPMOST)
	return ret == 6 // IDYES
}

// systemSpeaksRussian: the interface languages the window shows Russian for (i18n/index.js).
func systemSpeaksRussian() bool {
	langs, err := windows.GetUserPreferredUILanguages(windows.MUI_LANGUAGE_NAME)
	if err != nil || len(langs) == 0 {
		return false
	}
	l := strings.ToLower(langs[0])
	for _, p := range []string{"ru", "be", "uk", "kk"} {
		if l == p || strings.HasPrefix(l, p+"-") {
			return true
		}
	}
	return false
}
