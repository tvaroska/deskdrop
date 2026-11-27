package uia

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/tvaroska/deskdrop/internal/contract"
	"github.com/tvaroska/deskdrop/internal/driver"
)

func axVal(v any) *contract.AXValue {
	return &contract.AXValue{Value: v}
}

func sampleTaskManagerTree() []contract.AXNode {
	return []contract.AXNode{
		{
			NodeID:       "w1",
			Role:         axVal("ControlType.Window"),
			Name:         axVal("Task Manager"),
			AutomationID: "TaskManagerMain",
			ClassName:    "TaskManagerWindow",
			ChildIDs:     []string{"kpi_cpu", "kpi_mem", "search_edit", "pwd_box", "grid1"},
		},
		{
			NodeID:       "kpi_cpu",
			ParentID:     "w1",
			Role:         axVal("Text"),
			AutomationID: "CpuUtilizationText",
			Name:         axVal("CPU 18%"),
		},
		{
			NodeID:       "kpi_mem",
			ParentID:     "w1",
			Role:         axVal("Text"),
			AutomationID: "MemoryUsageText",
			Name:         axVal("Memory 14.2 GB"),
		},
		{
			NodeID:       "search_edit",
			ParentID:     "w1",
			Role:         axVal("Edit"),
			AutomationID: "FilterBox",
			Name:         axVal("Filter processes"),
			Value:        axVal("private-typed-filter-query"),
		},
		{
			NodeID:       "pwd_box",
			ParentID:     "w1",
			Role:         axVal("Edit"),
			AutomationID: "AdminPassInput",
			IsPassword:   true,
			Name:         axVal("Elevation Password"),
			Value:        axVal("hunter2-top-secret"),
		},
		{
			NodeID:       "grid1",
			ParentID:     "w1",
			Role:         axVal("DataGrid"),
			AutomationID: "ProcessTable",
			ChildIDs:     []string{"hdr1", "hdr2", "row1", "row2"},
		},
		{NodeID: "hdr1", ParentID: "grid1", Role: axVal("HeaderItem"), Name: axVal("Process")},
		{NodeID: "hdr2", ParentID: "grid1", Role: axVal("HeaderItem"), Name: axVal("Memory (MB)")},
		{
			NodeID:   "row1",
			ParentID: "grid1",
			Role:     axVal("DataItem"),
			Name:     axVal("deskdrop.exe"),
			ChildIDs: []string{"r1_name", "r1_mem"},
		},
		{NodeID: "r1_name", ParentID: "row1", Role: axVal("Text"), AutomationID: "ColProcessName", Name: axVal("deskdrop.exe")},
		{NodeID: "r1_mem", ParentID: "row1", Role: axVal("Text"), AutomationID: "ColMemoryMB", Name: axVal("24.5 MB")},
		{
			NodeID:   "row2",
			ParentID: "grid1",
			Role:     axVal("DataItem"),
			Name:     axVal("chrome.exe"),
			ChildIDs: []string{"r2_name", "r2_mem"},
		},
		{NodeID: "r2_name", ParentID: "row2", Role: axVal("Text"), AutomationID: "ColProcessName", Name: axVal("chrome.exe")},
		{NodeID: "r2_mem", ParentID: "row2", Role: axVal("Text"), AutomationID: "ColMemoryMB", Name: axVal("612 MB")},
	}
}

