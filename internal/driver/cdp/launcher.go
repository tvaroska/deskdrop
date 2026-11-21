package cdp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// BrowserProfile describes a discovered Chrome/Edge user profile on the workstation.
type BrowserProfile struct {
	DirName     string `json:"dir_name"`
	Name        string `json:"name,omitempty"`
	GaiaName    string `json:"gaia_name,omitempty"`
	DisplayName string `json:"display_name"`
	UserName    string `json:"user_name,omitempty"`
	IsLastUsed  bool   `json:"is_last_used,omitempty"`
	IsDedicated bool   `json:"is_dedicated,omitempty"`
}

// ResolveBrowserProfile resolves a user's preferred Chrome session/profile string against discovered profiles.
func ResolveBrowserProfile(browser string, customUserDataDir string, preferred string) (BrowserProfile, bool) {
	profiles := ListBrowserProfiles(browser, customUserDataDir)
	return MatchBrowserProfile(profiles, preferred)
}

// MatchBrowserProfile finds the best matching profile (case-insensitive exact match first, then substring).
func MatchBrowserProfile(profiles []BrowserProfile, preferred string) (BrowserProfile, bool) {
	q := strings.ToLower(strings.TrimSpace(preferred))
	if q == "" {
		return BrowserProfile{}, false
	}
	if q == "__dedicated__" || q == "isolated" || q == "dedicated" || q == ":9222" {
		for _, p := range profiles {
			if p.IsDedicated || p.DirName == "__dedicated__" {
				return p, true
			}
		}
	}
	for _, p := range profiles {
		if strings.EqualFold(p.DirName, q) ||
			(p.Name != "" && strings.EqualFold(p.Name, q)) ||
			(p.GaiaName != "" && strings.EqualFold(p.GaiaName, q)) ||
			(p.UserName != "" && strings.EqualFold(p.UserName, q)) ||
			strings.EqualFold(p.DisplayName, q) {
			return p, true
		}
	}
	for _, p := range profiles {
		if p.IsDedicated {
			continue
		}
		if (p.Name != "" && strings.Contains(strings.ToLower(p.Name), q)) ||
			(p.GaiaName != "" && strings.Contains(strings.ToLower(p.GaiaName), q)) ||
			(p.UserName != "" && strings.Contains(strings.ToLower(p.UserName), q)) ||
			strings.Contains(strings.ToLower(p.DisplayName), q) ||
			strings.Contains(strings.ToLower(p.DirName), q) {
			return p, true
		}
	}
	return BrowserProfile{}, false
}

// ListBrowserProfiles discovers existing Chrome/Edge profiles from Local State plus the isolated :9222 profile.
func ListBrowserProfiles(browser string, customUserDataDir string) []BrowserProfile {
	userDataDir := strings.TrimSpace(customUserDataDir)
	if userDataDir == "" {
		userDataDir = defaultMainBrowserUserDataDir(browser)
	}

	var profiles []BrowserProfile
	if userDataDir != "" {
		profiles = readProfilesFromLocalState(userDataDir)
		if len(profiles) == 0 {
			profiles = scanProfileDirectories(userDataDir)
		}
	}

	profiles = append(profiles, BrowserProfile{
		DirName:     "__dedicated__",
		DisplayName: "Isolated Profile (:9222, Separate Login)",
		IsDedicated: true,
	})
	return profiles
}

func readProfilesFromLocalState(userDataDir string) []BrowserProfile {
	raw, err := os.ReadFile(filepath.Join(userDataDir, "Local State"))
	if err != nil {
		return nil
	}
	var state struct {
		Profile struct {
			LastUsed  string `json:"last_used"`
			InfoCache map[string]struct {
				Name     string `json:"name"`
				GaiaName string `json:"gaia_name"`
				UserName string `json:"user_name"`
			} `json:"info_cache"`
		} `json:"profile"`
	}
	if err := json.Unmarshal(raw, &state); err != nil || len(state.Profile.InfoCache) == 0 {
		return nil
	}

	var keys []string
	for k := range state.Profile.InfoCache {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i] == state.Profile.LastUsed {
			return true
		}
		if keys[j] == state.Profile.LastUsed {
			return false
		}
		if keys[i] == "Default" {
			return true
		}
		if keys[j] == "Default" {
			return false
		}
		return keys[i] < keys[j]
	})

	var out []BrowserProfile
	for _, dir := range keys {
		info := state.Profile.InfoCache[dir]
		label := strings.TrimSpace(info.Name)
		if label == "" || strings.EqualFold(label, "Person 1") {
			if strings.TrimSpace(info.GaiaName) != "" {
				label = strings.TrimSpace(info.GaiaName)
			}
		}
		if label == "" {
			label = dir
		}
		email := strings.TrimSpace(info.UserName)
		display := fmt.Sprintf("%s [%s]", label, dir)
		if email != "" && !strings.Contains(strings.ToLower(label), strings.ToLower(email)) {
			display = fmt.Sprintf("%s (%s) [%s]", label, email, dir)
		}
		out = append(out, BrowserProfile{
			DirName:     dir,
			Name:        strings.TrimSpace(info.Name),
			GaiaName:    strings.TrimSpace(info.GaiaName),
			DisplayName: display,
			UserName:    email,
			IsLastUsed:  dir == state.Profile.LastUsed,
		})
	}
	return out
}

