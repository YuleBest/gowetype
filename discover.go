package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 应用数据目录的探测。目标是尽量不写死绝对路径：
// 包名是参数，数据目录按 Android 的几种约定去找，具体的库文件再按内容特征识别。

// knownPackages 是自动探测时的候选包名，按顺序试。
// 用 -pkg 可以指定别的。
var knownPackages = []string{
	"com.tencent.wetype", // 微信输入法
	"com.oplus.keyboard", // 小布输入法
	"com.sohu.inputmethod.sogou",
	"com.baidu.input",
	"com.iflytek.inputmethod",
	"com.google.android.inputmethod.latin",
}

// isDir 判断是不是目录。
func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// resolveDataDir 找应用私有数据目录。
// 先试 /data/user/0/<pkg> 和 /data/data/<pkg>，再扫 /data/user/*/<pkg> 覆盖多用户。
func resolveDataDir(pkg string) (string, error) {
	for _, c := range []string{
		filepath.Join("/data/user/0", pkg),
		filepath.Join("/data/data", pkg),
	} {
		if isDir(c) {
			return c, nil
		}
	}
	matches, _ := filepath.Glob(filepath.Join("/data/user", "*", pkg))
	sort.Strings(matches)
	for _, c := range matches {
		if isDir(c) {
			return c, nil
		}
	}
	return "", fmt.Errorf("找不到 %s 的数据目录（试过 /data/user/0、/data/data、/data/user/*）\n"+
		"读 /data 需要 root。也可以用 -data 直接指定已拷贝出来的目录", pkg)
}

// detectPackage 在没有指定包名时挑一个。装了多个输入法是常态，所以不能只按
// 候选表的顺序取第一个：先挑"确实有我们要的数据"的那个，都没有再退回第一个。
// hint 是命令名（words / clipboard），决定看哪类数据。
func detectPackage(hint string) (string, error) {
	var installed []string
	for _, p := range knownPackages {
		if _, err := resolveDataDir(p); err == nil {
			installed = append(installed, p)
		}
	}
	switch len(installed) {
	case 0:
		return "", fmt.Errorf("没探测到已安装的输入法，用 -pkg 指定包名")
	case 1:
		return installed[0], nil
	}
	for _, p := range installed {
		dir, err := resolveDataDir(p)
		if err != nil {
			continue
		}
		if hasRelevantData(dir, hint) {
			return p, nil
		}
	}
	fmt.Fprintf(os.Stderr, "注意：%s 都有数据，先按第一个来；用 -pkg 可以指定别的\n",
		strings.Join(installed, ", "))
	return installed[0], nil
}

// hasRelevantData 看这个数据目录里有没有该命令要的东西。
func hasRelevantData(dir, hint string) bool {
	switch hint {
	case "clipboard":
		for _, f := range findSQLiteFiles(dir, 4) {
			db, err := openSQLite(f)
			if err != nil {
				continue
			}
			if _, err := pickClipboardTable(db, ""); err == nil {
				return true
			}
		}
	default: // words
		for _, d := range findLevelDBDirs(dir, 6) {
			ents, err := dbEntries(d)
			if err != nil {
				continue
			}
			if countUserDictKeys(ents) > 0 {
				return true
			}
		}
	}
	return false
}

// hasMagic 判断文件开头是不是给定的魔数。
func hasMagic(path, magic string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, len(magic))
	if _, err := f.Read(buf); err != nil {
		return false
	}
	return string(buf) == magic
}

// findSQLiteFiles 找数据目录里的 SQLite 库。
//
// 先看 Android 约定的 databases/ 目录，找不到再做一次有深度上限的遍历，
// 按文件头 "SQLite format 3" 识别。这样不依赖具体文件名。
func findSQLiteFiles(dataDir string, maxDepth int) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}

	dbDir := filepath.Join(dataDir, "databases")
	if isDir(dbDir) {
		ents, _ := os.ReadDir(dbDir)
		for _, e := range ents {
			if e.IsDir() || strings.HasSuffix(e.Name(), "-wal") ||
				strings.HasSuffix(e.Name(), "-shm") || strings.HasSuffix(e.Name(), "-journal") {
				continue
			}
			p := filepath.Join(dbDir, e.Name())
			if hasMagic(p, "SQLite format 3\x00") {
				add(p)
			}
		}
	}
	if len(out) > 0 {
		return out
	}

	// 兜底：有深度上限的遍历
	base := strings.Count(dataDir, string(filepath.Separator))
	filepath.Walk(dataDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if strings.Count(p, string(filepath.Separator))-base > maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if info.Size() < 512 || info.Size() > 512<<20 {
			return nil
		}
		if hasMagic(p, "SQLite format 3\x00") {
			add(p)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// isLevelDBDir 判断目录是不是一个 LevelDB（有 CURRENT 和 MANIFEST-*）。
func isLevelDBDir(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "CURRENT")); err != nil {
		return false
	}
	m, _ := filepath.Glob(filepath.Join(dir, "MANIFEST-*"))
	return len(m) > 0
}

// findLevelDBDirs 找数据目录里的 LevelDB。
//
// 先按微信输入法的目录约定找（这是它引擎自己的布局，省得遍历），
// 找不到再按 LevelDB 的文件特征遍历。返回的目录里可能混着非词库的库，
// 由调用方按内容判断。
func findLevelDBDirs(dataDir string, maxDepth int) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}

	// 约定位置：userdict 下面每个账号一个库（v5/<uin>/），外加热词库。
	// 账号目录外面还套了一层版本目录（v5），所以往下找两层。
	userdict := filepath.Join(dataDir, "MicroMsg", "wxime", "common", "userdict")
	if isDir(userdict) {
		for _, pattern := range []string{"*", "*/*", "*/*/*"} {
			subs, _ := filepath.Glob(filepath.Join(userdict, pattern))
			sort.Strings(subs)
			for _, s := range subs {
				if isLevelDBDir(s) {
					add(s)
				}
			}
		}
	}
	if len(out) > 0 {
		return out
	}

	base := strings.Count(dataDir, string(filepath.Separator))
	filepath.Walk(dataDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			return nil
		}
		if strings.Count(p, string(filepath.Separator))-base > maxDepth {
			return filepath.SkipDir
		}
		switch info.Name() {
		case "cache", "code_cache", "app_webview", "lib", "oat":
			return filepath.SkipDir
		}
		if isLevelDBDir(p) {
			add(p)
			return filepath.SkipDir
		}
		return nil
	})
	sort.Strings(out)
	return out
}
