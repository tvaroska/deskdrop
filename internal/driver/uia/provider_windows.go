//go:build windows

package uia

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"github.com/tvaroska/deskdrop/internal/contract"
)

const (
	coinitMultithreaded   = 0x0
	clsctxInprocServer    = 0x1
	sOK                   = 0x00000000
	sFalse                = 0x00000001
	rpcEChangedMode       = 0x80010106
	uiaValuePatternID     = 10002
	maxUIATreeWalkDepth   = 14
	maxTopWindowScanCount = 256
)

type comGUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	// CLSID_CUIAutomation {ff48dba4-60ef-4201-aa87-54103eef594e}
	clsidCUIAutomation = comGUID{0xff48dba4, 0x60ef, 0x4201, [8]byte{0xaa, 0x87, 0x54, 0x10, 0x3e, 0xef, 0x59, 0x4e}}
	// IID_IUIAutomation {30cbe57d-d9d0-452a-ab13-7ac5ac4825ee}
	iidIUIAutomation = comGUID{0x30cbe57d, 0xd9d0, 0x452a, [8]byte{0xab, 0x13, 0x7a, 0xc5, 0xac, 0x48, 0x25, 0xee}}
	// IID_IUIAutomationValuePattern {a94cd8b1-0844-4cd6-9d2d-640537ab39e9}
	iidIUIAutomationValuePattern = comGUID{0xa94cd8b1, 0x0844, 0x4cd6, [8]byte{0x9d, 0x2d, 0x64, 0x05, 0x37, 0xab, 0x39, 0xe9}}

	ole32    = syscall.NewLazyDLL("ole32.dll")
	oleaut32 = syscall.NewLazyDLL("oleaut32.dll")

	procCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	procCoUninitialize   = ole32.NewProc("CoUninitialize")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")

	procSysFreeString = oleaut32.NewProc("SysFreeString")
	procSysStringLen  = oleaut32.NewProc("SysStringLen")
)

type iUnknownVtbl struct {
	QueryInterface uintptr
	AddRef         uintptr
	Release        uintptr
}

// iUIAutomationVtbl matches the frozen Windows SDK UIAutomationClient.h IUIAutomation vtable.
type iUIAutomationVtbl struct {
	iUnknownVtbl
	CompareElements             uintptr
	CompareRuntimeIds           uintptr
	GetRootElement              uintptr
	ElementFromHandle           uintptr
	ElementFromPoint            uintptr
	GetFocusedElement           uintptr
	GetRootElementBuildCache    uintptr
	ElementFromHandleBuildCache uintptr
	ElementFromPointBuildCache  uintptr
	GetFocusedElementBuildCache uintptr
	CreateTreeWalker            uintptr
	GetControlViewWalker        uintptr
	GetContentViewWalker        uintptr
	GetRawViewWalker            uintptr
}

type iUIAutomation struct {
	lpVtbl *iUIAutomationVtbl
}

type iUIAutomationTreeWalkerVtbl struct {
	iUnknownVtbl
	GetParentElement          uintptr
	GetFirstChildElement      uintptr
	GetLastChildElement       uintptr
	GetNextSiblingElement     uintptr
	GetPreviousSiblingElement uintptr
	NormalizeElement          uintptr
}

type iUIAutomationTreeWalker struct {
	lpVtbl *iUIAutomationTreeWalkerVtbl
}

