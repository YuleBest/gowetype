package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// gowetype —— 从输入法的私有数据里导出个人词库、筛选词条、导出剪贴板历史。
//
// 不写死路径：包名是参数（不指定就自动探测已安装的输入法），数据目录按 Android
// 的几种约定去找，具体是哪个库文件按内容特征识别（LevelDB 看 CURRENT+MANIFEST，
// SQLite 看文件头魔数，剪贴板表按表名含 clipboard 来认）。
//
// 所有读取都是只读的：不打开数据库、不做 checkpoint、不改任何文件，
// 所以输入法正在运行也能安全读。

const usage = `gowetype —— 导出输入法的个人词库与剪贴板历史

用法: gowetype [命令] [参数]

命令:
  words       导出个人词库（默认命令）
  grep        按词 / 拼音 / 词频筛选词条
  clipboard   导出剪贴板历史
  list        列出探测到的数据库，不导出

通用参数:
  -pkg NAME   应用包名，默认自动探测（候选：%s）
  -data DIR   直接指定应用数据目录，跳过包名探测
  -o FILE     输出到文件，默认写 stdout

words 的参数:
  -format tsv|txt|json   默认 tsv（词 / 拼音 / 词频）
  -min N -max N          按汉字数过滤，默认 1..12
  -length                只输出词的数量（-count 是同一个开关的别名）
  -no-hot                不合并热词库

grep 的参数:
  位置参数就是模式，可以给多个，之间是"或"
  -pinyin PAT            按拼音筛，可以重复给（拼音本身含逗号，不能用逗号分隔）
  -re                    模式和 -pinyin 都当正则（RE2 语法）
  -exact                 整串精确匹配
  -i                     正则忽略大小写（子串匹配本来就忽略大小写）
  -v                     反选，输出不匹配的
  -min N -max N          按汉字数过滤
  -score-min N -score-max N   按词频过滤
  -limit N               最多输出 N 条
  -length                只输出词的数量（-count 是同一个开关的别名）
  -no-hot                不合并热词库
  -format tsv|txt|json   默认 tsv

  flag 和模式可以混着写：grep 报错 -count 和 grep -count 报错 一样。

clipboard 的参数:
  -format json|tsv|txt   默认 json
  -table NAME            指定表名，默认挑名字含 clipboard 的表
  -cols a,b,c            tsv 模式下输出哪些列
  -limit N               只输出最新的 N 条
  -length                只输出条数（-count 是同一个开关的别名）

例子:
  gowetype words -format txt -o /sdcard/Download/wetype_words.txt
  gowetype grep 报错 目录                       # 词里含"报错"或"目录"
  gowetype grep -re -pinyin '^wei,zhuang$'      # 拼音正好是 wei,zhuang
  gowetype grep -pinyin 'wei,zhuang' -pinyin 'bao,cuo'
  gowetype grep -re '^[a-zA-Z]站$'              # 正则：单个字母 + 站
  gowetype grep -re -pinyin '^(wei|bao),' -score-min 20
  gowetype grep -v -re '^[\x{4e00}-\x{9fff}]{6,}$'   # 排除 6 字以上的
  gowetype grep 报错 -length                    # 只输出命中多少条
  gowetype words -length                        # 只输出词库共多少词
  gowetype grep -re '^[a-zA-Z]站$' -length
  gowetype clipboard -limit 100
  gowetype clipboard -length

读 /data 需要 root。没有 root 就先把数据目录拷出来，用 -data 指过去。
`

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-h", "--help", "help":
			fmt.Printf(usage, strings.Join(knownPackages, ", "))
			return
		}
	}

	cmd := "words"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd = args[0]
		args = args[1:]
	}

	switch cmd {
	case "words":
		runWords(args)
	case "grep":
		runGrep(args)
	case "clipboard":
		runClipboard(args)
	case "list":
		runList(args)
	default:
		fatalf("不认识的命令 %q，可选 words | grep | clipboard | list", cmd)
	}
}

// common 是各命令共用的参数。
type common struct {
	pkg     string
	dataDir string
	out     string
	hint    string // 命令名，给自动探测判断该找哪类数据用
}

