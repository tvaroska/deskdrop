//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/tvaroska/deskdrop/internal/bus"
	"github.com/tvaroska/deskdrop/internal/contract"
	"github.com/tvaroska/deskdrop/internal/driver/cdp"
)

const (
	wmDestroy      = 0x0002
	wmApp          = 0x8000
	wmTrayCallback = wmApp + 1
	wmTrayRefresh  = wmApp + 2
	wmLButtonUp    = 0x0202
	wmRButtonUp    = 0x0205
	wmContextMenu  = 0x007B

	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004
	nifInfo    = 0x00000010

	niifInfo  = 0x00000001
	niifError = 0x00000003

	mfString    = 0x00000000
	mfGrayed    = 0x00000001
	mfChecked   = 0x00000008
	mfPopup     = 0x00000010
	mfSeparator = 0x00000800

	tpmRightButton = 0x0002
	tpmBottomAlign = 0x0020
	tpmNonotify    = 0x0080
	tpmReturnCmd   = 0x0100

	mbYesNo        = 0x00000004
	mbIconQuestion = 0x00000020
	mbTopMost      = 0x00040000
	idYes          = 6

	swHide = 0

	menuIDConnectChrome = 1001
	menuIDSyncNow       = 1003
	menuIDToggleGate    = 1004
	menuIDUpdate        = 1005
	menuIDQuit          = 1009
	menuIDProfileBase   = 2000
	maxMenuProfiles     = 32
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procRegisterClassExW    = user32.NewProc("RegisterClassExW")
	procCreateWindowExW     = user32.NewProc("CreateWindowExW")
	procDefWindowProcW      = user32.NewProc("DefWindowProcW")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessageW    = user32.NewProc("DispatchMessageW")
	procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	procPostMessageW        = user32.NewProc("PostMessageW")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	procAppendMenuW         = user32.NewProc("AppendMenuW")
	procDestroyMenu         = user32.NewProc("DestroyMenu")
	procTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procCreateIcon          = user32.NewProc("CreateIcon")
	procShowWindow          = user32.NewProc("ShowWindow")
	procFindWindowW         = user32.NewProc("FindWindowW")
	procMessageBoxW         = user32.NewProc("MessageBoxW")

	procShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")
	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
)

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

type notifyIconDataW struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         guid
	HBalloonIcon     uintptr
}

type wndClassExW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type point struct {
	X int32
	Y int32
}

type winMsg struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type trayAction struct {
	cmdID   int
	profile cdp.BrowserProfile
}

type trayApp struct {
	cfgPath  string
	cdpURL   string
	interval time.Duration
	pool     *cdp.SessionPool

	hwnd      uintptr
	iconRed   uintptr
	iconGreen uintptr
	iconAmber uintptr

	mu              sync.Mutex
	browserStat     string
	lastAction      string
	selectedProfile string
	fallbackGate    string
	busy            bool
	connected       bool

	actionCh chan trayAction
	stopCh   chan struct{}
}

var activeTray *trayApp

func defaultShowsTrayOnNoArgs() bool { return true }

// confirmFallbackPlan opens a native Windows confirmation prompt for --fallback-gate=tray.
func confirmFallbackPlan(plan contract.FallbackPlan) (bool, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "DeskDrop requests %d bounded fallback action(s) on %s.\n\nURL: %s\nReason: %s\n\nActions:\n",
		len(plan.Actions), plan.AllowedDomain, plan.TargetURL, plan.Reason)
	for i, a := range plan.Actions {
		fmt.Fprintf(&b, "%d. %s (selector=%q, delta_y=%d, wait_ms=%d)\n", i+1, a.Action, a.Selector, a.DeltaY, a.WaitMs)
	}
	b.WriteString("\nOpen a temporary scratch tab, run these safe actions, and close it immediately?")
	titlePtr, _ := syscall.UTF16PtrFromString("DeskDrop — Approve Tier-2 Fallback")
	bodyPtr, _ := syscall.UTF16PtrFromString(b.String())
	ret, _, _ := procMessageBoxW.Call(0, uintptr(unsafe.Pointer(bodyPtr)), uintptr(unsafe.Pointer(titlePtr)), mbYesNo|mbIconQuestion|mbTopMost)
	return ret == idYes, nil
}