func scanProfileDirectories(userDataDir string) []BrowserProfile {
	entries, err := os.ReadDir(userDataDir)
	if err != nil {
		return nil
	}
	var out []BrowserProfile
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name != "Default" && !strings.HasPrefix(name, "Profile ") {
			continue
		}
		display := name
		profName := ""
		if raw, rerr := os.ReadFile(filepath.Join(userDataDir, name, "Preferences")); rerr == nil {
			var pref struct {
				Profile struct {
					Name string `json:"name"`
				} `json:"profile"`
			}
			if json.Unmarshal(raw, &pref) == nil && strings.TrimSpace(pref.Profile.Name) != "" {
				profName = strings.TrimSpace(pref.Profile.Name)
				display = fmt.Sprintf("%s [%s]", profName, name)
			}
		}
		out = append(out, BrowserProfile{
			DirName:     name,
			Name:        profName,
			DisplayName: display,
			IsLastUsed:  name == "Default",
		})
	}
	return out
}

func defaultMainBrowserUserDataDir(preferred string) string {
	pref := strings.ToLower(strings.TrimSpace(preferred))
	switch runtime.GOOS {
	case "windows":
		localApp := os.Getenv("LOCALAPPDATA")
		if localApp == "" {
			return ""
		}
		if pref == "edge" {
			return filepath.Join(localApp, "Microsoft", "Edge", "User Data")
		}
		return filepath.Join(localApp, "Google", "Chrome", "User Data")
	case "darwin":
		home, _ := os.UserHomeDir()
		if home == "" {
			return ""
		}
		if pref == "edge" {
			return filepath.Join(home, "Library", "Application Support", "Microsoft Edge")
		}
		return filepath.Join(home, "Library", "Application Support", "Google", "Chrome")
	default:
		home, _ := os.UserHomeDir()
		if home == "" {
			return ""
		}
		if pref == "edge" {
			return filepath.Join(home, ".config", "microsoft-edge")
		}
		return filepath.Join(home, ".config", "google-chrome")
	}
}

// LaunchBrowserProfile starts Chrome/Edge using a selected profile or the isolated :9222 profile.
func LaunchBrowserProfile(browser string, prof BrowserProfile, targetURL string) (string, error) {
	if prof.IsDedicated || prof.DirName == "__dedicated__" {
		bin, _, err := LaunchDedicatedBrowserWithURL(browser, 9222, "", targetURL)
		return bin, err
	}

	bin, err := findBrowserBinary(browser)
	if err != nil {
		return "", err
	}

	args := []string{fmt.Sprintf("--profile-directory=%s", prof.DirName)}
	if strings.TrimSpace(targetURL) != "" {
		args = append(args, strings.TrimSpace(targetURL))
	}
	cmd := exec.Command(bin, args...)
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("failed to start %s (%s): %w", bin, prof.DirName, err)
	}
	return bin, nil
}

// LaunchDedicatedBrowser spawns installed native Chrome or Edge with a dedicated user-data-dir on port.
func LaunchDedicatedBrowser(browser string, port int, userDataDir string) (string, string, error) {
	return LaunchDedicatedBrowserWithURL(browser, port, userDataDir, "")
}

