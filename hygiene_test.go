package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/tvaroska/deskdrop/internal/contract"
	"github.com/tvaroska/deskdrop/internal/verify"
)

func TestZeroExternalDepsInDesktopCLI(t *testing.T) {
	root := findRepoRoot(t)
	for _, goos := range []string{"linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			cmd := exec.Command("go", "list", "-f", `{{range .Deps}}{{.}}{{"\n"}}{{end}}`, "./cmd/deskdrop")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64", "CGO_ENABLED=0")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("go list ./cmd/deskdrop (GOOS=%s) failed: %v\n%s", goos, err, string(out))
			}
			for _, raw := range strings.Split(string(out), "\n") {
				pkg := strings.TrimSpace(raw)
				if pkg == "" {
					continue
				}
				if strings.HasPrefix(pkg, "github.com/tvaroska/deskdrop/") || pkg == "github.com/tvaroska/deskdrop" {
					continue
				}
				// Standard library packages never contain a dot in the first path segment.
				firstSeg := strings.SplitN(pkg, "/", 2)[0]
				if strings.Contains(firstSeg, ".") {
					t.Errorf("cmd/deskdrop (GOOS=%s) must link 0 external Go modules, found %q", goos, pkg)
				}
			}
		})
	}
}

func TestZeroPostgresAndZeroEvalJS(t *testing.T) {
	root := findRepoRoot(t)
	goMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	goModStr := string(goMod)
	if strings.Contains(goModStr, "github.com/lib/"+"pq") || strings.Contains(goModStr, "github.com/jackc/"+"pgx") {
		t.Fatal("go.mod must not reference PostgreSQL drivers (retired in ADR-003)")
	}

	evalNeedle := "eval" + "_js"
	for _, sub := range []string{"internal", "cmd", "examples"} {
		dir := filepath.Join(root, sub)
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, werr error) error {
			if werr != nil || d.IsDir() {
				return werr
			}
			if strings.HasSuffix(d.Name(), "_test.go") {
				return nil
			}
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			if strings.Contains(string(raw), evalNeedle) {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s contains retired %s reference", rel, evalNeedle)
			}
			return nil
		})
	}
}