func runPlatformTray(cfgPath string, cdpURL string, interval time.Duration, fallbackGate string, keepConsole bool) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if !keepConsole {
		hideConsoleWindow()
	}
	if interval <= 0 {
		interval = 20 * time.Second
	}

	initCfg, _ := loadConfig(cfgPath)
	initSel := initCfg.PreferredChromeSession()
	if matched, ok := cdp.ResolveBrowserProfile(initCfg.Browser, initCfg.UserDataDir, initSel); ok {
		initSel = matched.DirName
	}
	gate := fallbackGate
	if gate == "" {
		gate = initCfg.NormalizedFallbackGate()
	}

	app := &trayApp{
		cfgPath:         cfgPath,
		cdpURL:          cdpURL,
		interval:        interval,
		pool:            cdp.NewSessionPool(),
		browserStat:     "Checking Chrome...",
		lastAction:      "Idle (ready)",
		selectedProfile: initSel,
		fallbackGate:    gate,
		actionCh:        make(chan trayAction, 8),
		stopCh:          make(chan struct{}),
	}
	activeTray = app
	defer app.pool.Close()

	hInst, _, _ := procGetModuleHandleW.Call(0)
	app.iconGreen = createBadgeIcon(hInst, 0x48, 0xF2, 0x92)
	app.iconAmber = createBadgeIcon(hInst, 0xF5, 0xC4, 0x51)
	app.iconRed = createBadgeIcon(hInst, 0xFF, 0x52, 0x52)

	className, _ := syscall.UTF16PtrFromString("DeskDropTrayWndClass")
	wndTitle, _ := syscall.UTF16PtrFromString("DeskDrop Tray")

	for i := 0; i < 6; i++ {
		oldHwnd, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(wndTitle)))
		if oldHwnd == 0 {
			break
		}
		procPostMessageW.Call(oldHwnd, wmDestroy, 0, 0)
		time.Sleep(150 * time.Millisecond)
	}
	unblockExecutableOnWindows()

	wc := wndClassExW{
		CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		LpfnWndProc:   syscall.NewCallback(trayWndProc),
		HInstance:     hInst,
		LpszClassName: className,
	}
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	hwnd, _, _ := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(wndTitle)), 0, 0, 0, 0, 0, 0, 0, hInst, 0)
	if hwnd == 0 {
		return fmt.Errorf("CreateWindowExW failed")
	}
	app.hwnd = hwnd

	app.addTrayIcon()
	defer app.removeTrayIcon()

	go app.workerLoop()

	var m winMsg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}

	close(app.stopCh)
	return nil
}

func hideConsoleWindow() {
	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd != 0 {
		procShowWindow.Call(hwnd, swHide)
	}
}

func trayWndProc(hwnd uintptr, msg uint32, wParam uintptr, lParam uintptr) uintptr {
	app := activeTray
	switch msg {
	case wmTrayCallback:
		low := uint32(lParam & 0xFFFF)
		if low == wmRButtonUp || low == wmLButtonUp || low == wmContextMenu {
			if app != nil {
				app.showPopupMenu()
			}
			return 0
		}
	case wmTrayRefresh:
		if app != nil {
			app.updateTrayIcon("", "", 0)
		}
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
	return ret
}

func (a *trayApp) currentIcon() uintptr {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy {
		return a.iconAmber
	}
	if a.connected {
		return a.iconGreen
	}
	return a.iconRed
}

func (a *trayApp) currentTooltip() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	tip := fmt.Sprintf("DeskDrop %s [%s]\n%s\n%s", version, a.fallbackGate, a.browserStat, a.lastAction)
	if len(tip) > 120 {
		tip = tip[:120]
	}
	return tip
}

func (a *trayApp) addTrayIcon() {
	nid := notifyIconDataW{
		CbSize:           uint32(unsafe.Sizeof(notifyIconDataW{})),
		HWnd:             a.hwnd,
		UID:              1,
		UFlags:           nifMessage | nifIcon | nifTip,
		UCallbackMessage: wmTrayCallback,
		HIcon:            a.currentIcon(),
	}
	copyUTF16(nid.SzTip[:], a.currentTooltip())
	procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
}

func (a *trayApp) updateTrayIcon(balloonTitle string, balloonBody string, infoFlags uint32) {
	nid := notifyIconDataW{
		CbSize:           uint32(unsafe.Sizeof(notifyIconDataW{})),
		HWnd:             a.hwnd,
		UID:              1,
		UFlags:           nifMessage | nifIcon | nifTip,
		UCallbackMessage: wmTrayCallback,
		HIcon:            a.currentIcon(),
	}
	copyUTF16(nid.SzTip[:], a.currentTooltip())
	if balloonTitle != "" && balloonBody != "" {
		nid.UFlags |= nifInfo
		nid.DwInfoFlags = infoFlags
		copyUTF16(nid.SzInfoTitle[:], balloonTitle)
		copyUTF16(nid.SzInfo[:], balloonBody)
	}
	procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
}