// iUIAutomationElementVtbl matches UIAutomationClient.h IUIAutomationElement slots 0..36.
type iUIAutomationElementVtbl struct {
	iUnknownVtbl
	SetFocus                       uintptr
	GetRuntimeId                   uintptr
	FindFirst                      uintptr
	FindAll                        uintptr
	FindFirstBuildCache            uintptr
	FindAllBuildCache              uintptr
	BuildUpdatedCache              uintptr
	GetCurrentPropertyValue        uintptr
	GetCurrentPropertyValueEx      uintptr
	GetCachedPropertyValue         uintptr
	GetCachedPropertyValueEx       uintptr
	GetCurrentPatternAs            uintptr
	GetCachedPatternAs             uintptr
	GetCurrentPattern              uintptr
	GetCachedPattern               uintptr
	GetCachedParent                uintptr
	GetCachedChildren              uintptr
	GetCurrentProcessId            uintptr
	GetCurrentControlType          uintptr
	GetCurrentLocalizedControlType uintptr
	GetCurrentName                 uintptr
	GetCurrentAcceleratorKey       uintptr
	GetCurrentAccessKey            uintptr
	GetCurrentHasKeyboardFocus     uintptr
	GetCurrentIsKeyboardFocusable  uintptr
	GetCurrentIsEnabled            uintptr
	GetCurrentAutomationId         uintptr
	GetCurrentClassName            uintptr
	GetCurrentHelpText             uintptr
	GetCurrentCulture              uintptr
	GetCurrentIsControlElement     uintptr
	GetCurrentIsContentElement     uintptr
	GetCurrentIsPassword           uintptr
	GetCurrentNativeWindowHandle   uintptr
}

type iUIAutomationElement struct {
	lpVtbl *iUIAutomationElementVtbl
}

type iUIAutomationValuePatternVtbl struct {
	iUnknownVtbl
	SetValue        uintptr
	GetCurrentValue uintptr
}

type iUIAutomationValuePattern struct {
	lpVtbl *iUIAutomationValuePatternVtbl
}

type win32Provider struct{}

func newPlatformProvider() TreeProvider {
	return win32Provider{}
}

func (win32Provider) Available() (bool, string) {
	return true, "Connected (Windows UIA COM)"
}

func (win32Provider) Close() error {
	return nil
}

func (win32Provider) ListWindows(ctx context.Context) ([]WindowSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []WindowSummary
	err := withUIAutomation(func(uia *iUIAutomation, walker *iUIAutomationTreeWalker, root *iUIAutomationElement) error {
		cur, err := walkerFirstChild(walker, root)
		if err != nil {
			return err
		}
		scanned := 0
		for cur != nil && scanned < maxTopWindowScanCount {
			if ctx.Err() != nil {
				releaseElement(cur)
				return ctx.Err()
			}
			scanned++
			title := strings.TrimSpace(elemName(cur))
			cls := strings.TrimSpace(elemClassName(cur))
			autoID := strings.TrimSpace(elemAutomationID(cur))
			if title != "" {
				out = append(out, WindowSummary{
					Title:        title,
					ClassName:    cls,
					AutomationID: autoID,
				})
			}
			next, _ := walkerNextSibling(walker, cur)
			releaseElement(cur)
			cur = next
		}
		if cur != nil {
			releaseElement(cur)
		}
		return ctx.Err()
	})
	return out, err
}

func (win32Provider) CaptureWindowTree(ctx context.Context, windowQuery string, maxNodes int) ([]contract.AXNode, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if maxNodes <= 0 {
		maxNodes = DefaultMaxNodes
	}
	var (
		nodes        []contract.AXNode
		matchedTitle string
	)

	err := withUIAutomation(func(_ *iUIAutomation, walker *iUIAutomationTreeWalker, root *iUIAutomationElement) error {
		q := strings.ToLower(strings.TrimSpace(windowQuery))
		if q == "" || q == "*" || q == "desktop" {
			matchedTitle = "Desktop"
			var werr error
			nodes, werr = walkSubtree(ctx, walker, root, maxNodes, maxUIATreeWalkDepth)
			return werr
		}

		var target *iUIAutomationElement
		cur, err := walkerFirstChild(walker, root)
		if err != nil {
			return err
		}
		scanned := 0
		for cur != nil && scanned < maxTopWindowScanCount {
			if ctx.Err() != nil {
				releaseElement(cur)
				return ctx.Err()
			}
			scanned++
			title := strings.TrimSpace(elemName(cur))
			cls := strings.TrimSpace(elemClassName(cur))
			autoID := strings.TrimSpace(elemAutomationID(cur))
			if strings.Contains(strings.ToLower(title), q) ||
				strings.Contains(strings.ToLower(autoID), q) ||
				strings.Contains(strings.ToLower(cls), q) {
				target = cur
				matchedTitle = title
				break
			}
			next, _ := walkerNextSibling(walker, cur)
			releaseElement(cur)
			cur = next
		}
		if target == nil && cur != nil {
			releaseElement(cur)
		}
		if err := ctx.Err(); err != nil {
			if target != nil {
				releaseElement(target)
			}
			return err
		}
		if target == nil {
			return fmt.Errorf("no open desktop window matched %q", windowQuery)
		}
		defer releaseElement(target)

		var werr error
		nodes, werr = walkSubtree(ctx, walker, target, maxNodes, maxUIATreeWalkDepth)
		return werr
	})
	if err != nil {
		return nil, "", err
	}
	return nodes, matchedTitle, nil
}