func addCommon(fs *flag.FlagSet, hint string) *common {
	c := &common{hint: hint}
	fs.StringVar(&c.pkg, "pkg", "", "应用包名，默认自动探测")
	fs.StringVar(&c.dataDir, "data", "", "应用数据目录，跳过包名探测")
	fs.StringVar(&c.out, "o", "", "输出文件，默认 stdout")
	return c
}

// resolve 得到数据目录。
func (c *common) resolve() (string, string) {
	pkg := c.pkg
	if c.dataDir != "" {
		if !isDir(c.dataDir) {
			fatalf("目录不存在: %s", c.dataDir)
		}
		if pkg == "" {
			pkg = filepath.Base(strings.TrimRight(c.dataDir, "/"))
		}
		return c.dataDir, pkg
	}
	if pkg == "" {
		var err error
		if pkg, err = detectPackage(c.hint); err != nil {
			fatalf("%v", err)
		}
		fmt.Fprintf(os.Stderr, "自动探测到包名: %s\n", pkg)
	}
	dir, err := resolveDataDir(pkg)
	if err != nil {
		fatalf("%v", err)
	}
	return dir, pkg
}

// open 打开输出目标。
func (c *common) open() (io.WriteCloser, *bufio.Writer) {
	if c.out == "" {
		w := bufio.NewWriterSize(os.Stdout, 1<<16)
		return nopCloser{w}, w
	}
	f, err := os.Create(c.out)
	if err != nil {
		fatalf("写不了 %s: %v", c.out, err)
	}
	w := bufio.NewWriterSize(f, 1<<16)
	return f, w
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// stringList 是个可以重复给的同名参数：-pinyin a -pinyin b。
// 拼音本身含逗号（wei,zhuang），所以不能像别处那样用逗号分隔多个。
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, " ") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// parse 解析参数，允许 flag 和位置参数混着写。
//
// Go 标准库的 flag 包遇到第一个位置参数就停止解析 flag，于是 `grep 报错 -count`
// 里的 -count 会被当成模式。这里先把 flag 挑出来挪到前面，再交给 flag 包，
// 这样 `grep 报错 -count` 和 `grep -count 报错` 行为一致。
func parse(fs *flag.FlagSet, args []string) {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i+1:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' {
			rest = append(rest, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.ContainsRune(name, '=') {
			continue // -flag=value 自带值
		}
		f := fs.Lookup(name)
		if f == nil {
			continue // 未知 flag，交给 flag 包报错
		}
		if bv, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bv.IsBoolFlag() {
			continue // 布尔开关不吃下一个参数
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	fs.Parse(append(flags, rest...))
}

// --------------------------------------------------------------------------
// 词库加载（words 和 grep 共用）

// loadWords 探测所有词库目录并解析成词表。
func loadWords(dataDir string, noHot bool) (map[string]*wordEntry, map[string]int) {
	dirs := findLevelDBDirs(dataDir, 6)
	if len(dirs) == 0 {
		fatalf("在 %s 下没找到 LevelDB", dataDir)
	}
	var all []kv
	found := 0
	for _, d := range dirs {
		if noHot && strings.Contains(d, "hot_word") {
			continue
		}
		ents, err := dbEntries(d)
		if err != nil {
			fmt.Fprintf(os.Stderr, "跳过 %s: %v\n", d, err)
			continue
		}
		n := countUserDictKeys(ents)
		if n == 0 {
			continue // 不是词库（比如别的 LevelDB）
		}
		found++
		all = append(all, ents...)
		fmt.Fprintf(os.Stderr, "读取 %s：%d 条记录，其中词库条目 %d\n", d, len(ents), n)
	}
	if found == 0 {
		fatalf("在 %s 下没找到个人词库。\n"+
			"（本命令解析的是微信输入法自己的 userdict 格式，换成别的输入法没有对应前缀）",
			dataDir)
	}
	stats := map[string]int{}
	return parseUserDict(all, stats), stats
}

// selectWords 按词长挑出候选，并按词频降序排好。
func selectWords(words map[string]*wordEntry, minLen, maxLen int) []*wordEntry {
	rows := make([]*wordEntry, 0, len(words))
	for _, w := range words {
		if n := cjkCount(w.Word); n >= minLen && n <= maxLen {
			rows = append(rows, w)
		}
	}
	sortWords(rows)
	return rows
}

func sortWords(rows []*wordEntry) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Score != rows[j].Score {
			return rows[i].Score > rows[j].Score
		}
		return rows[i].Word < rows[j].Word
	})
}