func TestUIATargetDriverConformanceAndRedaction(t *testing.T) {
	provider := &StaticTreeProvider{
		Windows: []StaticWindowFixture{
			{
				Title:        "Task Manager",
				ClassName:    "TaskManagerWindow",
				AutomationID: "TaskManagerMain",
				Nodes:        sampleTaskManagerTree(),
			},
		},
	}
	drv := NewDriverWithProvider(provider)
	defer drv.Close()

	if drv.Name() != "uia" {
		t.Fatalf("drv.Name() = %q, want uia", drv.Name())
	}

	st := drv.ProbeStatus(context.Background())
	if !st.Connected || st.OpenTabsCount != 1 {
		t.Fatalf("unexpected ProbeStatus: %+v", st)
	}

	step := contract.Step{
		ID:          "processes",
		WindowTitle: "Task Manager",
		Fields: map[string]contract.FieldRule{
			"cpu_pct": {
				Selector: `[automation_id='CpuUtilizationText']`,
				Regex:    `(\d+)%`,
				Parse:    "number",
			},
			"memory_gb": {
				Selector: `#MemoryUsageText`,
				Regex:    `([\d.]+)\s*GB`,
				Parse:    "float",
			},
			"filter_value": {
				Selector: `edit#FilterBox`,
				Attr:     "value",
			},
			"password_value": {
				Selector: `#AdminPassInput`,
				Attr:     "value",
			},
		},
		Items: &contract.ItemRule{
			Container: `datagrid#ProcessTable > dataitem`,
			DedupeBy:  "process",
			Fields: map[string]contract.FieldRule{
				"process": {
					Selector: `[automation_id='ColProcessName']`,
				},
				"memory_mb": {
					Selector: `[automation_id='ColMemoryMB']`,
					Regex:    `([\d,.]+)\s*MB`,
					Parse:    "float",
				},
			},
		},
		Expect: contract.Expectation{
			MinItems:       2,
			MinTables:      1,
			RequiredFields: []string{"cpu_pct", "memory_gb"},
		},
	}

	res, err := drv.ExecuteStep(context.Background(), step)
	if err != nil {
		t.Fatalf("ExecuteStep failed: %v", err)
	}
	if res.Fields["cpu_pct"] != int64(18) || res.Fields["memory_gb"] != float64(14.2) {
		t.Fatalf("unexpected extracted fields: %+v", res.Fields)
	}
	if res.Fields["filter_value"] != "[REDACTED]" || res.Fields["password_value"] != "[REDACTED]" {
		t.Fatalf("expected edit and password values to be [REDACTED], got %+v", res.Fields)
	}
	for _, secret := range []string{"private-typed-filter-query", "hunter2-top-secret"} {
		if strings.Contains(res.TextExcerpt, secret) {
			t.Fatalf("TextExcerpt leaked sensitive input %q: %q", secret, res.TextExcerpt)
		}
	}
	if len(res.Items) != 2 || res.Items[0]["process"] != "deskdrop.exe" || res.Items[0]["memory_mb"] != float64(24.5) {
		t.Fatalf("unexpected extracted items: %+v", res.Items)
	}

	// Expectation failure packages scrubbed axtree.txt into StepExecError without leaking secrets.
	failStep := step
	failStep.Expect.MinItems = 99
	_, ferr := drv.ExecuteStep(context.Background(), failStep)
	var execErr *driver.StepExecError
	if !errors.As(ferr, &execErr) {
		t.Fatalf("expected *driver.StepExecError on expectation failure, got %v", ferr)
	}
	if !strings.Contains(execErr.AXTreeText, `id="CpuUtilizationText"`) {
		t.Fatalf("expected automationId in compact AXTreeText:\n%s", execErr.AXTreeText)
	}
	if strings.Contains(execErr.AXTreeText, "hunter2-top-secret") || strings.Contains(execErr.AXTreeText, "private-typed-filter-query") {
		t.Fatalf("diagnostic AXTreeText leaked sensitive values:\n%s", execErr.AXTreeText)
	}

	// Non-Windows default platform provider fails cleanly without panicking.
	if runtime.GOOS != "windows" {
		platDrv := NewDriver(contract.Config{Driver: "uia"})
		pst := platDrv.ProbeStatus(context.Background())
		if pst.Connected {
			t.Fatalf("expected non-Windows stub provider Connected=false, got %+v", pst)
		}
	}
}