func withUIAutomation(fn func(uia *iUIAutomation, walker *iUIAutomationTreeWalker, root *iUIAutomationElement) error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hr, _, _ := procCoInitializeEx.Call(0, coinitMultithreaded)
	hrCode := uint32(hr)
	if hrCode == sOK || hrCode == sFalse {
		defer procCoUninitialize.Call()
	} else if hrCode != rpcEChangedMode {
		return fmt.Errorf("CoInitializeEx failed (HRESULT 0x%08x)", hrCode)
	}

	var uia *iUIAutomation
	hrCreate, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidCUIAutomation)),
		0,
		clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidIUIAutomation)),
		uintptr(unsafe.Pointer(&uia)),
	)
	if int32(hrCreate) < 0 || uia == nil {
		return fmt.Errorf("CoCreateInstance(CUIAutomation) failed (HRESULT 0x%08x)", uint32(hrCreate))
	}
	defer syscall.SyscallN(uia.lpVtbl.Release, uintptr(unsafe.Pointer(uia)))

	var walker *iUIAutomationTreeWalker
	hrWalker, _, _ := syscall.SyscallN(
		uia.lpVtbl.GetControlViewWalker,
		uintptr(unsafe.Pointer(uia)),
		uintptr(unsafe.Pointer(&walker)),
	)
	if int32(hrWalker) < 0 || walker == nil {
		return fmt.Errorf("IUIAutomation.GetControlViewWalker failed (HRESULT 0x%08x)", uint32(hrWalker))
	}
	defer syscall.SyscallN(walker.lpVtbl.Release, uintptr(unsafe.Pointer(walker)))

	var root *iUIAutomationElement
	hrRoot, _, _ := syscall.SyscallN(
		uia.lpVtbl.GetRootElement,
		uintptr(unsafe.Pointer(uia)),
		uintptr(unsafe.Pointer(&root)),
	)
	if int32(hrRoot) < 0 || root == nil {
		return errors.New("IUIAutomation.GetRootElement returned nil")
	}
	defer releaseElement(root)

	return fn(uia, walker, root)
}