func writeWords(w io.Writer, rows []*wordEntry, format string) {
	switch format {
	case "txt":
		for _, r := range rows {
			fmt.Fprintln(w, r.Word)
		}
	case "tsv":
		for _, r := range rows {
			fmt.Fprintf(w, "%s\t%s\t%d\n", r.Word, r.Pinyin, r.Score)
		}
	case "json":
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", " ")
		if err := enc.Encode(rows); err != nil {
			fatalf("写 json 失败: %v", err)
		}
	default:
		fatalf("不认识的格式 %q，可选 tsv | txt | json", format)
	}
}

// --------------------------------------------------------------------------

func runWords(args []string) {
	fs := flag.NewFlagSet("words", flag.ExitOnError)
	c := addCommon(fs, "words")
	format := fs.String("format", "tsv", "输出格式: tsv | txt | json")
	minLen := fs.Int("min", 1, "最短词长（汉字数）")
	maxLen := fs.Int("max", 12, "最长词长（汉字数）")
	noHot := fs.Bool("no-hot", false, "不合并热词库")
	length := fs.Bool("length", false, "只输出词的数量")
	fs.BoolVar(length, "count", false, "同 -length")
	parse(fs, args)

	dataDir, _ := c.resolve()
	words, stats := loadWords(dataDir, *noHot)
	rows := selectWords(words, *minLen, *maxLen)
	fmt.Fprintf(os.Stderr, "命中记录：%s\n唯一词条：%d\n", formatStats(stats), len(rows))

	f, w := c.open()
	defer f.Close()
	defer w.Flush()
	if *length {
		fmt.Fprintln(w, len(rows))
		return
	}
	writeWords(w, rows, *format)
}

// --------------------------------------------------------------------------

// matcher 决定一个词条是否命中。
//
// 匹配规则：
//   - 位置参数是词的模式，多个之间是"或"
//   - -pinyin 是拼音的模式，和词的模式之间也是"或"（任一边命中就算）
//   - 默认子串匹配且忽略大小写；-exact 改成整串相等；-re 改成正则
//   - 正则用 Go 的 regexp（RE2 语法）：不支持反向引用和环视，但因此也不会有
//     回溯爆炸的问题。-i 让正则忽略大小写，也可以直接在模式里写 (?i)
type matcher struct {
	wordPats   []string
	pinyinPats []string
	wordRes    []*regexp.Regexp
	pinyinRes  []*regexp.Regexp
	useRe      bool
	exact      bool
	invert     bool
	scoreMin   uint64
	scoreMax   uint64
}

func newMatcher(wordPats, pinyinPats []string, useRe, exact, caseInsensitive, invert bool,
	scoreMin, scoreMax uint64) (*matcher, error) {

	m := &matcher{
		wordPats: wordPats, pinyinPats: pinyinPats,
		useRe: useRe, exact: exact, invert: invert,
		scoreMin: scoreMin, scoreMax: scoreMax,
	}
	if !useRe {
		for i, p := range m.wordPats {
			m.wordPats[i] = strings.ToLower(p)
		}
		for i, p := range m.pinyinPats {
			m.pinyinPats[i] = strings.ToLower(p)
		}
		return m, nil
	}
	prefix := ""
	if caseInsensitive {
		prefix = "(?i)"
	}
	for _, p := range wordPats {
		re, err := regexp.Compile(prefix + p)
		if err != nil {
			return nil, fmt.Errorf("词的正则 %q 编译失败: %v", p, err)
		}
		m.wordRes = append(m.wordRes, re)
	}
	for _, p := range pinyinPats {
		re, err := regexp.Compile(prefix + p)
		if err != nil {
			return nil, fmt.Errorf("拼音的正则 %q 编译失败: %v", p, err)
		}
		m.pinyinRes = append(m.pinyinRes, re)
	}
	return m, nil
}