// LaunchDedicatedBrowserWithURL spawns Chrome or Edge on a dedicated profile and opens startURL if non-empty.
func LaunchDedicatedBrowserWithURL(browser string, port int, userDataDir string, startURL string) (string, string, error) {
	if port <= 0 {
		port = 9222
	}
	if strings.TrimSpace(userDataDir) == "" {
		userDataDir = defaultDedicatedProfileDir()
	}
	if err := os.MkdirAll(userDataDir, 0o700); err != nil {
		return "", "", err
	}

	bin, err := findBrowserBinary(browser)
	if err != nil {
		return "", "", err
	}

	args := []string{
		fmt.Sprintf("--remote-debugging-port=%d", port),
		fmt.Sprintf("--user-data-dir=%s", userDataDir),
		"--no-first-run",
		"--no-default-browser-check",
	}
	if strings.TrimSpace(startURL) != "" {
		args = append(args, strings.TrimSpace(startURL))
	}
	cmd := exec.Command(bin, args...)
	if err := cmd.Start(); err != nil {
		return "", "", fmt.Errorf("failed to start %s: %w", bin, err)
	}
	return bin, userDataDir, nil
}

func defaultDedicatedProfileDir() string {
	switch runtime.GOOS {
	case "windows":
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			return filepath.Join(local, "DeskDrop", "chrome-profile")
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, "Library", "Application Support", "DeskDrop", "chrome-profile")
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "share", "deskdrop", "chrome-profile")
	}
	return filepath.Join(os.TempDir(), "deskdrop-profile")
}

func findBrowserBinary(preferred string) (string, error) {
	pref := strings.ToLower(strings.TrimSpace(preferred))
	var candidates []string

	switch runtime.GOOS {
	case "windows":
		pf := os.Getenv("ProgramFiles")
		pfx86 := os.Getenv("ProgramFiles(x86)")
		local := os.Getenv("LOCALAPPDATA")
		chromePaths := []string{
			filepath.Join(pf, "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(pfx86, "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(local, "Google", "Chrome", "Application", "chrome.exe"),
		}
		edgePaths := []string{
			filepath.Join(pf, "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(pfx86, "Microsoft", "Edge", "Application", "msedge.exe"),
		}
		if pref == "edge" {
			candidates = append(edgePaths, chromePaths...)
		} else {
			candidates = append(chromePaths, edgePaths...)
		}
	case "darwin":
		chrome := "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
		edge := "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge"
		if pref == "edge" {
			candidates = []string{edge, chrome}
		} else {
			candidates = []string{chrome, edge}
		}
	default:
		names := []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge"}
		if pref == "edge" {
			names = []string{"microsoft-edge", "google-chrome", "chromium"}
		}
		for _, n := range names {
			if p, err := exec.LookPath(n); err == nil {
				return p, nil
			}
		}
	}

	for _, c := range candidates {
		if c != "" {
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				return c, nil
			}
		}
	}
	return "", errors.New("could not find installed Google Chrome or Microsoft Edge binary")
}

func readProfileSignals(browser string, customUserDataDir string, dirName string) (map[string]bool, string) {
	userDataDir := strings.TrimSpace(customUserDataDir)
	if userDataDir == "" {
		userDataDir = defaultMainBrowserUserDataDir(browser)
	}
	if userDataDir == "" || dirName == "" || dirName == "__dedicated__" {
		return nil, ""
	}
	profDir := filepath.Join(userDataDir, dirName)
	extIDs := make(map[string]bool)
	if raw, err := os.ReadFile(filepath.Join(profDir, "Preferences")); err == nil {
		var pref struct {
			Extensions struct {
				Settings map[string]json.RawMessage `json:"settings"`
			} `json:"extensions"`
		}
		if json.Unmarshal(raw, &pref) == nil {
			for k := range pref.Extensions.Settings {
				extIDs[strings.ToLower(k)] = true
			}
		}
	}

	sessDir := filepath.Join(profDir, "Sessions")
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		return extIDs, ""
	}
	type fileMod struct {
		path string
		mod  int64
	}
	var files []fileMod
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasPrefix(n, "Session_") && !strings.HasPrefix(n, "Tabs_") {
			continue
		}
		if info, ierr := e.Info(); ierr == nil {
			files = append(files, fileMod{
				path: filepath.Join(sessDir, n),
				mod:  info.ModTime().UnixNano(),
			})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod > files[j].mod })
	if len(files) > 2 {
		files = files[:2]
	}
	var sb strings.Builder
	for _, f := range files {
		raw, rerr := os.ReadFile(f.path)
		if rerr != nil {
			continue
		}
		if len(raw) > 512*1024 {
			raw = raw[len(raw)-512*1024:]
		}
		sb.WriteString(strings.ToLower(string(raw)))
		sb.WriteByte('\n')
	}
	return extIDs, sb.String()
}