func walkSubtree(ctx context.Context, walker *iUIAutomationTreeWalker, start *iUIAutomationElement, maxNodes int, maxDepth int) ([]contract.AXNode, error) {
	if start == nil {
		return nil, nil
	}
	nextSeq := 0

	makeNode := func(el *iUIAutomationElement, parentID string) contract.AXNode {
		nextSeq++
		id := "uia_" + strconv.Itoa(nextSeq)
		ctrlType := elemControlType(el)
		role := mapUIAControlType(ctrlType)
		isPwd := elemIsPassword(el)
		if isPwd {
			role = "password"
		}
		name := elemName(el)
		autoID := elemAutomationID(el)
		cls := elemClassName(el)
		enabled := elemIsEnabled(el)

		val := ""
		if isPwd {
			val = "[REDACTED]"
		} else if role == "edit" || role == "combobox" || role == "spinbutton" {
			if rawVal := elemValuePattern(el); rawVal != "" {
				val = "[REDACTED]"
			}
		} else if role == "progressbar" || role == "slider" || role == "text" || role == "dataitem" {
			val = elemValuePattern(el)
		}

		var props []contract.AXProperty
		if !enabled {
			props = append(props, contract.AXProperty{
				Name:  "disabled",
				Value: contract.AXValue{Type: "boolean", Value: true},
			})
		}
		node := contract.AXNode{
			NodeID:       id,
			ParentID:     parentID,
			AutomationID: autoID,
			ClassName:    cls,
			IsPassword:   isPwd,
			Role:         &contract.AXValue{Type: "role", Value: role},
			Properties:   props,
		}
		if name != "" {
			node.Name = &contract.AXValue{Type: "string", Value: name}
		}
		if val != "" {
			node.Value = &contract.AXValue{Type: "string", Value: val}
		}
		return node
	}

	return walkUIASubtree(ctx, start, maxNodes, maxDepth, uiaWalkOps[*iUIAutomationElement]{
		firstChild: func(el *iUIAutomationElement) (*iUIAutomationElement, error) {
			return walkerFirstChild(walker, el)
		},
		nextSibling: func(el *iUIAutomationElement) (*iUIAutomationElement, error) {
			return walkerNextSibling(walker, el)
		},
		makeNode: makeNode,
		release:  releaseElement,
	})
}

func walkerFirstChild(walker *iUIAutomationTreeWalker, el *iUIAutomationElement) (*iUIAutomationElement, error) {
	var child *iUIAutomationElement
	hr, _, _ := syscall.SyscallN(
		walker.lpVtbl.GetFirstChildElement,
		uintptr(unsafe.Pointer(walker)),
		uintptr(unsafe.Pointer(el)),
		uintptr(unsafe.Pointer(&child)),
	)
	if int32(hr) < 0 {
		return nil, fmt.Errorf("GetFirstChildElement failed (0x%08x)", uint32(hr))
	}
	return child, nil
}

func walkerNextSibling(walker *iUIAutomationTreeWalker, el *iUIAutomationElement) (*iUIAutomationElement, error) {
	var sib *iUIAutomationElement
	hr, _, _ := syscall.SyscallN(
		walker.lpVtbl.GetNextSiblingElement,
		uintptr(unsafe.Pointer(walker)),
		uintptr(unsafe.Pointer(el)),
		uintptr(unsafe.Pointer(&sib)),
	)
	if int32(hr) < 0 {
		return nil, fmt.Errorf("GetNextSiblingElement failed (0x%08x)", uint32(hr))
	}
	return sib, nil
}

func releaseElement(el *iUIAutomationElement) {
	if el != nil && el.lpVtbl != nil {
		syscall.SyscallN(el.lpVtbl.Release, uintptr(unsafe.Pointer(el)))
	}
}

func elemControlType(el *iUIAutomationElement) int32 {
	var ct int32
	hr, _, _ := syscall.SyscallN(el.lpVtbl.GetCurrentControlType, uintptr(unsafe.Pointer(el)), uintptr(unsafe.Pointer(&ct)))
	if int32(hr) < 0 {
		return 0
	}
	return ct
}

func elemName(el *iUIAutomationElement) string {
	var bstr *uint16
	hr, _, _ := syscall.SyscallN(el.lpVtbl.GetCurrentName, uintptr(unsafe.Pointer(el)), uintptr(unsafe.Pointer(&bstr)))
	if int32(hr) < 0 {
		return ""
	}
	return readAndFreeBSTR(bstr)
}

func elemAutomationID(el *iUIAutomationElement) string {
	var bstr *uint16
	hr, _, _ := syscall.SyscallN(el.lpVtbl.GetCurrentAutomationId, uintptr(unsafe.Pointer(el)), uintptr(unsafe.Pointer(&bstr)))
	if int32(hr) < 0 {
		return ""
	}
	return readAndFreeBSTR(bstr)
}