func (a *trayApp) removeTrayIcon() {
	nid := notifyIconDataW{CbSize: uint32(unsafe.Sizeof(notifyIconDataW{})), HWnd: a.hwnd, UID: 1}
	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
}

func (a *trayApp) showPopupMenu() {
	hMenu, _, _ := procCreatePopupMenu.Call()
	if hMenu == 0 {
		return
	}
	defer procDestroyMenu.Call(hMenu)

	cfg, _ := loadConfig(a.cfgPath)
	profiles := cdp.ListBrowserProfiles(cfg.Browser, cfg.UserDataDir)
	if len(profiles) > maxMenuProfiles {
		profiles = profiles[:maxMenuProfiles]
	}

	a.mu.Lock()
	bStat := a.browserStat
	lAct := a.lastAction
	selProf := a.selectedProfile
	gate := a.fallbackGate
	busy := a.busy
	a.mu.Unlock()

	appendMenuText(hMenu, mfString|mfGrayed, 0, fmt.Sprintf("DeskDrop %s — %s", version, bStat))
	appendMenuText(hMenu, mfString|mfGrayed, 0, fmt.Sprintf("Last: %s", lAct))
	procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)

	flags := uint32(mfString)
	if busy {
		flags |= mfGrayed
	}

	hProfMenu, _, _ := procCreatePopupMenu.Call()
	if hProfMenu != 0 {
		for i, p := range profiles {
			if p.IsDedicated && i > 0 {
				procAppendMenuW.Call(hProfMenu, mfSeparator, 0, 0)
			}
			pFlags := flags
			if (selProf != "" && p.DirName == selProf) || (selProf == "" && p.IsLastUsed) {
				pFlags |= mfChecked
			}
			appendMenuText(hProfMenu, pFlags, uintptr(menuIDProfileBase+i), p.DisplayName)
		}
		subLabel, _ := syscall.UTF16PtrFromString("Chrome Session (Select Profile)")
		procAppendMenuW.Call(hMenu, uintptr(flags|mfPopup), hProfMenu, uintptr(unsafe.Pointer(subLabel)))
	}

	appendMenuText(hMenu, flags, menuIDConnectChrome, "Connect to Open Chrome")
	appendMenuText(hMenu, flags, menuIDSyncNow, "Sync Queue Now")
	appendMenuText(hMenu, flags, menuIDToggleGate, fmt.Sprintf("Fallback Gate: %s (Toggle)", strings.ToUpper(gate)))
	appendMenuText(hMenu, flags, menuIDUpdate, "Update DeskDrop")
	procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)
	appendMenuText(hMenu, mfString, menuIDQuit, "Exit")

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWindow.Call(a.hwnd)

	cmd, _, _ := procTrackPopupMenu.Call(hMenu, tpmRightButton|tpmBottomAlign|tpmNonotify|tpmReturnCmd, uintptr(pt.X), uintptr(pt.Y), 0, a.hwnd, 0)
	cmdID := int(cmd)
	switch {
	case cmdID >= menuIDProfileBase && cmdID < menuIDProfileBase+len(profiles):
		select {
		case a.actionCh <- trayAction{cmdID: menuIDProfileBase, profile: profiles[cmdID-menuIDProfileBase]}:
		default:
		}
	case cmdID == menuIDConnectChrome || cmdID == menuIDSyncNow || cmdID == menuIDToggleGate || cmdID == menuIDUpdate:
		select {
		case a.actionCh <- trayAction{cmdID: cmdID}:
		default:
		}
	case cmdID == menuIDQuit:
		procDestroyWindow.Call(a.hwnd)
	}
}

func appendMenuText(hMenu uintptr, flags uint32, id uintptr, label string) {
	ptr, _ := syscall.UTF16PtrFromString(label)
	procAppendMenuW.Call(hMenu, uintptr(flags), id, uintptr(unsafe.Pointer(ptr)))
}