func (m *matcher) match(e *wordEntry) bool {
	if uint64(e.Score) < m.scoreMin {
		return false
	}
	if m.scoreMax > 0 && uint64(e.Score) > m.scoreMax {
		return false
	}
	// 没给任何模式就只按词频/词长筛
	if len(m.wordPats) == 0 && len(m.pinyinPats) == 0 {
		return !m.invert
	}

	hit := false
	if m.useRe {
		for _, re := range m.wordRes {
			if re.MatchString(e.Word) {
				hit = true
				break
			}
		}
		if !hit {
			for _, re := range m.pinyinRes {
				if re.MatchString(e.Pinyin) {
					hit = true
					break
				}
			}
		}
	} else {
		w := strings.ToLower(e.Word)
		py := strings.ToLower(e.Pinyin)
		for _, p := range m.wordPats {
			if (m.exact && w == p) || (!m.exact && strings.Contains(w, p)) {
				hit = true
				break
			}
		}
		if !hit {
			for _, p := range m.pinyinPats {
				if (m.exact && py == p) || (!m.exact && strings.Contains(py, p)) {
					hit = true
					break
				}
			}
		}
	}
	if m.invert {
		return !hit
	}
	return hit
}

func runGrep(args []string) {
	fs := flag.NewFlagSet("grep", flag.ExitOnError)
	c := addCommon(fs, "words")
	format := fs.String("format", "tsv", "输出格式: tsv | txt | json")
	var pyPats stringList
	fs.Var(&pyPats, "pinyin", "按拼音筛，可以重复给")
	useRe := fs.Bool("re", false, "模式和 -pinyin 都当正则（RE2 语法）")
	exact := fs.Bool("exact", false, "整串精确匹配")
	caseIns := fs.Bool("i", false, "正则忽略大小写")
	invert := fs.Bool("v", false, "反选，输出不匹配的")
	minLen := fs.Int("min", 1, "最短词长（汉字数）")
	maxLen := fs.Int("max", 12, "最长词长（汉字数）")
	scoreMin := fs.Uint64("score-min", 0, "词频下限（含）")
	scoreMax := fs.Uint64("score-max", 0, "词频上限（含），0 表示不限")
	limit := fs.Int("limit", 0, "最多输出 N 条，0 表示不限")
	length := fs.Bool("length", false, "只输出词的数量")
	fs.BoolVar(length, "count", false, "同 -length")
	noHot := fs.Bool("no-hot", false, "不合并热词库")
	parse(fs, args)

	if *useRe && *exact {
		fatalf("-re 和 -exact 不能一起用（正则要整串匹配就写 ^...$）")
	}

	m, err := newMatcher(fs.Args(), pyPats, *useRe, *exact, *caseIns, *invert,
		*scoreMin, *scoreMax)
	if err != nil {
		fatalf("%v", err)
	}

	dataDir, _ := c.resolve()
	words, _ := loadWords(dataDir, *noHot)
	rows := selectWords(words, *minLen, *maxLen)

	hit := make([]*wordEntry, 0, 64)
	for _, r := range rows {
		if m.match(r) {
			hit = append(hit, r)
		}
	}
	fmt.Fprintf(os.Stderr, "候选 %d 条，命中 %d 条\n", len(rows), len(hit))

	f, w := c.open()
	defer f.Close()
	defer w.Flush()
	if *length {
		fmt.Fprintln(w, len(hit))
		return
	}
	if *limit > 0 && len(hit) > *limit {
		hit = hit[:*limit]
	}
	writeWords(w, hit, *format)
}

// --------------------------------------------------------------------------