func elemClassName(el *iUIAutomationElement) string {
	var bstr *uint16
	hr, _, _ := syscall.SyscallN(el.lpVtbl.GetCurrentClassName, uintptr(unsafe.Pointer(el)), uintptr(unsafe.Pointer(&bstr)))
	if int32(hr) < 0 {
		return ""
	}
	return readAndFreeBSTR(bstr)
}

func elemIsEnabled(el *iUIAutomationElement) bool {
	var flag int32
	hr, _, _ := syscall.SyscallN(el.lpVtbl.GetCurrentIsEnabled, uintptr(unsafe.Pointer(el)), uintptr(unsafe.Pointer(&flag)))
	if int32(hr) < 0 {
		return true
	}
	return flag != 0
}

func elemIsPassword(el *iUIAutomationElement) bool {
	var flag int32
	hr, _, _ := syscall.SyscallN(el.lpVtbl.GetCurrentIsPassword, uintptr(unsafe.Pointer(el)), uintptr(unsafe.Pointer(&flag)))
	if int32(hr) < 0 {
		return false
	}
	return flag != 0
}

func elemValuePattern(el *iUIAutomationElement) string {
	var pat *iUIAutomationValuePattern
	hr, _, _ := syscall.SyscallN(
		el.lpVtbl.GetCurrentPatternAs,
		uintptr(unsafe.Pointer(el)),
		uiaValuePatternID,
		uintptr(unsafe.Pointer(&iidIUIAutomationValuePattern)),
		uintptr(unsafe.Pointer(&pat)),
	)
	if int32(hr) < 0 || pat == nil || pat.lpVtbl == nil {
		return ""
	}
	defer syscall.SyscallN(pat.lpVtbl.Release, uintptr(unsafe.Pointer(pat)))

	var bstr *uint16
	hrVal, _, _ := syscall.SyscallN(
		pat.lpVtbl.GetCurrentValue,
		uintptr(unsafe.Pointer(pat)),
		uintptr(unsafe.Pointer(&bstr)),
	)
	if int32(hrVal) < 0 {
		return ""
	}
	return readAndFreeBSTR(bstr)
}

func readAndFreeBSTR(bstr *uint16) string {
	if bstr == nil {
		return ""
	}
	addr := uintptr(unsafe.Pointer(bstr))
	defer procSysFreeString.Call(addr)

	length, _, _ := procSysStringLen.Call(addr)
	if length == 0 {
		return ""
	}
	if length > 65536 {
		length = 65536
	}
	u16 := unsafe.Slice(bstr, int(length))
	return contract.NormalizeWhitespace(string(utf16.Decode(u16)))
}

func mapUIAControlType(id int32) string {
	switch id {
	case 50000:
		return "button"
	case 50001:
		return "calendar"
	case 50002:
		return "checkbox"
	case 50003:
		return "combobox"
	case 50004:
		return "edit"
	case 50005:
		return "link"
	case 50006:
		return "image"
	case 50007:
		return "listitem"
	case 50008:
		return "list"
	case 50009:
		return "menu"
	case 50010:
		return "menubar"
	case 50011:
		return "menuitem"
	case 50012:
		return "progressbar"
	case 50013:
		return "radiobutton"
	case 50014:
		return "scrollbar"
	case 50015:
		return "slider"
	case 50016:
		return "spinbutton"
	case 50017:
		return "statusbar"
	case 50018:
		return "tab"
	case 50019:
		return "tabitem"
	case 50020:
		return "text"
	case 50021:
		return "toolbar"
	case 50022:
		return "tooltip"
	case 50023:
		return "tree"
	case 50024:
		return "treeitem"
	case 50026:
		return "group"
	case 50027:
		return "thumb"
	case 50028:
		return "datagrid"
	case 50029:
		return "dataitem"
	case 50030:
		return "document"
	case 50031:
		return "splitbutton"
	case 50032:
		return "window"
	case 50033:
		return "pane"
	case 50034:
		return "header"
	case 50035:
		return "headeritem"
	case 50036:
		return "table"
	case 50037:
		return "titlebar"
	case 50038:
		return "separator"
	case 50040:
		return "appbar"
	default:
		return "custom"
	}
}
