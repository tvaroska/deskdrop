package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/tvaroska/deskdrop/internal/contract"
)

func TestZeroExternalDepsInDesktopCLI(t *testing.T) {
	root := findRepoRoot(t)
	cmd := exec.Command("go", "list", "-f", `{{range .Deps}}{{.}}{{"\n"}}{{end}}`, "./cmd/deskdrop")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list ./cmd/deskdrop failed: %v\n%s", err, string(out))
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
			t.Errorf("cmd/deskdrop must link 0 external Go modules, found %q", pkg)
		}
	}
}

func TestZeroPostgresAndZeroEvalJS(t *testing.T) {
	root := findRepoRoot(t)
	goMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(goMod), "github.com/lib/"+"pq") {
		t.Fatal("go.mod must not reference github.com/lib/pq (Postgres retired in ADR-003)")
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
	count := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		count++
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
		}
	}
	if count < 2 {
		t.Fatalf("expected at least 2 example recipes, found %d", count)
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
	checkCap("CRITICAL.md", 5_000)
	checkCap("DECISIONS.md", 25_000)

	forbidden := []*regexp.Regexp{
		regexp.MustCompile("file" + ":///"),
		regexp.MustCompile("/usr/local/google" + "/home/"),
		regexp.MustCompile(`\b` + "b" + `/\d{6,}\b`),
		regexp.MustCompile("//" + "depot/"),
		regexp.MustCompile("~/" + "customers|~/" + "notes|~/" + "ideas|~/" + "robotics"),
		regexp.MustCompile("boris" + `\.tvaroska\.sk`),
	}
	mdLinkRe := regexp.MustCompile(`\[[^\]]+\]\(([^)\s]+)\)`)

	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, werr error) error {
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
				resolved := filepath.Join(filepath.Dir(path), filepath.FromSlash(target))
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