func TestM82WalkUIASubtreeMaxNodesTruncationContextCancelAndHandleRelease(t *testing.T) {
	// Tree of 5 integer handles: 1 -> [2, 3], 2 -> [4, 5].
	children := map[int][]int{
		1: {2, 3},
		2: {4, 5},
	}
	runWalk := func(ctx context.Context, maxNodes int, maxDepth int, cancelAt int) ([]contract.AXNode, map[int]int, error) {
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		refs := make(map[int]int)
		ops := uiaWalkOps[int]{
			firstChild: func(p int) (int, error) {
				kids := children[p]
				if len(kids) == 0 {
					return 0, nil
				}
				ch := kids[0]
				refs[ch]++
				if ch == cancelAt {
					cancel()
				}
				return ch, nil
			},
			nextSibling: func(cur int) (int, error) {
				for _, kids := range children {
					for i, id := range kids {
						if id == cur && i+1 < len(kids) {
							nxt := kids[i+1]
							refs[nxt]++
							if nxt == cancelAt {
								cancel()
							}
							return nxt, nil
						}
					}
				}
				return 0, nil
			},
			makeNode: func(el int, parentID string) contract.AXNode {
				return contract.AXNode{
					NodeID:   strings.Repeat("n", el),
					ParentID: parentID,
					Role:     axVal("Text"),
				}
			},
			release: func(el int) {
				refs[el]--
			},
		}
		nodes, err := walkUIASubtree(cctx, 1, maxNodes, maxDepth, ops)
		return nodes, refs, err
	}

	assertZeroLeaks := func(t *testing.T, label string, refs map[int]int) {
		t.Helper()
		for id, rc := range refs {
			if rc != 0 {
				t.Fatalf("%s leaked handle %d with refCount=%d (refs=%v)", label, id, rc, refs)
			}
		}
	}

	// 1. Exact cap (maxNodes == 5) succeeds with 0 handle leaks.
	nodes, refs, err := runWalk(context.Background(), 5, 10, 0)
	if err != nil || len(nodes) != 5 {
		t.Fatalf("exact maxNodes=5 walk = (len=%d, err=%v), want (5, nil)", len(nodes), err)
	}
	assertZeroLeaks(t, "exact-5", refs)

	// 2. Truncation (maxNodes == 4 on 5-node tree) fails closed with ErrTreeTruncated and 0 leaks.
	truncNodes, truncRefs, truncErr := runWalk(context.Background(), 4, 10, 0)
	if !errors.Is(truncErr, ErrTreeTruncated) || truncNodes != nil {
		t.Fatalf("maxNodes=4 walk = (nodes=%v, err=%v), want (nil, ErrTreeTruncated)", truncNodes, truncErr)
	}
	assertZeroLeaks(t, "truncated-4", truncRefs)

	// 3. Non-positive maxNodes fails closed immediately.
	if _, _, zerr := runWalk(context.Background(), 0, 10, 0); !errors.Is(zerr, ErrTreeTruncated) {
		t.Fatalf("maxNodes=0 err = %v, want ErrTreeTruncated", zerr)
	}

	// 4. Mid-walk context cancellation fails closed with context.Canceled and releases queued handles
	// (both with queued siblings at depth>1 and at leaf maxDepth=1 where queue is empty).
	cNodes, cRefs, cErr := runWalk(context.Background(), 10, 10, 4)
	if !errors.Is(cErr, context.Canceled) || cNodes != nil {
		t.Fatalf("mid-walk cancel = (nodes=%v, err=%v), want (nil, context.Canceled)", cNodes, cErr)
	}
	assertZeroLeaks(t, "mid-cancel", cRefs)

	leafNodes, leafRefs, leafErr := runWalk(context.Background(), 10, 1, 3)
	if !errors.Is(leafErr, context.Canceled) || leafNodes != nil {
		t.Fatalf("leaf sibling cancel = (nodes=%v, err=%v), want (nil, context.Canceled)", leafNodes, leafErr)
	}
	assertZeroLeaks(t, "leaf-cancel", leafRefs)

	// 5. Driver.ExecuteStep & StaticTreeProvider propagate ErrTreeTruncated and context.Canceled.
	drv := NewDriverWithProvider(&StaticTreeProvider{
		Windows: []StaticWindowFixture{{
			Title: "Task Manager",
			Nodes: sampleTaskManagerTree(), // 14 nodes
		}},
	})
	drv.MaxNodes = 5
	if _, derr := drv.ExecuteStep(context.Background(), contract.Step{
		ID:          "trunc",
		WindowTitle: "Task Manager",
	}); !errors.Is(derr, ErrTreeTruncated) {
		t.Fatalf("Driver.ExecuteStep(MaxNodes=5) err = %v, want ErrTreeTruncated", derr)
	}

	preCanceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	drv.MaxNodes = DefaultMaxNodes
	if _, cerr := drv.ExecuteStep(preCanceled, contract.Step{ID: "c1", WindowTitle: "Task Manager"}); !errors.Is(cerr, context.Canceled) {
		t.Fatalf("Driver.ExecuteStep(canceled) err = %v, want context.Canceled", cerr)
	}
	if st := drv.ProbeStatus(preCanceled); st.Connected {
		t.Fatalf("ProbeStatus(canceled) Connected = true, want false (%+v)", st)
	}
}