func (a *trayApp) workerLoop() {
	// Single-attempt startup dial; background loop stays quiet until user clicks Connect.
	a.tryConnectOnce(false)
	a.runSyncCycle(false)

	syncTicker := time.NewTicker(a.interval)
	defer syncTicker.Stop()
	probeTicker := time.NewTicker(6 * time.Second)
	defer probeTicker.Stop()

	for {
		select {
		case <-a.stopCh:
			return
		case act := <-a.actionCh:
			switch act.cmdID {
			case menuIDProfileBase:
				a.doStartChromeProfile(act.profile)
			case menuIDConnectChrome:
				a.pool.ResetApprovalLatch()
				a.tryConnectOnce(true)
			case menuIDSyncNow:
				a.runSyncCycle(true)
			case menuIDToggleGate:
				a.mu.Lock()
				if a.fallbackGate == "tray" {
					a.fallbackGate = "auto"
				} else {
					a.fallbackGate = "tray"
				}
				next := a.fallbackGate
				a.mu.Unlock()
				a.setBusy(false, "Fallback gate: "+next)
			case menuIDUpdate:
				a.doSelfUpdate(true)
			}
		case <-syncTicker.C:
			a.runSyncCycle(false)
		case <-probeTicker.C:
			a.refreshStatus()
		}
	}
}

