package main

import (
	"fmt"
	"sort"
	"strings"
)

// 剪贴板导出。表名和列名都不写死：
// 表按"名字里含 clipboard"来找，列直接用建表语句里的列名，所以换成别的输入法
// 或者官方改了 schema 也能用。

// clipboardData 是一次导出结果。
type clipboardData struct {
	DBPath  string
	Table   string
	Columns []string
	Rows    []map[string]any
}

// pickClipboardTable 在库里挑出剪贴板表。
// want 非空时按名字精确匹配，否则挑名字含 "clipboard" 的第一张。
func pickClipboardTable(db *sqliteDB, want string) (sqliteTable, error) {
	tables, err := db.tables()
	if err != nil {
		return sqliteTable{}, err
	}
	if want != "" {
		for _, t := range tables {
			if t.Name == want {
				return t, nil
			}
		}
		var names []string
		for _, t := range tables {
			names = append(names, t.Name)
		}
		return sqliteTable{}, fmt.Errorf("库里没有表 %q，有的表：%s",
			want, strings.Join(names, ", "))
	}
	var names []string
	for _, t := range tables {
		names = append(names, t.Name)
		if strings.Contains(strings.ToLower(t.Name), "clipboard") {
			return t, nil
		}
	}
	return sqliteTable{}, fmt.Errorf("没找到剪贴板表（名字里含 clipboard 的表），库里有的表：%s",
		strings.Join(names, ", "))
}

// loadClipboard 读出一张表的所有行，转成 列名 -> 值 的形式。
func loadClipboard(path, table string) (*clipboardData, error) {
	db, err := openSQLite(path)
	if err != nil {
		return nil, err
	}
	t, err := pickClipboardTable(db, table)
	if err != nil {
		return nil, err
	}
	cols, raw, err := db.rows(t)
	if err != nil {
		return nil, err
	}
	out := &clipboardData{DBPath: path, Table: t.Name, Columns: cols}
	for _, r := range raw {
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			if i < len(r) {
				row[c] = r[i]
			}
		}
		out.Rows = append(out.Rows, row)
	}
	out.sortNewestFirst()
	return out, nil
}

// sortNewestFirst 按时间倒序排。优先 createTime，退到 timestamp，再退到 id。
func (d *clipboardData) sortNewestFirst() {
	key := ""
	for _, k := range []string{"createTime", "timestamp", "time", "id"} {
		for _, c := range d.Columns {
			if strings.EqualFold(c, k) {
				key = c
				break
			}
		}
		if key != "" {
			break
		}
	}
	if key == "" {
		return
	}
	sort.SliceStable(d.Rows, func(i, j int) bool {
		return asInt(d.Rows[i][key]) > asInt(d.Rows[j][key])
	})
}

func asInt(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	}
	return 0
}

// contentColumn 猜哪一列是剪贴板正文。
func (d *clipboardData) contentColumn() string {
	for _, want := range []string{"content", "text", "clipboard_text", "value"} {
		for _, c := range d.Columns {
			if strings.EqualFold(c, want) {
				return c
			}
		}
	}
	return ""
}

// defaultTSVColumns 选一个适合肉眼看/当词表用的列集合。
// 有 createTime 和 content 就只给这两列，否则给全部。
func (d *clipboardData) defaultTSVColumns() []string {
	var timeCol string
	for _, c := range d.Columns {
		if strings.EqualFold(c, "createTime") || strings.EqualFold(c, "timestamp") {
			timeCol = c
			break
		}
	}
	if c := d.contentColumn(); c != "" && timeCol != "" {
		return []string{timeCol, c}
	}
	return d.Columns
}

// pick 按名字挑列，保持用户给的顺序；名字不存在就报错。
func (d *clipboardData) pick(names []string) ([]string, error) {
	var out []string
	for _, n := range names {
		hit := ""
		for _, c := range d.Columns {
			if strings.EqualFold(c, n) {
				hit = c
				break
			}
		}
		if hit == "" {
			return nil, fmt.Errorf("没有列 %q，有的列：%s", n, strings.Join(d.Columns, ", "))
		}
		out = append(out, hit)
	}
	return out, nil
}

// valueString 把单元格转成适合输出的字符串。
func valueString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}