func runClipboard(args []string) {
	fs := flag.NewFlagSet("clipboard", flag.ExitOnError)
	c := addCommon(fs, "clipboard")
	format := fs.String("format", "json", "输出格式: json | tsv | txt")
	table := fs.String("table", "", "表名，默认挑名字含 clipboard 的表")
	colsFlag := fs.String("cols", "", "tsv 输出哪些列，逗号分隔")
	limit := fs.Int("limit", 0, "只输出最新的 N 条，0 表示不限")
	length := fs.Bool("length", false, "只输出条数")
	fs.BoolVar(length, "count", false, "同 -length")
	parse(fs, args)

	dataDir, _ := c.resolve()
	files := findSQLiteFiles(dataDir, 4)
	if len(files) == 0 {
		fatalf("在 %s 下没找到 SQLite 数据库", dataDir)
	}

	// 按"有没有剪贴板表"来挑库，不靠文件名
	var data *clipboardData
	var tried []string
	for _, p := range files {
		d, err := loadClipboard(p, *table)
		if err != nil {
			tried = append(tried, fmt.Sprintf("  %s: %v", p, err))
			continue
		}
		data = d
		break
	}
	if data == nil {
		fatalf("这些库里都没有剪贴板表：\n%s", strings.Join(tried, "\n"))
	}
	fmt.Fprintf(os.Stderr, "库: %s\n表: %s\n列: %s\n共 %d 条\n",
		data.DBPath, data.Table, strings.Join(data.Columns, ", "), len(data.Rows))

	f, w := c.open()
	defer f.Close()
	defer w.Flush()
	if *length {
		fmt.Fprintln(w, len(data.Rows))
		return
	}
	if *limit > 0 && len(data.Rows) > *limit {
		data.Rows = data.Rows[:*limit]
	}

	switch *format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", " ")
		if err := enc.Encode(data.Rows); err != nil {
			fatalf("写 json 失败: %v", err)
		}
	case "tsv":
		cols := data.defaultTSVColumns()
		if *colsFlag != "" {
			var err error
			if cols, err = data.pick(strings.Split(*colsFlag, ",")); err != nil {
				fatalf("%v", err)
			}
		}
		fmt.Fprintln(w, strings.Join(cols, "\t"))
		for _, r := range data.Rows {
			vals := make([]string, len(cols))
			for i, col := range cols {
				vals[i] = strings.ReplaceAll(valueString(r[col]), "\t", " ")
			}
			fmt.Fprintln(w, strings.Join(vals, "\t"))
		}
	case "txt":
		col := data.contentColumn()
		if col == "" {
			fatalf("没找到正文字段，用 -format tsv -cols 指定")
		}
		for _, r := range data.Rows {
			fmt.Fprintln(w, valueString(r[col]))
		}
	default:
		fatalf("不认识的格式 %q", *format)
	}
}

// --------------------------------------------------------------------------

func runList(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	c := addCommon(fs, "list")
	parse(fs, args)

	dataDir, pkg := c.resolve()
	fmt.Printf("包名:     %s\n数据目录: %s\n\n", pkg, dataDir)

	fmt.Println("LevelDB:")
	for _, d := range findLevelDBDirs(dataDir, 6) {
		ents, err := dbEntries(d)
		if err != nil {
			fmt.Printf("  %s\n    解析失败: %v\n", d, err)
			continue
		}
		n := countUserDictKeys(ents)
		note := ""
		if n > 0 {
			note = fmt.Sprintf("  ← 个人词库，词库条目 %d", n)
		}
		fmt.Printf("  %s\n    %d 条记录%s\n", d, len(ents), note)
	}

	fmt.Println("\nSQLite:")
	for _, p := range findSQLiteFiles(dataDir, 4) {
		db, err := openSQLite(p)
		if err != nil {
			fmt.Printf("  %s\n    打开失败: %v\n", p, err)
			continue
		}
		tables, _ := db.tables()
		var names []string
		for _, t := range tables {
			mark := ""
			if strings.Contains(strings.ToLower(t.Name), "clipboard") {
				mark = " ← 剪贴板"
			}
			names = append(names, t.Name+mark)
		}
		fmt.Printf("  %s\n    page=%d 编码=%d 表: %s\n",
			p, db.pageSize, db.encoding, strings.Join(names, ", "))
	}
}

// --------------------------------------------------------------------------

func countUserDictKeys(ents []kv) int {
	n := 0
	for _, e := range ents {
		for _, p := range userDictPrefixes {
			if len(e.key) >= len(p) && string(e.key[:len(p)]) == p {
				n++
				break
			}
		}
	}
	return n
}

func formatStats(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+strconv.Itoa(m[k]))
	}
	return strings.Join(parts, " ")
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "错误: "+format+"\n", a...)
	os.Exit(1)
}