func (a *trayApp) tryConnectOnce(notifyOnSuccess bool) {
	cfg, _ := loadConfig(a.cfgPath)
	if a.cdpURL != "" {
		cfg.CDPURL = a.cdpURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	c, err := a.pool.Acquire(ctx, cfg.CDPURL, cfg.UserDataDir)
	if err != nil {
		a.refreshStatus()
		if notifyOnSuccess {
			a.setBusy(false, "Connect failed: "+err.Error())
			a.updateTrayIcon("DeskDrop", "Could not connect to Chrome: "+err.Error(), niifError)
		}
		return
	}
	c.SetPreferredSession(cfg.Browser, cfg.UserDataDir, cfg.PreferredChromeSession())
	a.refreshStatus()
	if notifyOnSuccess {
		a.setBusy(false, "Connected to Chrome (persistent session)")
		a.updateTrayIcon("DeskDrop", "Connected to Chrome.", niifInfo)
	}
}

func (a *trayApp) refreshStatus() {
	cfg, cfgErr := loadConfig(a.cfgPath)
	if a.cdpURL != "" {
		cfg.CDPURL = a.cdpURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	conn, label := a.pool.ProbeQuiet(ctx, cfg.CDPURL)
	if cfgErr != nil {
		label = "Config error: " + cfgErr.Error()
	}
	a.mu.Lock()
	changed := (a.connected != conn || a.browserStat != label)
	a.connected = conn
	a.browserStat = label
	a.mu.Unlock()

	if changed {
		procPostMessageW.Call(a.hwnd, wmTrayRefresh, 0, 0)
		if cfgErr == nil && cfg.ServiceURL != "" && cfg.ServiceToken != "" {
			go a.sendBackgroundHeartbeat(cfg)
		}
	}
}

func (a *trayApp) sendBackgroundHeartbeat(cfg contract.Config) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	a.mu.Lock()
	busy := a.busy
	lastAct := a.lastAction
	cfg.FallbackGate = a.fallbackGate
	a.mu.Unlock()

	svcClient := bus.NewDesktopClient(cfg)
	svcClient.ClientVersion = version
	if svcClient.ValidateAuth() != nil {
		return
	}
	drv := cdp.NewDriver(a.pool, cfg)
	ws := collectWorkerStatus(ctx, drv, cfg, cfg.From, busy, lastAct, "")
	_ = svcClient.ReportWorkerStatus(ctx, ws)
}

func (a *trayApp) setBusy(busy bool, action string) {
	a.mu.Lock()
	a.busy = busy
	if action != "" {
		a.lastAction = action
	}
	a.mu.Unlock()
	procPostMessageW.Call(a.hwnd, wmTrayRefresh, 0, 0)
}

func (a *trayApp) doStartChromeProfile(prof cdp.BrowserProfile) {
	a.mu.Lock()
	a.selectedProfile = prof.DirName
	a.mu.Unlock()
	sessionVal := prof.Name
	if strings.TrimSpace(sessionVal) == "" || prof.IsDedicated {
		sessionVal = prof.DirName
	}
	_ = saveConfigChromeSession(a.cfgPath, sessionVal)
	a.setBusy(true, fmt.Sprintf("Selecting Chrome session (%s)...", prof.DisplayName))
	cfg, _ := loadConfig(a.cfgPath)
	if _, err := cdp.LaunchBrowserProfile(cfg.Browser, prof, ""); err != nil {
		a.setBusy(false, "Start Chrome failed: "+err.Error())
		return
	}
	time.Sleep(1200 * time.Millisecond)
	a.pool.ResetApprovalLatch()
	a.tryConnectOnce(false)
	a.setBusy(false, fmt.Sprintf("Session: %s", prof.DisplayName))
}

func (a *trayApp) runSyncCycle(manual bool) {
	cfg, err := loadConfig(a.cfgPath)
	if err != nil {
		a.setBusy(false, "Config error: "+err.Error())
		return
	}
	if cfg.ServiceURL == "" || cfg.ServiceToken == "" {
		return
	}
	if a.cdpURL != "" {
		cfg.CDPURL = a.cdpURL
	}
	a.mu.Lock()
	cfg.FallbackGate = a.fallbackGate
	a.mu.Unlock()

	svcClient := bus.NewDesktopClient(cfg)
	svcClient.ClientVersion = version
	if svcClient.ValidateAuth() != nil {
		return
	}
	if manual {
		a.setBusy(true, "Checking relay queue...")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	drv := cdp.NewDriver(a.pool, cfg)
	approver := makeFallbackApprover(cfg.NormalizedFallbackGate())
	drained, updated, derr := runTraySyncStep(ctx, drv, cfg, svcClient, resolveHostLabel(cfg), approver, func(_ string) bool {
		return a.doSelfUpdate(false)
	})
	if updated {
		return
	}
	if errors.Is(derr, errRelayRestarting) {
		a.setBusy(false, "Relay restarting")
		return
	}
	a.refreshStatus()
	stamp := time.Now().Format("15:04:05")
	if derr != nil {
		a.setBusy(false, fmt.Sprintf("Sync err (%s): %v", stamp, derr))
		return
	}
	if drained > 0 {
		a.setBusy(false, fmt.Sprintf("Synced %d task(s) at %s", drained, stamp))
	} else if manual {
		a.setBusy(false, fmt.Sprintf("Queue empty (%s)", stamp))
	}
}

func (a *trayApp) doSelfUpdate(manual bool) bool {
	cfg, err := loadConfig(a.cfgPath)
	if err != nil {
		return false
	}
	svcClient := bus.NewDesktopClient(cfg)
	svcClient.ClientVersion = version
	if svcClient.ValidateAuth() != nil {
		return false
	}
	a.setBusy(true, "Updating DeskDrop...")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	newVer, uerr := performSelfUpdate(ctx, svcClient)
	if uerr != nil {
		a.setBusy(false, "Update failed: "+uerr.Error())
		if manual {
			a.updateTrayIcon("DeskDrop Update Failed", uerr.Error(), niifError)
		}
		return false
	}
	if serr := spawnDetachedSelf(os.Args[1:]); serr != nil {
		a.setBusy(false, "Update restart failed: "+serr.Error())
		a.updateTrayIcon("DeskDrop Update Restart Failed", serr.Error(), niifError)
		return false
	}
	a.updateTrayIcon("DeskDrop Updated", fmt.Sprintf("Updated to %s — restarting...", newVer), niifInfo)
	time.Sleep(300 * time.Millisecond)
	procPostMessageW.Call(a.hwnd, wmDestroy, 0, 0)
	return true
}

func copyUTF16(dst []uint16, s string) {
	u, err := syscall.UTF16FromString(s)
	if err != nil {
		return
	}
	if len(u) > len(dst) {
		copy(dst, u[:len(dst)-1])
		dst[len(dst)-1] = 0
		return
	}
	copy(dst, u)
}

func createBadgeIcon(hInst uintptr, r byte, g byte, b byte) uintptr {
	const size = 16
	var andMask [size * 4]byte
	for i := range andMask {
		andMask[i] = 0xFF
	}
	var xorMask [size * size * 4]byte
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx := float64(x) - 7.5
			dy := float64(y) - 7.5
			distSq := dx*dx + dy*dy
			idx := (y*size + x) * 4
			if distSq <= 49.0 {
				andMask[y*4+(x/8)] &^= (0x80 >> (x % 8))
				if distSq >= 34.0 {
					xorMask[idx+0] = 0x19
					xorMask[idx+1] = 0x11
					xorMask[idx+2] = 0x0C
					xorMask[idx+3] = 0xFF
				} else {
					xorMask[idx+0] = b
					xorMask[idx+1] = g
					xorMask[idx+2] = r
					xorMask[idx+3] = 0xFF
				}
			}
		}
	}
	icon, _, _ := procCreateIcon.Call(hInst, size, size, 1, 32, uintptr(unsafe.Pointer(&andMask[0])), uintptr(unsafe.Pointer(&xorMask[0])))
	return icon
}