func TestExampleRecipesParseStrictly(t *testing.T) {
	root := findRepoRoot(t)
	exDir := filepath.Join(root, "examples", "recipes")
	entries, err := os.ReadDir(exDir)
	if err != nil {
		t.Fatalf("read examples/recipes: %v", err)
	}
	loaded := make(map[string]contract.Recipe)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(exDir, e.Name()))
		if rerr != nil {
			t.Fatal(rerr)
		}
		var r contract.Recipe
		if perr := contract.ParseJSONStrict(raw, &r); perr != nil {
			t.Errorf("example recipe %s failed strict parse: %v", e.Name(), perr)
			continue
		}
		if _, rerr := contract.ResolveRecipe(r, nil); rerr != nil {
			t.Errorf("example recipe %s failed ResolveRecipe: %v", e.Name(), rerr)
			continue
		}
		loaded[e.Name()] = r
	}
	if len(loaded) < 4 {
		t.Fatalf("expected at least 4 example recipes, found %d", len(loaded))
	}

	// Verify each shipped example recipe deterministically against minimal matching fixtures (T-7).
	if r, ok := loaded["status-dashboard.json"]; ok {
		html := `<html><body><main>
			<div data-testid="global-health">All Systems Operational</div>
			<div data-testid="open-incidents">0</div>
			<article class="service-card"><h3 class="card-title">API</h3><span class="badge-health">Up</span><span data-kpi="uptime">99.98</span></article>
		</main></body></html>`
		out, verr := verify.VerifyRecipeOnHTML(r, nil, "overview", html, "https://status.example.com/overview")
		if verr != nil || !out.Passed || out.ItemCount != 1 {
			t.Fatalf("status-dashboard.json verify failed: %+v err=%v", out, verr)
		}
	} else {
		t.Error("missing examples/recipes/status-dashboard.json")
	}

	if r, ok := loaded["release-feed.json"]; ok {
		html := `<html><body>
			<article class="release">
				<a class="release-tag" href="/example-org/example-repo/releases/v1.4.0">v1.4.0</a>
				<time datetime="2026-11-26T00:00:00Z">Nov 26</time>
				<span class="download-count">1,250</span>
			</article>
		</body></html>`
		out, verr := verify.VerifyRecipeOnHTML(r, nil, "releases", html, "https://releases.example.com/example-org/example-repo")
		if verr != nil || !out.Passed || out.ItemCount != 1 {
			t.Fatalf("release-feed.json verify failed: %+v err=%v", out, verr)
		}
	} else {
		t.Error("missing examples/recipes/release-feed.json")
	}

	if r, ok := loaded["google-scholar-profile.json"]; ok {
		if r.AllowsComputerUseFallback() {
			t.Error("google-scholar-profile.json must keep allow_computer_use_fallback=false")
		}
		profileHTML := `<html><body>
			<div id="gsc_prf_in">Ada Researcher</div>
			<table id="gsc_rsb_st"><tbody>
				<tr><td>Citations</td><td class="gsc_rsb_std">420</td><td class="gsc_rsb_std">310</td></tr>
				<tr><td>h-index</td><td class="gsc_rsb_std">11</td><td class="gsc_rsb_std">9</td></tr>
				<tr><td>i10-index</td><td class="gsc_rsb_std">12</td><td class="gsc_rsb_std">10</td></tr>
			</tbody></table>
			<table id="gsc_a_b"><tr class="gsc_a_tr">
				<td><a class="gsc_a_at" href="/citations?view_op=view_citation">Declarative Extraction</a><div class="gs_gray">A. Researcher</div></td>
				<td><a class="gsc_a_ac">42</a></td>
				<td class="gsc_a_y"><span class="gsc_a_h">2025</span></td>
			</tr></table>
		</body></html>`
		out1, verr := verify.VerifyRecipeOnHTML(r, nil, "profile_citations", profileHTML, "https://scholar.google.com/citations?user=example_scholar_id&hl=en")
		if verr != nil || !out1.Passed || out1.ItemCount != 1 {
			t.Fatalf("google-scholar-profile.json profile_citations verify failed: %+v err=%v", out1, verr)
		}
		homeHTML := `<html><body>
			<div class="gs_or"><h3 class="gs_rt"><a href="https://arxiv.org/abs/2601.00001">Systems Paper</a></h3>
			<div class="gs_a">Author - Conf, 2026</div><div class="gs_rs">Abstract text.</div>
			<a href="/scholar?cites=123">Cited by 7</a></div>
		</body></html>`
		out2, verr := verify.VerifyRecipeOnHTML(r, nil, "recommended_articles", homeHTML, "https://scholar.google.com/")
		if verr != nil || !out2.Passed || out2.ItemCount != 1 {
			t.Fatalf("google-scholar-profile.json recommended_articles verify failed: %+v err=%v", out2, verr)
		}
	} else {
		t.Error("missing examples/recipes/google-scholar-profile.json")
	}

	if r, ok := loaded["desktop-task-manager.json"]; ok {
		if r.NormalizedDriver() != "uia" {
			t.Errorf("desktop-task-manager.json driver = %q, want uia", r.NormalizedDriver())
		}
		if r.AllowsComputerUseFallback() {
			t.Error("desktop-task-manager.json must keep allow_computer_use_fallback=false")
		}
		nodes := []contract.AXNode{
			{NodeID: "1", Role: &contract.AXValue{Value: "window"}, Name: &contract.AXValue{Value: "Task Manager"}, ChildIDs: []string{"2", "3", "4"}},
			{NodeID: "2", Role: &contract.AXValue{Value: "text"}, Name: &contract.AXValue{Value: "CPU 18%"}, Properties: []contract.AXProperty{{Name: "automation_id", Value: contract.AXValue{Value: "CpuUtilizationText"}}}},
			{NodeID: "3", Role: &contract.AXValue{Value: "text"}, Name: &contract.AXValue{Value: "Memory 12.4 GB"}, Properties: []contract.AXProperty{{Name: "automation_id", Value: contract.AXValue{Value: "MemoryUsageText"}}}},
			{NodeID: "4", Role: &contract.AXValue{Value: "datagrid"}, ChildIDs: []string{"5"}},
			{NodeID: "5", Role: &contract.AXValue{Value: "dataitem"}, ChildIDs: []string{"6", "7"}},
			{NodeID: "6", Role: &contract.AXValue{Value: "text"}, Name: &contract.AXValue{Value: "deskdrop.exe"}, Properties: []contract.AXProperty{{Name: "automation_id", Value: contract.AXValue{Value: "ColProcessName"}}}},
			{NodeID: "7", Role: &contract.AXValue{Value: "text"}, Name: &contract.AXValue{Value: "24.5 MB"}, Properties: []contract.AXProperty{{Name: "automation_id", Value: contract.AXValue{Value: "ColMemoryMB"}}}},
		}
		out, verr := verify.VerifyRecipeOnAXTree(r, nil, "processes", nodes, "uia://window/Task Manager")
		if verr != nil || !out.Passed || out.ItemCount != 1 {
			t.Fatalf("desktop-task-manager.json AXTree verify failed: %+v err=%v", out, verr)
		}
	} else {
		t.Error("missing examples/recipes/desktop-task-manager.json")
	}
}

func TestPortableRepoHygieneAndLinks(t *testing.T) {
	repoRoot := findRepoRoot(t)

	if _, err := os.Stat(filepath.Join(repoRoot, ".github", "workflows")); err == nil {
		t.Fatal("CI/CD must use cloudbuild.yaml only (zero .github/workflows/)")
	}

	checkCap := func(name string, maxBytes int64) {
		info, err := os.Stat(filepath.Join(repoRoot, name))
		if err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
		if info.Size() > maxBytes {
			t.Fatalf("%s exceeds %d byte cap (%d bytes)", name, maxBytes, info.Size())
		}
	}
	checkCap("AGENTS.md", 24_000)
	checkCap("CLAUDE.md", 24_000)
	checkCap("CRITICAL.md", 5_000)
	checkCap("DECISIONS.md", 25_000)

	gitignoreRaw, err := os.ReadFile(filepath.Join(repoRoot, ".gitignore"))
	if err != nil {
		t.Fatalf("missing .gitignore: %v", err)
	}
	gitignoreLines := make(map[string]bool)
	for _, ln := range strings.Split(string(gitignoreRaw), "\n") {
		gitignoreLines[strings.TrimSpace(ln)] = true
	}
	for _, reqPattern := range []string{".env", ".env.*"} {
		if !gitignoreLines[reqPattern] {
			t.Errorf(".gitignore must cover %q", reqPattern)
		}
	}

	forbidden := []*regexp.Regexp{
		regexp.MustCompile("file" + ":///"),
		regexp.MustCompile("/usr/local/google" + "/home/"),
		regexp.MustCompile(`\b` + "b" + `/\d{6,}\b`),
		regexp.MustCompile(`(?:^|[\s(\[` + "`" + `])` + "go" + `/[a-zA-Z0-9][a-zA-Z0-9._-]+`),
		regexp.MustCompile("//" + "depot/"),
		regexp.MustCompile("~/" + "customers|~/" + "notes|~/" + "ideas|~/" + "robotics"),
		regexp.MustCompile("boris" + `\.tvaroska\.sk`),
		regexp.MustCompile("gemini-" + `4\.0-(pro|carbon|flash)`),
		regexp.MustCompile("sites-" + "470716"),
		regexp.MustCompile("1076807" + "043236"),
	}
	mdLinkRe := regexp.MustCompile(`\[[^\]]+\]\(([^)\s]+)\)`)

	err = filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		name := d.Name()
		if d.IsDir() {
			if strings.HasPrefix(name, ".") || name == "bin" || name == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		if name == "hygiene_test.go" || strings.HasSuffix(name, ".exe") || strings.HasSuffix(name, ".zip") {
			return nil
		}

		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		text := string(raw)
		rel, _ := filepath.Rel(repoRoot, path)

		for _, re := range forbidden {
			if m := re.FindString(text); m != "" {
				t.Errorf("%s: forbidden non-portable pattern %q", rel, m)
			}
		}

		if strings.HasSuffix(name, ".md") {
			for _, m := range mdLinkRe.FindAllStringSubmatch(text, -1) {
				target := strings.SplitN(m[1], "#", 2)[0]
				if target == "" || strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "mailto:") {
					continue
				}
				resolved := filepath.Clean(filepath.Join(filepath.Dir(path), filepath.FromSlash(target)))
				relResolved, rerr := filepath.Rel(repoRoot, resolved)
				if rerr != nil || relResolved == ".." || strings.HasPrefix(relResolved, ".."+string(filepath.Separator)) || filepath.IsAbs(target) {
					t.Errorf("%s: relative markdown link %q escapes repository root", rel, target)
					continue
				}
				if _, serr := os.Stat(resolved); serr != nil {
					t.Errorf("%s: broken relative markdown link %q", rel, target)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find go.mod root")
		}
		dir = parent
	}
}
